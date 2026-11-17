//go:build unix

package control

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestProviderModels(t *testing.T) {
	ts := newTestServer(t)
	const secret = "sk-provider-0123456789"
	ts.rawJSON(http.MethodPut, "/api/credentials/llm", map[string]any{"hosts": []string{"llm.lan"}, "scheme": "http", "port": 8080,
		"header": "Authorization", "prefix": "Bearer ", "secret": secret})
	ts.rawJSON(http.MethodPut, "/api/credentials/elsewhere", map[string]any{"hosts": []string{"other.lan"}, "scheme": "http", "port": 8080,
		"header": "Authorization", "prefix": "Bearer ", "secret": "sk-elsewhere-0123456789"})
	providers := Catalog{Providers: []Provider{
		{Name: "llm", Type: "litellm", URL: "http://llm.lan:8080", Credential: "llm"},
		{Name: "wrong", Type: "litellm", URL: "http://llm.lan:8080", Credential: "elsewhere"},
		{Name: "ollama", Type: "ollama", URL: "http://llm.lan:8080"},
	}}
	if code := ts.do(http.MethodPut, "/api/providers", providers, nil); code != http.StatusOK {
		t.Fatalf("PUT providers: %d", code)
	}

	var mu sync.Mutex
	var auths []string
	mode := "list"
	fakeProvider(t, ts, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		m := mode
		mu.Unlock()
		switch {
		case m == "redirect":
			http.Redirect(w, r, "http://evil.example/collect", http.StatusFound)
		case m == "refuse":
			w.WriteHeader(http.StatusUnauthorized)
		case m == "ollama" && r.URL.Path == "/v1/models":
			http.NotFound(w, r)
		case m == "ollama" && r.URL.Path == "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"},{"name":"llama3.3:70b"}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"id":"zeta"},{"id":"alpha","display_name":"Alpha \u001b[31mmodel"},{"id":"bad id"},{"id":"zeta"}]}`))
		}
	})
	ask := func(name string) (int, map[string]any) {
		var out map[string]any
		code := ts.do(http.MethodPost, "/api/providers/"+name+"/models", nil, &out)
		return code, out
	}
	code, out := ask("llm")
	if code != http.StatusOK {
		t.Fatalf("models: %d %v", code, out)
	}
	models := out["models"].([]any)
	if len(models) != 2 || models[0].(map[string]any)["id"] != "alpha" || strings.ContainsRune(models[0].(map[string]any)["label"].(string), 0x1b) {
		t.Errorf("models = %v", models)
	}
	if auths[len(auths)-1] != "Bearer "+secret {
		t.Errorf("auth = %q", auths[len(auths)-1])
	}
	// A credential for another host is never sent.
	if code, out := ask("wrong"); code != http.StatusBadGateway || !strings.Contains(out["error"].(string), "not for http://llm.lan:8080") {
		t.Errorf("wrong credential: %d %v", code, out)
	}
	for _, a := range auths {
		if strings.Contains(a, "elsewhere") {
			t.Error("the other host's credential was sent")
		}
	}
	// Ollama's own list when /v1/models is missing.
	mode = "ollama"
	if code, out := ask("ollama"); code != http.StatusOK || len(out["models"].([]any)) != 2 {
		t.Errorf("ollama: %d %v", code, out)
	}
	// A redirect is not followed; a refusal says so.
	mode = "redirect"
	if code, out := ask("llm"); code != http.StatusBadGateway || !strings.Contains(out["error"].(string), "302") {
		t.Errorf("redirect: %d %v", code, out)
	}
	mode = "refuse"
	if code, out := ask("llm"); code != http.StatusBadGateway || !strings.Contains(out["error"].(string), "refused the credential") {
		t.Errorf("refused: %d %v", code, out)
	}
	if code, _ := ask("nope"); code != http.StatusNotFound {
		t.Errorf("unknown provider: %d", code)
	}
	// No answer ever carries the secret.
	for _, mode = range []string{"list", "refuse", "redirect"} {
		for _, name := range []string{"llm", "wrong"} {
			if rec := ts.rawJSON(http.MethodPost, "/api/providers/"+name+"/models", nil); strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("%s/%s: the secret is in the answer: %s", mode, name, rec.Body.String())
			}
		}
	}
}

func TestProviderWithoutCredentialSaysSo(t *testing.T) {
	ts := newTestServer(t)
	ts.do(http.MethodPut, "/api/providers", Catalog{Providers: []Provider{{Name: "open", Type: "omlx", URL: "http://llm.lan:8080"}}}, nil)
	fakeProvider(t, ts, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/providers/open/models", nil, &out); code != http.StatusBadGateway || !strings.Contains(out["error"].(string), "wants a credential") {
		t.Errorf("%d %v", code, out)
	}
}

// A connection is checked before it is saved, with the key typed for it.
func TestProviderTestBeforeSaving(t *testing.T) {
	ts := newTestServer(t)
	var got string
	fakeProvider(t, ts, func(w http.ResponseWriter, r *http.Request) {
		got = r.Host + " " + r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:8b"}]}`))
	})
	body := map[string]any{"type": "litellm", "url": "http://llm.lan:8080", "local": true,
		"auth": map[string]string{"header": "Authorization", "prefix": "Bearer ", "key": "sk-typed-123"}}
	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/providers/test", body, &out); code != http.StatusOK || len(out["models"].([]any)) != 1 {
		t.Fatalf("test: %d %v", code, out)
	}
	if got != "llm.lan:8080 Bearer sk-typed-123" {
		t.Errorf("sent %q", got)
	}
	for name, b := range map[string]map[string]any{
		"ip address":   {"type": "other", "url": "http://10.0.0.1:8080"},
		"bad header":   {"type": "other", "url": "https://a.example", "auth": map[string]string{"header": "X: y", "key": "k"}},
		"key newline":  {"type": "other", "url": "https://a.example", "auth": map[string]string{"header": "X-Key", "key": "k\r\nX-Evil: 1"}},
		"unknown type": {"type": "gpt", "url": "https://a.example"},
	} {
		if code := ts.do(http.MethodPost, "/api/providers/test", b, &out); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if rec := ts.rawJSON(http.MethodPost, "/api/providers/test", body); strings.Contains(rec.Body.String(), "sk-typed-123") {
		t.Error("the key is in the answer")
	}
}
