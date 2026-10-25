package egress

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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
// event stream (parsed event by event). It understands the Anthropic
// Messages API ("input_tokens", "output_tokens", cache tokens) and the
// OpenAI API ("prompt_tokens", "completion_tokens"). Counts are
// cumulative within a response, so the largest seen wins.
type usageTap struct {
	sse bool

	mu      sync.Mutex
	buf     bytes.Buffer // whole JSON body, or the current SSE line
	tooBig  bool
	in, out int64
}

func newUsageTap(contentType string) *usageTap {
	return &usageTap{sse: strings.HasPrefix(strings.ToLower(contentType), "text/event-stream")}
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
}

// finish returns the response's token counts.
func (t *usageTap) finish() (in, out int64) {
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
	return t.in, t.out
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
	for _, u := range []*usageFields{v.Usage, messageUsage(v.Message)} {
		if u == nil {
			continue
		}
		in := u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens + u.PromptTokens
		out := u.OutputTokens + u.CompletionTokens
		t.in, t.out = max(t.in, in), max(t.out, out)
	}
}

func messageUsage(m *struct {
	Usage *usageFields `json:"usage"`
}) *usageFields {
	if m == nil {
		return nil
	}
	return m.Usage
}

// meteredBody records a response's usage once it has been read to the end
// or closed.
type meteredBody struct {
	io.ReadCloser
	tap    *usageTap
	once   sync.Once
	record func(in, out int64)
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
	b.once.Do(func() { b.record(b.tap.finish()) })
}

// meteredTransport enforces and records a job's budget for brokered
// requests: once it is exhausted, requests are answered 429 without
// reaching the upstream; every response's usage is recorded.
type meteredTransport struct {
	base  http.RoundTripper
	meter *budget.Meter
}

func (t *meteredTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.meter == nil {
		return t.base.RoundTrip(req)
	}
	if err := t.meter.Allow(); err != nil {
		body := err.Error() + "\n"
		return &http.Response{
			StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests",
			Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header:        http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       req,
		}, nil
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &meteredBody{ReadCloser: resp.Body, tap: newUsageTap(resp.Header.Get("Content-Type")), record: t.meter.Record}
	return resp, nil
}
