package control

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

func goodPolicies() Policies {
	return Policies{Policies: []Policy{
		{Name: "github", Description: "GitHub and its downloads", Hosts: []PolicyHost{{Host: "github.com"}, {Host: "api.github.com"}}},
		{Name: "nas", Hosts: []PolicyHost{{Host: "nas.lan", Ports: []int{443, 8443}}}, Private: true},
	}}
}

func TestPoliciesValidate(t *testing.T) {
	ps := goodPolicies()
	if err := ps.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		change func(*Policies)
		want   string
	}{
		"internal, not private": {func(p *Policies) { p.Policies[1].Private = false }, "looks internal"},
		"ip address":            {func(p *Policies) { p.Policies[0].Hosts[0].Host = "10.0.0.1" }, "IP addresses"},
		"too broad":             {func(p *Policies) { p.Policies[0].Hosts[0].Host = "*.com" }, "too broad"},
		"bad port":              {func(p *Policies) { p.Policies[1].Hosts[0].Ports = []int{0} }, "port 0"},
		"no hosts":              {func(p *Policies) { p.Policies[0].Hosts = nil }, "at least one host"},
		"bad name":              {func(p *Policies) { p.Policies[0].Name = "Git Hub" }, "policy name"},
		"duplicate":             {func(p *Policies) { p.Policies[1].Name = "github" }, "another policy"},
		"two-line description":  {func(p *Policies) { p.Policies[0].Description = "a\nb" }, "one line"},
	} {
		ps := goodPolicies()
		tt.change(&ps)
		if err := ps.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

func TestPolicyCovers(t *testing.T) {
	p := goodPolicies().Policies[1]
	if !p.Covers([]spec.EgressRule{{Host: "x.example"}, {Host: "nas.lan", Ports: []int{8443, 443}}}) {
		t.Error("same ports in another order")
	}
	if p.Covers([]spec.EgressRule{{Host: "nas.lan"}}) {
		t.Error("fewer ports")
	}
	if !goodPolicies().Policies[0].Covers([]spec.EgressRule{{Host: "github.com", Ports: []int{443}}, {Host: "api.github.com"}}) {
		t.Error("443 explicit or not")
	}
}

func TestPoliciesThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	with := strings.Replace(okHarness, "  prompt: hi\n", "  prompt: hi\n  permissions:\n    network: {egress: [{host: github.com}, {host: api.github.com}, {host: pypi.org}]}\n", 1)
	if code := ts.do(http.MethodPut, "/api/harnesses", map[string]string{"yaml": with}, nil); code != http.StatusOK {
		t.Fatalf("save harness: %d", code)
	}
	ps := goodPolicies()
	ps.Policies[0].Hosts[0].Host = "GitHub.com." // normalized on save
	var out struct{ Policies []PolicyView }
	if code := ts.do(http.MethodPut, "/api/policies", ps, &out); code != http.StatusOK {
		t.Fatalf("PUT: %d", code)
	}
	if len(out.Policies) != 2 || out.Policies[0].Hosts[0].Host != "github.com" || len(out.Policies[0].UsedBy) != 1 || len(out.Policies[1].UsedBy) != 0 {
		t.Errorf("after save: %+v", out.Policies)
	}
	data, err := os.ReadFile(filepath.Join(ts.dir, "policies.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bad := goodPolicies()
	bad.Policies[1].Private = false
	var res map[string]any
	if code := ts.do(http.MethodPut, "/api/policies", bad, &res); code != http.StatusBadRequest {
		t.Errorf("invalid: %d %v", code, res)
	}
	if after, _ := os.ReadFile(filepath.Join(ts.dir, "policies.yaml")); string(after) != string(data) {
		t.Error("a refused change wrote the file")
	}
	ts.do(http.MethodGet, "/api/policies", nil, &out)
	if len(out.Policies) != 2 {
		t.Errorf("GET: %+v", out)
	}
}
