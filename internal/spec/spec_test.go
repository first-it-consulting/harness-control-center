package spec

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

const minimal = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata:
  name: hello
spec:
  runtime: claude-code
  prompt: Say hello.
`

func TestParseMinimalAppliesSafeDefaults(t *testing.T) {
	h, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := h.Spec
	if s.Target != "local" {
		t.Errorf("Target = %q, want local", s.Target)
	}
	if s.Sandbox.Timeout.Duration != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", s.Sandbox.Timeout, DefaultTimeout)
	}
	if s.Budget.MaxCostUSD != DefaultMaxCostUSD || s.Budget.MaxTokens != DefaultMaxTokens {
		t.Errorf("Budget = %+v, want defaults", s.Budget)
	}
	// Deny by default: no permission may be granted implicitly.
	p := s.Permissions
	if len(p.Network.Egress) != 0 || p.Network.AllowPrivateNetworks ||
		len(p.Credentials) != 0 || len(p.Tools.Install) != 0 || p.Subagents.Max != 0 {
		t.Errorf("Permissions = %+v, want none", p)
	}
}

func TestParseFull(t *testing.T) {
	doc := minimal + `
  target: server:homelab-01
  trigger:
    schedule: "0 2 * * *"
  sandbox:
    image: base-dev:1.0
    cpus: 2
    memoryMiB: 4096
    timeout: 2h
  permissions:
    network:
      egress:
        - host: GitLab.com.
        - host: "*.npmjs.org"
          ports: [443, 80]
    credentials: [gitlab-main]
    tools:
      install: [node@22, ripgrep]
    subagents:
      max: 2
  budget:
    maxCostUSD: 5
    maxTokens: 2000000
  approvals:
    requireFor: [git.push, delete.*]
`
	h, err := Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := h.Spec
	if s.Sandbox.Timeout.Duration != 2*time.Hour {
		t.Errorf("Timeout = %v", s.Sandbox.Timeout)
	}
	if got := s.Permissions.Network.Egress[0]; got.Host != "gitlab.com" || len(got.Ports) != 1 || got.Ports[0] != 443 {
		t.Errorf("egress[0] = %+v, want normalized host with default port", got)
	}
	if len(s.Permissions.Network.Egress[1].Ports) != 2 {
		t.Errorf("egress[1] ports = %v", s.Permissions.Network.Egress[1].Ports)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{"empty", "", "empty document"},
		{"unknown field (typo must not be ignored)", minimal + "  permission: {}\n", "field permission not found"},
		{"multiple documents", minimal + "---\n" + minimal, "multiple documents"},
		{"wrong apiVersion", strings.Replace(minimal, "hcc/v1alpha1", "hcc/v9", 1), "apiVersion"},
		{"wrong kind", strings.Replace(minimal, "kind: Harness", "kind: Pod", 1), "kind"},
		{"bad name", strings.Replace(minimal, "name: hello", "name: Hello_World", 1), "metadata.name"},
		{"empty prompt", strings.Replace(minimal, "Say hello.", "'  '", 1), "spec.prompt"},
		{"bad target", minimal + "  target: cloud\n", "spec.target"},
		{"schedule and loop", minimal + "  trigger: {schedule: '* * * * *', loop: {maxIterations: 1}}\n", "mutually exclusive"},
		{"bad cron", minimal + "  trigger: {schedule: 'every day'}\n", "cron"},
		{"loop without limit", minimal + "  trigger: {loop: {}}\n", "maxIterations"},
		{"timeout too long", minimal + "  sandbox: {timeout: 48h}\n", "spec.sandbox.timeout"},
		{"negative timeout", minimal + "  sandbox: {timeout: -1m}\n", "spec.sandbox.timeout"},
		{"bad duration", minimal + "  sandbox: {timeout: forever}\n", "invalid duration"},
		{"image traversal", minimal + "  sandbox: {image: base/../../etc}\n", "spec.sandbox.image"},
		{"too many cpus", minimal + "  sandbox: {cpus: 999}\n", "spec.sandbox.cpus"},
		{"budget too high", minimal + "  budget: {maxCostUSD: 100000}\n", "maxCostUSD"},
		{"negative budget", minimal + "  budget: {maxCostUSD: -1}\n", "maxCostUSD"},
		{"NaN budget", minimal + "  budget: {maxCostUSD: .nan}\n", "maxCostUSD"},
		{"infinite budget", minimal + "  budget: {maxCostUSD: .inf}\n", "maxCostUSD"},
		{"bad port", minimal + "  permissions: {network: {egress: [{host: a.com, ports: [0]}]}}\n", "ports[0]"},
		{"duplicate credential", minimal + "  permissions: {credentials: [a, a]}\n", "duplicate"},
		{"bad tool", minimal + "  permissions: {tools: {install: ['rm -rf /']}}\n", "tools.install"},
		{"too many subagents", minimal + "  permissions: {subagents: {max: 100}}\n", "subagents.max"},
		{"bad approval", minimal + "  approvals: {requireFor: ['*']}\n", "requireFor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil {
				t.Fatalf("Parse succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseReportsAllErrors(t *testing.T) {
	doc := minimal + "  target: nowhere\n  sandbox: {cpus: 99}\n"
	_, err := Parse([]byte(doc))
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("want FieldError, got %v", err)
	}
	for _, want := range []string{"spec.target", "spec.sandbox.cpus"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestParseTooLarge(t *testing.T) {
	big := make([]byte, MaxDocumentBytes+1)
	if _, err := Parse(big); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestValidateEgressHost(t *testing.T) {
	tests := []struct {
		host         string
		allowPrivate bool
		ok           bool
	}{
		{"gitlab.com", false, true},
		{"registry.npmjs.org", false, true},
		{"*.npmjs.org", false, true},
		{"xn--bcher-kva.example", false, true},
		{"*", false, false},
		{"*.com", false, false},
		{"*.*.example.com", false, false},
		{"foo.*.example.com", false, false},
		{"localhost", false, false},
		{"com", false, false},
		{"GitLab.com", false, false}, // must be normalized first
		{"gitlab.com.", false, false},
		{"1.2.3.4", false, false},
		{"169.254.169.254", true, false}, // cloud metadata: IPs never allowed
		{"[::1]", true, false},
		{"::1", true, false},
		{"-bad.example.com", false, false},
		{"under_score.example.com", false, false},
		{"gitlab.lan", false, false},
		{"gitlab.lan", true, true},
		{"nas.home.arpa", false, false},
		{"svc.cluster.internal", false, false},
		{"printer.local", true, true},
		{"nas.fritz.box", false, false},
		{"nas.fritz.box", true, true},
		{"", false, false},
		{strings.Repeat("a.", 130) + "com", false, false},
	}
	for _, tt := range tests {
		err := ValidateEgressHost(tt.host, tt.allowPrivate)
		if (err == nil) != tt.ok {
			t.Errorf("ValidateEgressHost(%q, %v) = %v, want ok=%v", tt.host, tt.allowPrivate, err, tt.ok)
		}
	}
}

func TestDurationMarshalRoundTrip(t *testing.T) {
	d := Duration{90 * time.Minute}
	v, err := d.MarshalYAML()
	if err != nil || v != "1h30m0s" {
		t.Errorf("MarshalYAML = %v, %v", v, err)
	}
}

// FuzzParse checks that arbitrary input never panics and that anything
// accepted is bounded, whatever the input.
func FuzzParse(f *testing.F) {
	f.Add([]byte(minimal))
	f.Add([]byte(minimal + "  budget: {maxCostUSD: .nan}\n"))
	f.Add([]byte(minimal + "  permissions: {network: {egress: [{host: '*.a.com'}]}}\n"))
	f.Add([]byte("a: &a [*a, *a, *a]"))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := Parse(data)
		if err != nil {
			return
		}
		s := h.Spec
		if s.Sandbox.Timeout.Duration <= 0 || s.Sandbox.Timeout.Duration > MaxTimeout {
			t.Fatalf("accepted unbounded timeout %v", s.Sandbox.Timeout)
		}
		if !(s.Budget.MaxCostUSD > 0 && s.Budget.MaxCostUSD <= MaxCostUSD) || math.IsInf(s.Budget.MaxCostUSD, 0) {
			t.Fatalf("accepted unbounded cost %v", s.Budget.MaxCostUSD)
		}
		if s.Permissions.Subagents.Max > MaxSubagents {
			t.Fatalf("accepted %d subagents", s.Permissions.Subagents.Max)
		}
		for _, r := range s.Permissions.Network.Egress {
			if err := ValidateEgressHost(r.Host, s.Permissions.Network.AllowPrivateNetworks); err != nil {
				t.Fatalf("accepted invalid host %q: %v", r.Host, err)
			}
		}
	})
}

func TestValidateHostNameRejectsIPs(t *testing.T) {
	for _, h := range []string{"1.2.3.4", "127.0.0.1", "::1", "[::1]", "2001:db8::1"} {
		if ValidateHostName(h) == nil {
			t.Errorf("ValidateHostName(%q) accepted an IP address", h)
		}
	}
	for _, h := range []string{"a.example", "1.example", "x1.2.example"} {
		if err := ValidateHostName(h); err != nil {
			t.Errorf("ValidateHostName(%q) = %v", h, err)
		}
	}
}

func TestModel(t *testing.T) {
	const with = minimal + `  model:
    name: Ornith-1.5-35B-A3B-MLX
    endpoint: http://LLM.example:4000
    credential: llm
  permissions:
    credentials: [llm]
