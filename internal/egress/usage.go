package egress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
)

// Bounds on what the usage tap keeps in memory; responses come from
// outside, so nothing here may grow without limit.
const (
	maxUsageJSON = 8 << 20
	maxSSELine   = 1 << 20
)

// usageTap reads token usage from a model API response while it passes
// through unchanged: a JSON body (parsed at the end) or a server-sent
// event stream (parsed event by event, recorded as it arrives, so a long
// stream is cut off when it crosses the budget). It understands the
// Anthropic Messages API ("input_tokens", "output_tokens", cache tokens)
// and the OpenAI API ("prompt_tokens", "completion_tokens"). Counts are
// cumulative within a response, so the largest seen wins; record gets the
// increments.
type usageTap struct {
	sse    bool
	record func(in, out int64)

	mu           sync.Mutex
	buf          bytes.Buffer // whole JSON body, or the current SSE line
	tooBig       bool
	seen         bool
	in, out      int64
	recIn, recOt int64
}

func newUsageTap(contentType string, record func(in, out int64)) *usageTap {
	return &usageTap{sse: strings.HasPrefix(strings.ToLower(contentType), "text/event-stream"), record: record}
}

func (t *usageTap) Write(p []byte) (int, error) {
	n := len(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.sse {
		if !t.tooBig {
			if t.buf.Len()+len(p) > maxUsageJSON {
				t.tooBig = true
				t.buf.Reset()
			} else {
				t.buf.Write(p)
			}
		}
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if t.buf.Len()+len(chunk) <= maxSSELine {
			t.buf.Write(chunk)
		}
		if i < 0 {
			break
		}
		t.sseLine(t.buf.Bytes())
		t.buf.Reset()
		p = p[i+1:]
	}
	return n, nil
}

func (t *usageTap) sseLine(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok {
		return
	}
	t.observe(bytes.TrimSpace(data))
	t.flush()
}

// flush records what was counted since the last flush.
func (t *usageTap) flush() {
	din, dout := t.in-t.recIn, t.out-t.recOt
	if (din > 0 || dout > 0) && t.record != nil {
		t.recIn, t.recOt = t.in, t.out
		t.mu.Unlock()
		t.record(din, dout)
		t.mu.Lock()
	}
}

// finish records what remains and reports whether any usage was seen.
func (t *usageTap) finish() (in, out int64, seen bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sse {
		if t.buf.Len() > 0 {
			t.sseLine(t.buf.Bytes())
			t.buf.Reset()
		}
	} else if !t.tooBig {
		t.observe(t.buf.Bytes())
	}
	t.flush()
	return t.in, t.out, t.seen
}

type usageFields struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
}

func (t *usageTap) observe(doc []byte) {
	var v struct {
		Usage   *usageFields `json:"usage"`
		Message *struct {
			Usage *usageFields `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(doc, &v) != nil {
		return
	}
	var mu *usageFields
	if v.Message != nil {
		mu = v.Message.Usage
	}
	for _, u := range []*usageFields{v.Usage, mu} {
		if u == nil {
			continue
		}
		t.seen = true
		in := u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.PromptTokens
		out := u.OutputTokens + u.CompletionTokens
		t.in, t.out = max(t.in, in), max(t.out, out)
	}
}

// meteredBody records a response's usage as it is read and, at the end,
// fails the budget if a generation response reported none.
type meteredBody struct {
	io.ReadCloser
	tap  *usageTap
	once sync.Once
	// onEnd gets whether usage was seen.
	onEnd func(seen bool)
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		_, _ = b.tap.Write(p[:n])
	}
	if err == io.EOF {
		b.done()
	}
	return n, err
}

func (b *meteredBody) Close() error {
	b.done()
	return b.ReadCloser.Close()
}

func (b *meteredBody) done() {
	b.once.Do(func() {
		_, _, seen := b.tap.finish()
		if b.onEnd != nil {
			b.onEnd(seen)
		}
	})
}

// generationPaths are the API calls that consume tokens; their successful
// responses must report usage.
var generationPaths = []string{"/messages", "/chat/completions", "/completions", "/responses"}

func isGeneration(req *http.Request) bool {
	if req.Method != http.MethodPost {
		return false
	}
	p := strings.TrimSuffix(req.URL.Path, "/")
	for _, g := range generationPaths {
		if strings.HasSuffix(p, g) {
			return true
		}
	}
	return false
}

// maxModelRequestBody bounds the request bodies the proxy rewrites.
const maxModelRequestBody = 64 << 20

// meteredTransport enforces and records a job's budget for brokered
// requests. Fail closed: responses are requested uncompressed (a
// compressed response could not be counted), at most budget.MaxInFlight
// requests run at once, OpenAI-style streams are asked to report usage,
// and a successful generation response without readable usage stops the
// budget.
type meteredTransport struct {
	base  http.RoundTripper
	meter *budget.Meter
}

func (t *meteredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.meter == nil {
		return t.base.RoundTrip(req)
	}
	release, err := t.meter.Begin()
	if err != nil {
		return plainResponse(req, http.StatusTooManyRequests, err.Error()), nil
	}
	req = req.Clone(req.Context())
	req.Header.Set("Accept-Encoding", "identity")
	gen := isGeneration(req)
	if gen {
		if err := requestUsage(req); err != nil {
			release()
			return plainResponse(req, http.StatusBadRequest, err.Error()), nil
		}
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		release()
		return nil, err
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		_ = resp.Body.Close()
		release()
		if gen {
			t.meter.Fail("model sent a compressed response that cannot be metered")
		}
		return plainResponse(req, http.StatusBadGateway, "compressed responses cannot be metered"), nil
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	resp.Body = &meteredBody{
		ReadCloser: resp.Body,
		tap:        newUsageTap(resp.Header.Get("Content-Type"), t.meter.Record),
		onEnd: func(seen bool) {
			release()
			if gen && ok && !seen {
				t.meter.Fail("model reported no usage for " + req.URL.Path)
			}
		},
	}
	return resp, nil
}

// requestUsage makes an OpenAI-style streaming request report usage
// ("stream_options": {"include_usage": true}); Anthropic streams always
// do. Non-JSON bodies are left alone (the response is then checked).
func requestUsage(req *http.Request) error {
	if req.Body == nil || !strings.Contains(req.URL.Path, "completions") {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxModelRequestBody+1))
	_ = req.Body.Close()
	if err != nil {
		return err
	}
	if len(raw) > maxModelRequestBody {
		return errors.New("model request too large")
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) == nil {
		if stream, _ := body["stream"].(bool); stream {
			opts, _ := body["stream_options"].(map[string]any)
			if opts == nil {
				opts = map[string]any{}
			}
			opts["include_usage"] = true
			body["stream_options"] = opts
			if rewritten, err := json.Marshal(body); err == nil {
				raw = rewritten
			}
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	req.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return nil
}

func plainResponse(req *http.Request, code int, msg string) *http.Response {
	body := msg + "\n"
	return &http.Response{
		StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
