package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"
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

// CheckApprovalPatterns reports the patterns no gate can enforce.
func CheckApprovalPatterns(patterns []string) error {
	var errs []error
	for _, p := range patterns {
		if !enforceable(p) {
			errs = append(errs, fmt.Errorf("approval for %q cannot be enforced (supported: %s, or http.* and git.*)",
				p, strings.Join(ApprovalActions, ", ")))
		}
	}
	return errors.Join(errs...)
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