`
	h, err := Parse([]byte(with))
	if err != nil {
		t.Fatal(err)
	}
	scheme, host, port := h.Spec.Model.EndpointHostPort()
	if scheme != "http" || host != "llm.example" || port != 4000 {
		t.Errorf("EndpointHostPort = %s %s %d", scheme, host, port)
	}
	for endpoint, want := range map[string]int{"https://api.anthropic.com": 443, "http://a.example": 80, "https://a.example:8443/v1": 8443} {
		m := Model{Endpoint: endpoint}
		if _, _, port := m.EndpointHostPort(); port != want {
			t.Errorf("%s: port %d, want %d", endpoint, port, want)
		}
	}

	bad := map[string]struct{ model, want string }{
		"ftp":              {"{name: m, endpoint: 'ftp://a.example'}", "http:// or https://"},
		"ip literal":       {"{name: m, endpoint: 'http://10.10.20.140:4000'}", "IP addresses"},
		"userinfo":         {"{name: m, endpoint: 'https://u:p@a.example'}", "credentials, a query"},
		"query":            {"{name: m, endpoint: 'https://a.example/?k=1'}", "credentials, a query"},
		"bad path":         {"{name: m, endpoint: 'https://a.example/%0d%0a'}", "invalid path"},
		"internal name":    {"{name: m, endpoint: 'http://llm.lan:4000'}", "looks internal"},
		"bad port":         {"{name: m, endpoint: 'http://a.example:99999'}", "port"},
		"bad name":         {"{name: 'a b', endpoint: 'https://a.example'}", "model name"},
		"credential grant": {"{name: m, endpoint: 'https://a.example', credential: llm}", "not in permissions.credentials"},
	}
	for name, tt := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(minimal + "  model: " + tt.model + "\n"))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse = %v, want %q", err, tt.want)
			}
		})
	}
}
