package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// ApprovalActions are the actions approval gates can enforce: what the
// egress proxy can see in the HTTP requests it reads. An action pattern
// in spec.approvals.requireFor must name one of these, or a group with
// ".*" ("http.*", "git.*"); a runner rejects harnesses with any other,
// so nobody is told "asks you before X" for an X nothing can catch.
var ApprovalActions = []string{
	"http.get", "http.head", "http.post", "http.put", "http.patch", "http.delete", "http.other",
	"git.push",
}

// CheckApprovalPatterns reports the patterns of s.approvals.requireFor no
// gate can enforce. Besides ApprovalActions, MCP tool calls of the
// harness's own servers can be gated: mcp.<server>.<tool> (tool names in
// lower case), or groups like mcp.<server>.* and mcp.*.
func CheckApprovalPatterns(s *spec.Spec) error {
	var errs []error
	for _, p := range s.Approvals.RequireFor {
		if !enforceable(p) && !enforceableMCP(p, s) {
			errs = append(errs, fmt.Errorf("approval for %q cannot be enforced (supported: %s, http.* and git.*, "+
				"and mcp.<server>.<tool> for the servers in spec.mcp)", p, strings.Join(ApprovalActions, ", ")))
		}
	}
	return errors.Join(errs...)
}

// enforceableMCP: a pattern below "mcp" naming no server or one of s's.
func enforceableMCP(p string, s *spec.Spec) bool {
	parts := strings.Split(strings.TrimSuffix(p, ".*"), ".")
	if parts[0] != "mcp" || len(s.MCP.Servers) == 0 {
		return false
	}
	return len(parts) == 1 || slices.ContainsFunc(s.MCP.Servers, func(m spec.MCPServer) bool { return m.Name == parts[1] })
}

// GatesMCP reports whether some MCP tool calls need approval.
func (e *Engine) GatesMCP() bool {
	return slices.ContainsFunc(e.h.Spec.Approvals.RequireFor, func(p string) bool {
		return p == "mcp" || strings.HasPrefix(p, "mcp.")
	})
}

// MCPAction is the approval action of calling tool on MCP server.
func MCPAction(server, tool string) string {
	return "mcp." + server + "." + strings.ToLower(tool)
}

func enforceable(p string) bool {
	if prefix, ok := strings.CutSuffix(p, ".*"); ok {
		return slices.ContainsFunc(ApprovalActions, func(a string) bool { return strings.HasPrefix(a, prefix+".") })
	}
	return slices.Contains(ApprovalActions, p) || p == "http" || p == "git"
}

// HTTPActions are the approval actions of one HTTP request.
func HTTPActions(method, path string) []string {
	m := strings.ToLower(method)
	switch m {
	case "get", "head", "post", "put", "patch", "delete":
	default:
		m = "other"
	}
	actions := []string{"http." + m}
	if m == "post" && strings.HasSuffix(path, "/git-receive-pack") {
		actions = append(actions, "git.push")
	}
	return actions
}

// RequiresApprovalAny reports whether any of actions needs approval.
func (e *Engine) RequiresApprovalAny(actions []string) bool {
	return slices.ContainsFunc(actions, e.RequiresApproval)
}
