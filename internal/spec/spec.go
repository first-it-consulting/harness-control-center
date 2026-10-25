// Package spec defines the Harness specification: the declarative,
// versioned description of what an agent may do, where it runs and
// within which limits.
//
// Safety principles enforced by this package:
//   - Everything is denied unless explicitly granted (no network, no
//     credentials, no subagents, no tool installs).
//   - Every harness is bounded: a timeout and a budget always apply, and
//     neither can be disabled.
//   - Parsing is strict: unknown fields are an error, so a typo in a
//     permission can never be silently ignored.
package spec

import (
	"net/url"
	"strconv"
	"time"
)

// APIVersion is the only spec version this build understands.
const APIVersion = "hcc/v1alpha1"

// Kind is the only document kind this package parses.
const Kind = "Harness"

// Harness is the top-level document.
type Harness struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata identifies a harness.
type Metadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

// Spec is the body of a harness.
type Spec struct {
	// Runtime selects the agent runtime plugin, e.g. "claude-code".
	Runtime string `yaml:"runtime"`
	// Prompt is the task given to the agent.
	Prompt string `yaml:"prompt"`
	// Model selects the model and the endpoint the runtime talks to; nil
	// means the runtime's default.
	Model *Model `yaml:"model,omitempty"`
	// Target is "local" or "server:<name>".
	Target      string      `yaml:"target,omitempty"`
	Trigger     Trigger     `yaml:"trigger,omitempty"`
	Sandbox     Sandbox     `yaml:"sandbox,omitempty"`
	Permissions Permissions `yaml:"permissions,omitempty"`
	Budget      Budget      `yaml:"budget,omitempty"`
	Approvals   Approvals   `yaml:"approvals,omitempty"`
}

// Model is the model an agent runtime uses.
type Model struct {
	// Name is the model's identifier at the endpoint.
	Name string `yaml:"name"`
	// Endpoint is the API base URL, e.g. https://api.anthropic.com. Its
	// host and port must be in the egress allowlist.
	Endpoint string `yaml:"endpoint"`
	// Credential names the granted credential that authenticates to the
	// endpoint (added by the egress proxy; the agent never sees it).
	Credential string `yaml:"credential,omitempty"`
}

// EndpointHostPort returns the endpoint's host and port (default 443 for
// https, 80 for http). The endpoint must have been validated.
func (m *Model) EndpointHostPort() (scheme, host string, port int) {
	u, err := url.Parse(m.Endpoint)
	if err != nil {
		return "", "", 0
	}
	port = 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	return u.Scheme, NormalizeHost(u.Hostname()), port
}

// Trigger decides when a harness runs. With neither field set, it runs
// only when started manually.
type Trigger struct {
	// Schedule is a standard 5-field cron expression.
	Schedule string `yaml:"schedule,omitempty"`
	Loop     *Loop  `yaml:"loop,omitempty"`
}

// Loop re-runs the agent until it reports completion or a limit is hit.
type Loop struct {
	MaxIterations int      `yaml:"maxIterations"`
	Interval      Duration `yaml:"interval,omitempty"`
}

// Sandbox describes the isolated environment the agent runs in.
type Sandbox struct {
	Image     string `yaml:"image,omitempty"`
	CPUs      int    `yaml:"cpus,omitempty"`
	MemoryMiB int    `yaml:"memoryMiB,omitempty"`
	// DiskMiB bounds the files the agent can hand back (see ADR-0006).
	DiskMiB int      `yaml:"diskMiB,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty"`
}

// Permissions lists everything the agent is granted beyond an empty,
// offline sandbox.
type Permissions struct {
	Network Network `yaml:"network,omitempty"`
	// Credentials are names of secrets stored in the Control Center. They
	// are injected by the egress proxy and never enter the sandbox.
	Credentials []string  `yaml:"credentials,omitempty"`
	Tools       Tools     `yaml:"tools,omitempty"`
	Subagents   Subagents `yaml:"subagents,omitempty"`
}

// Network controls egress. Ingress is never allowed.
type Network struct {
	Egress []EgressRule `yaml:"egress,omitempty"`
	// AllowPrivateNetworks permits internal names (e.g. "gitlab.lan") and,
	// at the proxy, private resolved addresses. Off by default because it
	// opens the user's own network to the agent.
	AllowPrivateNetworks bool `yaml:"allowPrivateNetworks,omitempty"`
}

// EgressRule allows connections to Host on the given Ports.
// Host is a DNS name or a single leading wildcard ("*.example.com").
type EgressRule struct {
	Host  string `yaml:"host"`
	Ports []int  `yaml:"ports,omitempty"`
}

// Tools lists packages the agent may install inside the sandbox.
type Tools struct {
	Install []string `yaml:"install,omitempty"`
}

// Subagents controls whether the agent may spawn children. Children always
// receive a subset of the parent's permissions and share its budget.
type Subagents struct {
	Max int `yaml:"max,omitempty"`
}

// Budget caps resource spend. It covers the whole subagent tree.
type Budget struct {
	MaxCostUSD float64 `yaml:"maxCostUSD,omitempty"`
	MaxTokens  int64   `yaml:"maxTokens,omitempty"`
}

// Approvals lists actions that pause the agent until a human approves.
type Approvals struct {
	RequireFor []string `yaml:"requireFor,omitempty"`
}

// Duration is a time.Duration that (un)marshals as a Go duration string
// such as "30m" or "2h".
type Duration struct {
	time.Duration
}
