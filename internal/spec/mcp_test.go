package spec

import (
	"strings"
	"testing"
)

const mcpDoc = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: m}
spec:
  runtime: claude-code
  prompt: p
  mcp:
    servers:
      - {name: docs, url: "https://mcp.example.com/v1/mcp"}
      - {name: notes, plugin: notes-mcp}
  permissions:
    network:
      egress: [{host: mcp.example.com}]
`

func TestMCPServers(t *testing.T) {
	h, err := Parse([]byte(mcpDoc))
	if err != nil {
		t.Fatal(err)
	}
	host, port, path := h.Spec.MCP.Servers[0].EndpointHostPort()
	if host != "mcp.example.com" || port != 443 || path != "/v1/mcp" {
		t.Errorf("endpoint = %s %d %s", host, port, path)
	}
	for name, tt := range map[string]struct{ old, new, want string }{
		"both":       {"plugin: notes-mcp", "plugin: notes-mcp, url: \"https://x.example.com\"", "exactly one"},
		"neither":    {", plugin: notes-mcp", "", "exactly one"},
		"http":       {"https://mcp.example.com", "http://mcp.example.com", "https://"},
		"userinfo":   {"https://mcp.example.com", "https://u:p@mcp.example.com", "credentials"},
		"query":      {"/v1/mcp\"", "/v1/mcp?k=v\"", "query"},
		"ip":         {"https://mcp.example.com", "https://10.0.0.1", "url"},
		"dup":        {"name: notes", "name: docs", "listed twice"},
		"bad name":   {"name: notes", "name: Notes", "name"},
		"bad plugin": {"plugin: notes-mcp", "plugin: ../x", "plugin"},
	} {
		_, err := Parse([]byte(strings.Replace(mcpDoc, tt.old, tt.new, 1)))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

func TestA2AAgents(t *testing.T) {
	doc := `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: a}
spec:
  runtime: claude-code
  prompt: p
  a2a:
    agents: [{name: reviewer, url: "https://agents.example.com/a2a"}]
  permissions:
    network:
      egress: [{host: agents.example.com}]
`
	h, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if host, port, path := h.Spec.A2A.Agents[0].EndpointHostPort(); host != "agents.example.com" || port != 443 || path != "/a2a" {
		t.Errorf("endpoint = %s %d %s", host, port, path)
	}
	for name, tt := range map[string]struct{ old, new, want string }{
		"http":    {"https://agents", "http://agents", "https://"},
		"no url":  {`, url: "https://agents.example.com/a2a"`, "", "https://"},
		"dup":     {"[{name: reviewer", "[{name: x, url: \"https://agents.example.com/b\"}, {name: x", "listed twice"},
		"private": {"agents.example.com/a2a", "agents.lan/a2a", "url"},
	} {
		if _, err := Parse([]byte(strings.Replace(doc, tt.old, tt.new, 1))); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}
