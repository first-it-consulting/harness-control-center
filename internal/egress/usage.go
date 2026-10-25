package egress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
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
// increments. A response's usage is final once a complete JSON body or a
// stream's closing usage (Anthropic's message_delta, OpenAI's usage
// chunk) was read; message_start only carries a first estimate.
type usageTap struct {
	sse    bool
	record func(in, out int64)

	mu           sync.Mutex
	buf          bytes.Buffer // whole JSON body, or the current SSE line
	tooBig       bool
	seen, final  bool
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

// finish records what remains and returns the counts and whether usage
// was seen at all, and in final form.
func (t *usageTap) finish() (in, out int64, seen, final bool) {
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
	return t.in, t.out, t.seen, t.final
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
		Type    string       `json:"type"`
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
		if !t.sse || v.Type != "message_start" {
			t.final = true
		}
		in := u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.PromptTokens
		out := u.OutputTokens + u.CompletionTokens
		t.in, t.out = max(t.in, in), max(t.out, out)
	}
}

// meteredBody records a response's usage as it is read and calls onEnd
// once, when the body is done (read to the end, or closed early).
type meteredBody struct {
	io.ReadCloser
	tap   *usageTap
	once  sync.Once
	onEnd func(in, out int64, seen, final bool)
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
		in, out, seen, final := b.tap.finish()
		if b.onEnd != nil {
			b.onEnd(in, out, seen, final)
		}
	})
}

// freePaths are the POST calls on a model endpoint that use no tokens.
// Every other POST is metered and must report usage (deny by default).
var freePaths = []string{"/messages/count_tokens"}

// Bounds on what the proxy parses in a model request. Larger bodies pass
// through unparsed: their response must then report usage by itself.
const (
	maxParsedRequest  = 8 << 20
	maxRequestOptions = 64 << 10
)

// cleanPath reports whether p is in canonical form, so that the path
// classified here is the path the upstream serves.
func cleanPath(u *http.Request) bool {
	p := u.URL.Path
	if u.URL.RawPath != "" || !strings.HasPrefix(p, "/") {
		return false
	}
	c := path.Clean(p)
	return c == p || c+"/" == p
}

func isFree(req *http.Request) bool {
	if req.Method == http.MethodGet || req.Method == http.MethodHead {
		return true
	}
	p := strings.TrimSuffix(req.URL.Path, "/")
	for _, f := range freePaths {
		if strings.HasSuffix(p, f) {
			return true
		}
	}
	return false
}

// meteredTransport enforces and records a job's budget for brokered
// requests. Fail closed: only canonical paths are forwarded, responses are
// requested uncompressed (a compressed response could not be counted), at
// most budget.MaxInFlight requests run at once, OpenAI-style streams are
// asked to report usage, and every metered response must report its
// final usage. One that ends without it (e.g. a stream closed early) is
// charged its request's max_tokens; without that bound, the budget stops.
type meteredTransport struct {
	base  http.RoundTripper
	meter *budget.Meter
}

func (t *meteredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.meter == nil {
		return t.base.RoundTrip(req)
	}
	if !cleanPath(req) {
		return plainResponse(req, http.StatusBadRequest, "model request path must be canonical"), nil
	}
	release, err := t.meter.Begin()
	if err != nil {
		return plainResponse(req, http.StatusTooManyRequests, err.Error()), nil
	}
	req = req.Clone(req.Context())
	req.Header.Set("Accept-Encoding", "identity")
	metered := !isFree(req)
	maxOut := int64(-1)
	if metered {
		if maxOut, err = prepareRequest(req); err != nil {
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
		if metered {
			t.meter.Fail("model sent a compressed response that cannot be metered")
		}
		return plainResponse(req, http.StatusBadGateway, "compressed responses cannot be metered"), nil
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	resp.Body = &meteredBody{
		ReadCloser: resp.Body,
		tap:        newUsageTap(resp.Header.Get("Content-Type"), t.meter.Record),
		onEnd: func(_, out int64, seen, final bool) {
			release()
			switch {
			case !metered || !ok || final:
			case seen && maxOut > 0:
				// Cut short: the model may have generated up to max_tokens.
				t.meter.Record(0, max(0, maxOut-out))
			default:
				t.meter.Fail("model reported no usage for " + req.URL.Path)
			}
		},
	}
	return resp, nil
}

// prepareRequest reads a metered request's max_tokens and makes an
// OpenAI-style streaming request report usage ("stream_options":
// {"include_usage": true}); Anthropic streams always do. Only the top
// level of the JSON body is decoded (values stay raw bytes), and bodies
// over maxParsedRequest are passed through unparsed (max_tokens -1).
func prepareRequest(req *http.Request) (maxTokens int64, err error) {
	if req.Body == nil || req.Body == http.NoBody {
		return -1, nil
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, maxParsedRequest+1))
	if err != nil {
		_ = req.Body.Close()
		return -1, err
	}
	if len(raw) > maxParsedRequest {
		req.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(raw), req.Body), req.Body}
		return -1, nil
	}
	_ = req.Body.Close()
	maxTokens = -1
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) == nil {
		for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			var n int64
			if v, ok := top[k]; ok && json.Unmarshal(v, &n) == nil && n > 0 {
				maxTokens = n
				break
			}
		}
		if openAIStyle(req.URL.Path) && string(bytes.TrimSpace(top["stream"])) == "true" {
			opts := map[string]json.RawMessage{}
			if v, ok := top["stream_options"]; ok && len(v) <= maxRequestOptions {
				_ = json.Unmarshal(v, &opts)
			}
			opts["include_usage"] = json.RawMessage("true")
			if o, err := json.Marshal(opts); err == nil {
				top["stream_options"] = o
				if rewritten, err := json.Marshal(top); err == nil {
					raw = rewritten
				}
			}
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	req.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	return maxTokens, nil
}

func openAIStyle(p string) bool {
	p = strings.TrimSuffix(p, "/")
	return strings.HasSuffix(p, "/chat/completions") || strings.HasSuffix(p, "/completions")
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
