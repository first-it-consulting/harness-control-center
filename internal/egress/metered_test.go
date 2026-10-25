package egress

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(code int, ct, ce, body string) *http.Response {
	h := http.Header{"Content-Type": {ct}}
	if ce != "" {
		h.Set("Content-Encoding", ce)
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// post sends a request and drains the response, returning its status.
func post(t *testing.T, tr http.RoundTripper, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://model.example"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept-Encoding", "gzip, br")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestMeteredForcesIdentityAndRejectsCompressed(t *testing.T) {
	m := budget.New(budget.Limits{MaxTokens: 1000}, nil, nil)
	var sawEncoding string
	tr := &meteredTransport{meter: m, base: rtFunc(func(r *http.Request) (*http.Response, error) {
		sawEncoding = r.Header.Get("Accept-Encoding")
		return respond(200, "application/json", "gzip", "\x1f\x8b..."), nil
	})}
	code := post(t, tr, "/v1/messages", `{}`)
	if sawEncoding != "identity" {
		t.Errorf("upstream saw Accept-Encoding %q", sawEncoding)
	}
	if code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", code)
	}
	if m.Exceeded() == "" {
		t.Error("compressed generation response did not stop the budget")
	}
}

func TestMeteredNoUsageFailsClosed(t *testing.T) {
	m := budget.New(budget.Limits{MaxTokens: 1000}, nil, nil)
	tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
		return respond(200, "text/event-stream", "", "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"), nil
	})}
	post(t, tr, "/v1/chat/completions", `{"stream":true}`)
	if m.Exceeded() == "" {
		t.Fatal("response without usage did not stop the budget")
	}
	if code := post(t, tr, "/v1/chat/completions", `{}`); code != http.StatusTooManyRequests {
		t.Errorf("next request status = %d, want 429", code)
	}
}

func TestMeteredNonGenerationAndErrorsNeedNoUsage(t *testing.T) {
	m := budget.New(budget.Limits{MaxTokens: 1000}, nil, nil)
	code := 200
	tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
		return respond(code, "application/json", "", `{"data":[]}`), nil
	})}
	req, _ := http.NewRequest(http.MethodGet, "https://model.example/v1/models", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	code = 500
	post(t, tr, "/v1/messages", `{}`)
	if m.Exceeded() != "" {
		t.Errorf("budget stopped: %s", m.Exceeded())
	}
}

