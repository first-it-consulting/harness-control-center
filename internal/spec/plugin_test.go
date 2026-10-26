package spec

import (
	"strings"
	"testing"
	"time"
)

const notifierDoc = `
apiVersion: hcc/v1alpha1
kind: Plugin
metadata: {name: webhook, version: 0.1.0, description: Posts events to a webhook.}
spec:
  type: notifier
  command: [./notify.sh, --verbose]
  events: [job.ended, job.awaiting_approval]
  permissions:
    network:
      egress: [{host: hooks.example.com}]
    credentials: [hook]
`

func TestParsePlugin(t *testing.T) {
	p, err := ParsePlugin([]byte(notifierDoc))
	if err != nil {
		t.Fatal(err)
	}
	if p.Spec.Sandbox != (PluginSandbox{Image: "claude-code", CPUs: 1, MemoryMiB: 512, Timeout: Duration{2 * time.Minute}}) {
		t.Errorf("defaults = %+v", p.Spec.Sandbox)
	}
	if p.Spec.Permissions.Network.Egress[0].Ports[0] != 443 {
		t.Error("egress port not defaulted")
	}
	h, err := p.Harness("server:r1")
	if err != nil {
		t.Fatal(err)
	}
	if h.Spec.Runtime != "plugin" || h.Spec.Target != "server:r1" || h.Spec.Sandbox.MemoryMiB != 512 || len(h.Spec.Permissions.Credentials) != 1 {
		t.Errorf("harness = %+v", h.Spec)
	}
}

func TestParsePluginRejects(t *testing.T) {
	for name, tt := range map[string]struct{ old, new, want string }{
		"kind":            {"kind: Plugin", "kind: Harness", "kind"},
		"version":         {"version: 0.1.0", "version: latest", "metadata.version"},
		"type":            {"type: notifier", "type: shell", "spec.type"},
		"event":           {"[job.ended, job.awaiting_approval]", "[job.ended, everything]", "spec.events[1]"},
		"no events":       {"  events: [job.ended, job.awaiting_approval]\n", "", "spec.events"},
		"mcp with events": {"type: notifier", "type: mcp-server", "only notifiers"},
		"absolute":        {"[./notify.sh, --verbose]", "[/bin/sh, -c, x]", "spec.command[0]"},
		"escape":          {"[./notify.sh, --verbose]", "[../../bin/sh]", "spec.command[0]"},
		"option":          {"[./notify.sh, --verbose]", "[--help]", "spec.command[0]"},
		"no command":      {"[./notify.sh, --verbose]", "[]", "spec.command"},
		"line break":      {"--verbose]", "\"a\\nb\"]", "spec.command[1]"},
		"subagents":       {"    credentials: [hook]\n", "    credentials: [hook]\n    subagents: {max: 1}\n", "cannot start agents"},
		"timeout":         {"  events:", "  sandbox: {timeout: 2h}\n  events:", "spec.sandbox.timeout"},
		"unknown field":   {"  type: notifier", "  type: notifier\n  privileged: true", "not found"},
		"ip egress":       {"host: hooks.example.com", "host: 10.0.0.1", "egress"},
	} {
		_, err := ParsePlugin([]byte(strings.Replace(notifierDoc, tt.old, tt.new, 1)))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

func TestValidProgram(t *testing.T) {
	for p, want := range map[string]bool{
		"./run.sh": true, "bin/server": true, "python3": true, "a/./b": true,
		"/bin/sh": false, "../x": false, "a/../../x": false, ".": false, "-x": false, `a\b`: false,
	} {
		if got := validProgram(p); got != want {
			t.Errorf("validProgram(%q) = %v", p, got)
		}
	}
}
