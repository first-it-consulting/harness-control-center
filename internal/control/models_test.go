package control

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func goodProviders() Catalog {
	return Catalog{Providers: []Provider{
		{Name: "anthropic", Type: "anthropic", URL: "https://api.anthropic.com", Credential: "anthropic"},
		{Name: "studio", Type: "omlx", URL: "http://mac-studio.lan:4000", Credential: "omlx"},
		{Name: "ollama", Type: "ollama", URL: "http://ollama.example.com:11434", Local: true},
	}}
}

func TestProvidersValidate(t *testing.T) {
	c := goodProviders()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.Providers[1].Private() || !c.Providers[2].Private() || c.Providers[0].Private() {
		t.Error("Private()")
	}
	for name, tt := range map[string]struct {
		change func(*Catalog)
		want   string
	}{
		"internal, not local": {func(c *Catalog) { c.Providers[0].URL = "https://llm.internal" }, ""},
		"ip address":          {func(c *Catalog) { c.Providers[1].URL = "http://10.0.0.5:4000" }, "IP addresses"},
		"secret in the url":   {func(c *Catalog) { c.Providers[0].URL = "https://key:sk@api.anthropic.com" }, "credentials"},
		"bad name":            {func(c *Catalog) { c.Providers[0].Name = "Anthropic API" }, "provider name"},
		"duplicate":           {func(c *Catalog) { c.Providers[1].Name = "anthropic" }, "another provider"},
		"unknown type":        {func(c *Catalog) { c.Providers[0].Type = "gpt" }, "unknown type"},
		"bad credential":      {func(c *Catalog) { c.Providers[0].Credential = "../x" }, "credential name"},
	} {
		c := goodProviders()
		tt.change(&c)
		err := c.Validate()
		if tt.want == "" {
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

// The first format (endpoints and models) is read as providers.
func TestLoadCatalogFirstFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.yaml")
	old := "endpoints:\n  - {name: anthropic, kind: anthropic, url: 'https://api.anthropic.com', credential: anthropic}\n" +
		"  - {name: studio, kind: network, url: 'http://mac-studio.lan:4000'}\nmodels:\n  - {id: m, endpoint: studio}\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Providers) != 2 || c.Providers[0].Type != "anthropic" || c.Providers[1].Type != "other" || c.Providers[1].URL != "http://mac-studio.lan:4000" {
		t.Errorf("providers = %+v", c.Providers)
	}
}

func TestProvidersThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	var out struct{ Providers []ProviderView }
	if code := ts.do(http.MethodGet, "/api/providers", nil, &out); code != http.StatusOK || len(out.Providers) != 0 {
		t.Fatalf("empty: %d %+v", code, out)
	}
	used := strings.Replace(okHarness, "  prompt: hi\n", `  prompt: hi
  model: {name: Ornith, endpoint: 'http://mac-studio.lan:4000'}
  permissions:
    network: {allowPrivateNetworks: true, egress: [{host: mac-studio.lan, ports: [4000]}]}
`, 1)
	if code := ts.do(http.MethodPut, "/api/harnesses", map[string]string{"yaml": used}, nil); code != http.StatusOK {
		t.Fatalf("save harness: %d", code)
	}
	if code := ts.do(http.MethodPut, "/api/providers", goodProviders(), &out); code != http.StatusOK {
		t.Fatalf("PUT: %d", code)
	}
	if len(out.Providers) != 3 || !out.Providers[1].Private || len(out.Providers[1].UsedBy) != 1 || out.Providers[1].UsedBy[0] != "hello" {
		t.Errorf("after save: %+v", out.Providers)
	}
	data, err := os.ReadFile(filepath.Join(ts.dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(ts.dir, "models.yaml")); runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("models.yaml mode %v", fi.Mode())
	}
	bad := goodProviders()
	bad.Providers[0].URL = "http://10.0.0.1"
	bad.Providers[1].Type = "gpt"
	var res map[string]any
	if code := ts.do(http.MethodPut, "/api/providers", bad, &res); code != http.StatusBadRequest || len(res["errors"].([]any)) != 2 {
		t.Errorf("invalid: %d %v", code, res)
	}
	if after, _ := os.ReadFile(filepath.Join(ts.dir, "models.yaml")); string(after) != string(data) {
		t.Error("a refused change wrote the file")
	}
}

// fakeProvider serves a model list as llm.example (the Control Center's
// client dials it there), recording the requests' auth headers.
func fakeProvider(t *testing.T, ts *testServer, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	c := newProviderClient()
	c.Transport = tr
	ts.s.providerClient = c
}

func TestUnreachableSaysWhy(t *testing.T) {
	ts := newTestServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens there now
	c := newProviderClient()
	c.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	ts.s.providerClient = c
	_, err = ts.s.fetchModelsWith(context.Background(), Provider{Name: "x", Type: "omlx", URL: "http://mac.lan:4000"}, "", "")
	want := "nothing is listening at mac.lan:4000"
	if runtime.GOOS == "windows" {
		want = "mac.lan:4000" // Windows reports a refusal with its own error number
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("refused: %v", err)
	}
	ts.s.providerClient = newProviderClient()
	_, err = ts.s.fetchModelsWith(context.Background(), Provider{Name: "x", Type: "other", URL: "https://no-such-host.invalid"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "is not known") {
		t.Errorf("unknown name: %v", err)
	}
}