func TestMeteredInjectsIncludeUsage(t *testing.T) {
	m := budget.New(budget.Limits{}, nil, nil)
	var got map[string]any
	tr := &meteredTransport{meter: m, base: rtFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if int64(len(raw)) != r.ContentLength {
			t.Errorf("content length %d for %d bytes", r.ContentLength, len(raw))
		}
		_ = json.Unmarshal(raw, &got)
		return respond(200, "application/json", "", `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil
	})}
	post(t, tr, "/v1/chat/completions", `{"model":"x","stream":true,"stream_options":{"foo":1}}`)
	opts, _ := got["stream_options"].(map[string]any)
	if opts["include_usage"] != true || opts["foo"] == nil || got["model"] != "x" {
		t.Errorf("rewritten body = %v", got)
	}
}

func TestMeteredInFlightCap(t *testing.T) {
	m := budget.New(budget.Limits{}, nil, nil)
	block := make(chan struct{})
	tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(io.MultiReader(blockingReader(block), strings.NewReader(`{"usage":{"input_tokens":1,"output_tokens":1}}`)))}, nil
	})}
	var open []io.ReadCloser
	for range budget.MaxInFlight {
		req, _ := http.NewRequest(http.MethodPost, "https://model.example/v1/messages", strings.NewReader(`{}`))
		resp, err := tr.RoundTrip(req) //nolint:bodyclose // the bodies are closed after the cap is checked
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("request: %v %v", resp, err)
		}
		open = append(open, resp.Body)
	}
	if code := post(t, tr, "/v1/messages", `{}`); code != http.StatusTooManyRequests {
		t.Errorf("request over the cap: status %d, want 429", code)
	}
	close(block)
	for _, r := range open {
		_, _ = io.ReadAll(r)
		_ = r.Close()
	}
	if code := post(t, tr, "/v1/messages", `{}`); code == http.StatusTooManyRequests {
		t.Error("slots not released")
	}
}

type blockingReader chan struct{}

func (b blockingReader) Read([]byte) (int, error) {
	<-b
	return 0, io.EOF
}

const messageStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n"

// A stream that ends before its final usage is charged its max_tokens.
func TestMeteredCutStreamChargedMaxTokens(t *testing.T) {
	m := budget.New(budget.Limits{MaxTokens: 1_000_000}, nil, nil)
	tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
		return respond(200, "text/event-stream", "", messageStart), nil
	})}
	post(t, tr, "/v1/messages", `{"max_tokens":5000,"stream":true}`)
	if u := m.Usage(); u.InputTokens != 100 || u.OutputTokens != 5000 || m.Exceeded() != "" {
		t.Errorf("usage %+v exceeded %q, want 100/5000 charged", u, m.Exceeded())
	}
	// Without max_tokens there is no bound: the budget stops.
	post(t, tr, "/v1/messages", `{"stream":true}`)
	if m.Exceeded() == "" {
		t.Error("cut stream without max_tokens did not stop the budget")
	}
}

func TestMeteredDenyByDefault(t *testing.T) {
	m := budget.New(budget.Limits{}, nil, nil)
	tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
		return respond(200, "application/json", "", `{"input_tokens":12}`), nil
	})}
	post(t, tr, "/v1/messages/count_tokens", `{}`)
	if m.Exceeded() != "" {
		t.Fatalf("count_tokens stopped the budget: %s", m.Exceeded())
	}
	post(t, tr, "/v1/some/new/endpoint", `{}`)
	if m.Exceeded() == "" {
		t.Error("an unknown POST without usage did not stop the budget")
	}
}

func TestMeteredRejectsNonCanonicalPaths(t *testing.T) {
	m := budget.New(budget.Limits{}, nil, nil)
	called := false
	tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return respond(200, "application/json", "", `{}`), nil
	})}
	for _, p := range []string{"/v1/messages/count_tokens/../../messages", "/v1//messages", "/v1/./messages", "/v1/messages%2Fcount_tokens"} {
		req, err := http.NewRequest(http.MethodPost, "https://model.example"+p, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", p, resp.StatusCode)
		}
	}
	if called {
		t.Error("a non-canonical path reached the upstream")
	}
}

// Large bodies are forwarded intact without being parsed.
func TestMeteredLargeBodyPassesThrough(t *testing.T) {
	m := budget.New(budget.Limits{}, nil, nil)
	body := `{"stream":true,"pad":"` + strings.Repeat("x", maxParsedRequest) + `"}`
	var got int
	tr := &meteredTransport{meter: m, base: rtFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		got = len(raw)
		return respond(200, "application/json", "", `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`), nil
	})}
	post(t, tr, "/v1/chat/completions", body)
	if got != len(body) {
		t.Errorf("upstream got %d bytes, want %d", got, len(body))
	}
}

// cutAfter sends a request and closes its response after n bytes.
func cutAfter(t *testing.T, tr http.RoundTripper, body string, n int64) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://model.example/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.CopyN(io.Discard, resp.Body, n)
	_ = resp.Body.Close()
}

// Usage counts as final only at EOF: a body closed early may have been
// generating more than its last usage event said.
func TestMeteredEarlyCloseIsNotFinal(t *testing.T) {
	stream := messageStart + "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":10}}\n\n" + strings.Repeat("data: {}\n\n", 100)
	m := budget.New(budget.Limits{}, nil, nil)
	tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
		return respond(200, "text/event-stream", "", stream), nil
	})}
	cutAfter(t, tr, `{"max_tokens":800}`, int64(len(stream)-50))
	if u := m.Usage(); u.OutputTokens != 800 {
		t.Errorf("output tokens = %d, want max_tokens (800) charged", u.OutputTokens)
	}
}

func TestMeteredChargesTheLargestMaxTokens(t *testing.T) {
	m := budget.New(budget.Limits{}, nil, nil)
	tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
		return respond(200, "text/event-stream", "", messageStart), nil
	})}
	post(t, tr, "/v1/messages", `{"max_tokens":10,"max_completion_tokens":5000}`)
	if u := m.Usage(); u.OutputTokens != 5000 {
		t.Errorf("output tokens = %d, want 5000", u.OutputTokens)
	}
}

func TestMeteredInsaneMaxTokensIsUnbounded(t *testing.T) {
	for _, body := range []string{`{"max_tokens":9223372036854775807}`, `{"max_tokens":100,"max_output_tokens":"lots"}`, `{"max_tokens":-5}`} {
		m := budget.New(budget.Limits{}, nil, nil)
		tr := &meteredTransport{meter: m, base: rtFunc(func(*http.Request) (*http.Response, error) {
			return respond(200, "text/event-stream", "", messageStart), nil
		})}
		post(t, tr, "/v1/messages", body)
		if m.Exceeded() == "" {
			t.Errorf("%s: cut stream did not stop the budget (usage %+v)", body, m.Usage())
		}
	}
}
