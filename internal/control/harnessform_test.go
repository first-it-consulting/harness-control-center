package control

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Every example survives YAML → form data → YAML with the same meaning.
func TestFormRoundTripsTheExamples(t *testing.T) {
	ts := newTestServer(t)
	files, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("examples: %v %v", files, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // test data
		if err != nil {
			t.Fatal(err)
		}
		want, err := spec.Parse(data)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		var dec formDoc
		if code := ts.do(http.MethodPost, "/api/harness/decode", map[string]string{"yaml": string(data)}, &dec); code != http.StatusOK {
			t.Fatalf("%s: decode %d", f, code)
		}
		var enc encodeResult
		if code := ts.do(http.MethodPost, "/api/harness/encode", map[string]any{"doc": dec.Doc}, &enc); code != http.StatusOK {
			t.Fatalf("%s: encode %d", f, code)
		}
		got, err := spec.Parse([]byte(enc.YAML))
		if err != nil {
			t.Fatalf("%s: the form's YAML does not parse: %v\n%s", f, err, enc.YAML)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: changed meaning:\n%s", f, enc.YAML)
		}
		if !enc.Check.Valid {
			t.Errorf("%s: check %+v", f, enc.Check)
		}
	}
}

func TestEncodeForm(t *testing.T) {
	doc := `{"spec": {"prompt": "line one\nline two", "runtime": "claude-code", "budget": {"maxCostUSD": 2.5, "maxTokens": 0},
		"permissions": {"network": {"allowPrivateNetworks": false, "egress": [{"host": "api.example", "ports": [443, 8443]}]},
		"credentials": [], "tools": {"install": ["", "git"]}}, "model": null, "target": "  "},
		"metadata": {"name": "x", "description": ""}, "kind": "Harness", "apiVersion": "hcc/v1alpha1"}`
	out, err := encodeForm(json.RawMessage(doc))
	if err != nil {
		t.Fatal(err)
	}
	// Reading order, empty values left out, the prompt as a block.
	order := []string{"apiVersion:", "kind:", "metadata:", "spec:", "runtime:", "prompt: |", "budget:", "maxCostUSD: 2.5", "permissions:", "egress:", "ports: [443, 8443]", "install: [git]"}
	last := -1
	for _, k := range order {
		i := strings.Index(out, k)
		if i < 0 || i < last {
			t.Fatalf("%q missing or out of order in:\n%s", k, out)
		}
		last = i
	}
	for _, gone := range []string{"description", "maxTokens", "allowPrivateNetworks", "credentials", "model", "target"} {
		if strings.Contains(out, gone) {
			t.Errorf("empty %s kept:\n%s", gone, out)
		}
	}
	var back map[string]any
	if err := yaml.Unmarshal([]byte(out), &back); err != nil {
		t.Fatal(err)
	}

	for name, bad := range map[string]string{
		"not an object": `["a"]`,
		"too deep":      strings.Repeat(`{"a":`, 20) + `1` + strings.Repeat(`}`, 20),
		"broken":        `{"a":`,
	} {
		if _, err := encodeForm(json.RawMessage(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestHarnessFormAPI(t *testing.T) {
	ts := newTestServer(t)
	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/harness/decode", map[string]string{"yaml": "a: [b"}, &out); code != http.StatusBadRequest {
		t.Errorf("broken YAML: %d", code)
	}
	var enc encodeResult
	ts.do(http.MethodPost, "/api/harness/encode", map[string]any{"doc": map[string]any{"spec": map[string]any{"prompt": "hi"}}}, &enc)
	if enc.Check.Valid || len(enc.Check.Errors) == 0 {
		t.Errorf("an incomplete harness is valid: %+v", enc.Check)
	}
	var l formLimits
	if code := ts.do(http.MethodGet, "/api/harness/limits", nil, &l); code != http.StatusOK || l.MaxCPUs != spec.MaxCPUs || l.Defaults.Timeout != "30m0s" || len(l.ApprovalAction) == 0 {
		t.Errorf("limits: %d %+v", code, l)
	}
}

func FuzzEncodeForm(f *testing.F) {
	f.Add([]byte(`{"spec": {"prompt": "a\nb", "budget": {"maxCostUSD": 1.5}}}`))
	f.Add([]byte(`{"a": [1, "", null, {"b": false}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := encodeForm(json.RawMessage(data))
		if err != nil {
			return
		}
		var v any
		if err := yaml.Unmarshal([]byte(out), &v); err != nil {
			t.Fatalf("not YAML: %v\n%s", err, out)
		}
	})
}
