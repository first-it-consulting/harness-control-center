package control

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func goodCatalog() Catalog {
	return Catalog{
		Endpoints: []CatalogEndpoint{
			{Name: "anthropic", Kind: EndpointAnthropic, URL: "https://api.anthropic.com", Credential: "anthropic"},
			{Name: "studio", Kind: EndpointNetwork, URL: "http://mac-studio.lan:4000", Credential: "omlx"},
		},
		Models: []CatalogModel{
			{ID: "claude-sonnet-5-5", Label: "Claude Sonnet", Endpoint: "anthropic", SmallModel: "claude-haiku-5-5", MaxOutputTokens: 32000, ThinkingTokens: 8000},
			{ID: "Ornith-1.5-35B-A3B-MLX", Endpoint: "studio", ContextWindow: 262144},
		},
	}
}

func TestCatalogValidate(t *testing.T) {
	c := goodCatalog()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	m, private := c.Spec(c.Models[1])
	if !private || m.Endpoint != "http://mac-studio.lan:4000" || m.Credential != "omlx" || m.ContextWindow != 262144 {
		t.Errorf("spec = %+v, private %v", m, private)
	}
	for name, tt := range map[string]struct {
		change func(*Catalog)
		want   string
	}{
		"http outside the network": {func(c *Catalog) { c.Endpoints[0].URL = "http://api.anthropic.com" }, "must be https://"},
		"internal name, not network": {func(c *Catalog) {
			c.Endpoints[1].Kind = EndpointOther
			c.Endpoints[1].URL = "https://mac-studio.lan:4000"
		}, "looks internal"},
		"ip address":         {func(c *Catalog) { c.Endpoints[1].URL = "http://10.0.0.5:4000" }, "IP addresses"},
		"secret in the url":  {func(c *Catalog) { c.Endpoints[0].URL = "https://key:sk@api.anthropic.com" }, "credentials"},
		"bad endpoint name":  {func(c *Catalog) { c.Endpoints[0].Name = "Anthropic API" }, "endpoint name"},
		"duplicate endpoint": {func(c *Catalog) { c.Endpoints[1].Name = "anthropic" }, "another endpoint"},
		"unknown kind":       {func(c *Catalog) { c.Endpoints[0].Kind = "openai" }, "unknown kind"},
		"bad credential":     {func(c *Catalog) { c.Endpoints[0].Credential = "../secrets" }, "credential name"},
		"missing endpoint":   {func(c *Catalog) { c.Models[0].Endpoint = "nope" }, "no endpoint"},
		"duplicate model":    {func(c *Catalog) { c.Models[1] = c.Models[0] }, "listed twice"},
		"bad model id":       {func(c *Catalog) { c.Models[0].ID = "a b" }, "model name"},
		"thinking >= output": {func(c *Catalog) { c.Models[0].ThinkingTokens = 32000 }, "less than maxOutputTokens"},
		"small window":       {func(c *Catalog) { c.Models[1].ContextWindow = 100 }, "contextWindow"},
		"label with newline": {func(c *Catalog) { c.Models[0].Label = "a\nb" }, "one line"},
	} {
		c := goodCatalog()
		tt.change(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

func TestModelsThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	var v CatalogView
	if code := ts.do(http.MethodGet, "/api/models", nil, &v); code != http.StatusOK || v.Endpoints == nil || len(v.Models) != 0 {
		t.Fatalf("empty: %d %+v", code, v)
	}
	// A saved harness that uses the network model.
	used := strings.Replace(okHarness, "  prompt: hi\n", `  prompt: hi
  model: {name: Ornith-1.5-35B-A3B-MLX, endpoint: 'http://mac-studio.lan:4000', credential: omlx}
  permissions:
    credentials: [omlx]
    network: {allowPrivateNetworks: true, egress: [{host: mac-studio.lan, ports: [4000]}]}
`, 1)
	if code := ts.do(http.MethodPut, "/api/harnesses", map[string]string{"yaml": used}, nil); code != http.StatusOK {
		t.Fatalf("save harness: %d", code)
	}
	if code := ts.do(http.MethodPut, "/api/models", goodCatalog(), &v); code != http.StatusOK {
		t.Fatalf("PUT: %d", code)
	}
	if len(v.UsedBy) != 2 || len(v.UsedBy[0]) != 0 || len(v.UsedBy[1]) != 1 || v.UsedBy[1][0] != "hello" {
		t.Errorf("usedBy = %v", v.UsedBy)
	}
	ts.do(http.MethodGet, "/api/models", nil, &v)
	if len(v.Models) != 2 || v.Models[0].SmallModel != "claude-haiku-5-5" {
		t.Errorf("after save: %+v", v.Models)
	}
	data, err := os.ReadFile(filepath.Join(ts.dir, "models.yaml"))
	if err != nil || !strings.Contains(string(data), "mac-studio.lan") {
		t.Fatalf("models.yaml: %v %s", err, data)
	}
	if fi, err := os.Stat(filepath.Join(ts.dir, "models.yaml")); err == nil && fi.Mode().Perm()&0o077 != 0 && runtime.GOOS != "windows" {
		t.Errorf("models.yaml mode %v", fi.Mode())
	}

	// Refused: invalid entries (with every reason), unknown fields; the
	// file is left alone.
	bad := goodCatalog()
	bad.Endpoints[0].URL = "http://api.anthropic.com"
	bad.Models[0].ThinkingTokens = 99999
	var out map[string]any
	if code := ts.do(http.MethodPut, "/api/models", bad, &out); code != http.StatusBadRequest || len(out["errors"].([]any)) != 2 {
		t.Errorf("invalid: %d %v", code, out)
	}
	if rec := ts.rawJSON(http.MethodPut, "/api/models", map[string]any{"endpoints": []any{}, "models": []any{}, "secret": "x"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if after, _ := os.ReadFile(filepath.Join(ts.dir, "models.yaml")); string(after) != string(data) {
		t.Error("a refused change wrote the file")
	}
}
