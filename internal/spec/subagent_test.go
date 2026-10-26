package spec

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const leadDoc = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: lead}
spec:
  runtime: claude-code
  prompt: lead the work
  target: server:r1
  model: {name: m1, endpoint: 'http://llm.lan:4000', credential: llm}
  sandbox: {image: claude-code, cpus: 2, memoryMiB: 2048, diskMiB: 1024, timeout: 30m}
  permissions:
    credentials: [llm, gitlab]
    subagents: {max: 3}
    network:
      allowPrivateNetworks: true
      egress:
        - {host: llm.lan, ports: [4000]}
        - host: gitlab.com
  budget: {maxTokens: 5000}
  approvals: {requireFor: [git.push]}
`

func TestChildInheritsFromItsParent(t *testing.T) {
	parent, err := Parse([]byte(leadDoc))
	if err != nil {
		t.Fatal(err)
	}
	req, err := ParseSubagent([]byte(`{"prompt": "write the tests", "permissions": {"network": {"egress": [{"host": "gitlab.com"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	c, err := Child(parent, req, 20*time.Minute+500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	s := c.Spec
	if c.Metadata.Name != DefaultSubagentName || s.Prompt != "write the tests" || s.Runtime != "claude-code" || s.Target != "server:r1" {
		t.Errorf("child = %+v", c)
	}
	if s.Model == nil || *s.Model != *parent.Spec.Model {
		t.Errorf("model = %+v", s.Model)
	}
	if s.Sandbox != (Sandbox{Image: "claude-code", CPUs: 2, MemoryMiB: 2048, DiskMiB: 1024, Timeout: Duration{20 * time.Minute}}) {
		t.Errorf("sandbox = %+v, want the parent's, limited to the time it has left", s.Sandbox)
	}
	if s.Budget != parent.Spec.Budget || !slices.Equal(s.Approvals.RequireFor, []string{"git.push"}) {
		t.Errorf("budget %+v, approvals %v", s.Budget, s.Approvals.RequireFor)
	}
	// What reaching the model needs is added; nothing else is.
	p := s.Permissions
	if !slices.Equal(p.Credentials, []string{"llm"}) || !p.Network.AllowPrivateNetworks || p.Subagents.Max != 0 {
		t.Errorf("permissions = %+v", p)
	}
	if len(p.Network.Egress) != 2 || p.Network.Egress[1].Host != "llm.lan" || !slices.Equal(p.Network.Egress[1].Ports, []int{4000}) {
		t.Errorf("egress = %+v", p.Network.Egress)
	}
	if s.Trigger != (Trigger{}) {
		t.Errorf("trigger = %+v", s.Trigger)
	}
}

func TestChildModelAccessIsNotDuplicated(t *testing.T) {
	parent, err := Parse([]byte(leadDoc))
	if err != nil {
		t.Fatal(err)
	}
	req := &Subagent{Prompt: "x", Permissions: Permissions{
		Credentials: []string{"llm"},
		Network:     Network{AllowPrivateNetworks: true, Egress: []EgressRule{{Host: "llm.lan", Ports: []int{4000}}}},
	}}
	c, err := Child(parent, req, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Spec.Permissions.Network.Egress) != 1 || len(c.Spec.Permissions.Credentials) != 1 {
		t.Errorf("permissions = %+v", c.Spec.Permissions)
	}
	if req.Permissions.Network.Egress[0].Host != "llm.lan" || len(req.Permissions.Credentials) != 1 {
		t.Error("Child changed the request")
	}
}

func TestChildRejects(t *testing.T) {
	parent, err := Parse([]byte(leadDoc))
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		req  Subagent
		left time.Duration
		want string
	}{
		"cpus":         {Subagent{Prompt: "x", Sandbox: SubagentSandbox{CPUs: 3}}, time.Hour, "cpus"},
		"memory":       {Subagent{Prompt: "x", Sandbox: SubagentSandbox{MemoryMiB: 4096}}, time.Hour, "memoryMiB"},
		"disk":         {Subagent{Prompt: "x", Sandbox: SubagentSandbox{DiskMiB: 2048}}, time.Hour, "diskMiB"},
		"timeout":      {Subagent{Prompt: "x", Sandbox: SubagentSandbox{Timeout: Duration{11 * time.Minute}}}, 10 * time.Minute, "remaining time"},
		"no time left": {Subagent{Prompt: "x"}, 30 * time.Second, "less than a minute"},
		"no prompt":    {Subagent{Prompt: "  "}, time.Hour, "spec.prompt"},
		"bad name":     {Subagent{Name: "Bad", Prompt: "x"}, time.Hour, "metadata.name"},
		"bad tool":     {Subagent{Prompt: "x", Permissions: Permissions{Tools: Tools{Install: []string{"a;b"}}}}, time.Hour, "tools.install"},
	} {
		if _, err := Child(parent, &tt.req, tt.left); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

func TestParseSubagentIsStrict(t *testing.T) {
	for _, in := range []string{
		`{"prompt": "x", "image": "y"}`,
		`{"prompt": "x", "sandbox": {"image": "y"}}`,
		`{"prompt": "x", "budget": {"maxTokens": 1}}`,
		`{"prompt": "x", "prompt": "y"}`,
		"prompt: x\n---\nprompt: y\n",
		"",
		strings.Repeat("x", MaxSubagentBytes+1),
	} {
		if _, err := ParseSubagent([]byte(in)); err == nil {
			t.Errorf("ParseSubagent(%.40q) succeeded", in)
		}
	}
}
