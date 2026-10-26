// Package policy turns a validated harness into decisions. Every
// enforcement point (egress proxy, runner, credential broker) asks the
// Engine instead of interpreting the spec itself, so there is exactly one
// implementation of "is this allowed?".
//
// The Engine fails closed: anything not explicitly granted is denied.
package policy

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Decision is the result of a policy check. Reason is meant for logs and
// for the user, and is set for both allow and deny.
type Decision struct {
	Allowed bool
	Reason  string
}

func allow(format string, args ...any) Decision {
	return Decision{Allowed: true, Reason: fmt.Sprintf(format, args...)}
}

func deny(format string, args ...any) Decision {
	return Decision{Allowed: false, Reason: fmt.Sprintf(format, args...)}
}

// Engine evaluates requests against one harness. It is immutable and safe
// for concurrent use.
type Engine struct {
	h *spec.Harness
}

// New returns an engine for h. h must come from spec.Parse (or have passed
// Validate); New re-validates to guarantee that.
func New(h *spec.Harness) (*Engine, error) {
	if h == nil {
		return nil, fmt.Errorf("policy: nil harness")
	}
	if err := h.Validate(); err != nil {
		return nil, fmt.Errorf("policy: invalid harness: %w", err)
	}
	e := &Engine{h: h}
	if m := h.Spec.Model; m != nil {
		if _, host, port := m.EndpointHostPort(); !e.CheckEgress(host, port).Allowed {
			return nil, fmt.Errorf("policy: model endpoint %s:%d is not in the egress allowlist", host, port)
		}
	}
	for _, s := range h.Spec.MCP.Servers {
		if s.URL == "" {
			continue
		}
		if host, port, _ := s.EndpointHostPort(); !e.CheckEgress(host, port).Allowed {
			return nil, fmt.Errorf("policy: MCP server %s (%s:%d) is not in the egress allowlist", s.Name, host, port)
		}
	}
	return e, nil
}

// CheckEgress decides whether the sandbox may open a connection to
// host:port. host is what the agent asked for (e.g. the CONNECT target),
// not a resolved address; the proxy separately checks resolved IPs.
func (e *Engine) CheckEgress(host string, port int) Decision {
	host = spec.NormalizeHost(host)
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return deny("connections to IP addresses are not allowed (%s)", host)
	}
	// Only plain DNS names are matched. Anything else ("evil.com#.a.org",
	// "evil.com/.a.org", "*.a.org") could suffix-match a rule here while a
	// proxy or resolver interprets it as a different host.
	if err := spec.ValidateHostName(host); err != nil {
		return deny("invalid host: %v", err)
	}
	for _, r := range e.h.Spec.Permissions.Network.Egress {
		if hostMatches(r.Host, host) && slices.Contains(r.Ports, port) {
			return allow("matches egress rule %s", r.Host)
		}
	}
	return deny("%s:%d is not in the egress allowlist", host, port)
}

// hostMatches reports whether host matches pattern. "*.example.com"
// matches any subdomain of example.com but not example.com itself and not
// "badexample.com".
func hostMatches(pattern, host string) bool {
	if suffix, ok := strings.CutPrefix(pattern, "*"); ok {
		return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
	}
	return pattern == host
}

// CheckCredential decides whether the named credential may be injected
// into this harness's outbound requests.
func (e *Engine) CheckCredential(name string) Decision {
	if slices.Contains(e.h.Spec.Permissions.Credentials, name) {
		return allow("credential %s is granted", name)
	}
	return deny("credential %s is not granted", name)
}

// CheckToolInstall decides whether pkg (optionally "name@version") may be
// installed. A grant without a version allows any version; a pinned grant
// allows only that exact version.
func (e *Engine) CheckToolInstall(pkg string) Decision {
	// Reject malformed references outright, so an installer can never be
	// handed something like "ripgrep@14;rm -rf /" because the name matched.
	if !spec.ValidPackage(pkg) {
		return deny("invalid package reference %q", pkg)
	}
	name, _, _ := strings.Cut(pkg, "@")
	for _, g := range e.h.Spec.Permissions.Tools.Install {
		if g == pkg || g == name {
			return allow("tool %s is granted", g)
		}
	}
	return deny("installing %s is not granted", pkg)
}

// CheckSpawnSubagent decides whether another subagent may start, given how
// many this harness's job and its subagents have started so far (the
// limit covers its whole subtree, over its whole run).
func (e *Engine) CheckSpawnSubagent(started int) Decision {
	limit := e.h.Spec.Permissions.Subagents.Max
	if started < limit {
		return allow("subagent %d of %d", started+1, limit)
	}
	if limit == 0 {
		return deny("subagents are not allowed")
	}
	return deny("subagent limit of %d reached", limit)
}

// RequiresApproval reports whether action (e.g. "git.push") must be
// approved by a human before it runs. A pattern "git.*" covers "git" and
// every action below it.
func (e *Engine) RequiresApproval(action string) bool {
	for _, p := range e.h.Spec.Approvals.RequireFor {
		if prefix, ok := strings.CutSuffix(p, ".*"); ok {
			if action == prefix || strings.HasPrefix(action, prefix+".") {
				return true
			}
		} else if p == action {
			return true
		}
	}
	return false
}

// CheckChild decides whether a subagent may run with the permissions
// requested. A child may only ever receive a subset of its parent's
// permissions: it can narrow, never widen.
func (e *Engine) CheckChild(child spec.Permissions) Decision {
	parent := e.h.Spec.Permissions
	if child.Network.AllowPrivateNetworks && !parent.Network.AllowPrivateNetworks {
		return deny("child requests private networks, parent does not have them")
	}
	for _, r := range child.Network.Egress {
		host := spec.NormalizeHost(r.Host)
		if err := spec.ValidateEgressHost(host, parent.Network.AllowPrivateNetworks); err != nil {
			return deny("child requests invalid egress: %v", err)
		}
		ports := r.Ports
		if len(ports) == 0 {
			ports = []int{spec.DefaultEgressPort} // same default as the spec
		}
		for _, port := range ports {
			if !e.coversEgress(host, port) {
				return deny("child requests egress %s:%d beyond the parent's", host, port)
			}
		}
	}
	for _, c := range child.Credentials {
		if d := e.CheckCredential(c); !d.Allowed {
			return deny("child requests %s", d.Reason)
		}
	}
	for _, t := range child.Tools.Install {
		if d := e.CheckToolInstall(t); !d.Allowed {
			return deny("child requests %s", d.Reason)
		}
	}
	if child.Subagents.Max > parent.Subagents.Max {
		return deny("child requests %d subagents, parent has %d", child.Subagents.Max, parent.Subagents.Max)
	}
	return allow("child permissions are a subset of the parent's")
}

// coversEgress reports whether some parent rule allows everything the
// child rule (host, port) would allow. A child wildcard is covered only
// by a parent wildcard of the same or a broader suffix.
func (e *Engine) coversEgress(host string, port int) bool {
	for _, r := range e.h.Spec.Permissions.Network.Egress {
		if !slices.Contains(r.Ports, port) {
			continue
		}
		if r.Host == host {
			return true
		}
		if strings.HasPrefix(r.Host, "*.") && hostMatches(r.Host, strings.TrimPrefix(host, "*.")) {
			return true
		}
		if !strings.HasPrefix(host, "*.") && hostMatches(r.Host, host) {
			return true
		}
	}
	return false
}
