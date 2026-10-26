package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"
)

// Subagent is a running agent's request for a helper agent (ADR-0011):
// the helper's task and the part of the parent's permissions it needs.
// Everything else comes from the parent (see Child).
type Subagent struct {
	// Name labels the helper (metadata.name rules; default "subagent").
	Name        string          `yaml:"name,omitempty"`
	Prompt      string          `yaml:"prompt"`
	Sandbox     SubagentSandbox `yaml:"sandbox,omitempty"`
	Permissions Permissions     `yaml:"permissions,omitempty"`
}

// SubagentSandbox sizes a helper's sandbox; omitted fields are the
// parent's. The image is always the parent's.
type SubagentSandbox struct {
	CPUs      int      `yaml:"cpus,omitempty"`
	MemoryMiB int      `yaml:"memoryMiB,omitempty"`
	DiskMiB   int      `yaml:"diskMiB,omitempty"`
	Timeout   Duration `yaml:"timeout,omitempty"`
}

// MaxSubagentBytes bounds a subagent request.
const MaxSubagentBytes = MaxPromptBytes + 64<<10

// DefaultSubagentName names helpers that do not name themselves.
const DefaultSubagentName = "subagent"

// ParseSubagent decodes a request strictly (JSON or YAML; unknown fields
// are errors). It is not validated yet: Child does that.
func ParseSubagent(data []byte) (*Subagent, error) {
	if len(data) > MaxSubagentBytes {
		return nil, fmt.Errorf("spec: subagent request exceeds %d bytes", MaxSubagentBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var s Subagent
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("spec: empty subagent request")
		}
		return nil, fmt.Errorf("spec: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("spec: multiple documents are not allowed")
	}
	return &s, nil
}

// Child builds the harness a subagent runs with. It gets the parent's
// runtime, model, target, image, budget (which it shares) and approval
// gates, its own prompt and the permissions it asked for, plus what it
// needs to reach the model. Its sandbox may not exceed the parent's, nor
// its time limit maxTimeout (the parent's remaining time). The result is
// defaulted and validated like any parsed harness; whether its
// permissions are a subset of the parent's is the policy's decision
// (policy.Engine.CheckChild).
func Child(parent *Harness, req *Subagent, maxTimeout time.Duration) (*Harness, error) {
	ps := parent.Spec
	name := req.Name
	if name == "" {
		name = DefaultSubagentName
	}
	sb := Sandbox{
		Image:     ps.Sandbox.Image,
		CPUs:      orDefault(req.Sandbox.CPUs, ps.Sandbox.CPUs),
		MemoryMiB: orDefault(req.Sandbox.MemoryMiB, ps.Sandbox.MemoryMiB),
		DiskMiB:   orDefault(req.Sandbox.DiskMiB, ps.Sandbox.DiskMiB),
		Timeout:   req.Sandbox.Timeout,
	}
	limit := min(ps.Sandbox.Timeout.Duration, maxTimeout).Truncate(time.Second)
	if sb.Timeout.Duration == 0 {
		sb.Timeout.Duration = limit
	}
	var errs []error
	if sb.CPUs > ps.Sandbox.CPUs {
		errs = append(errs, fmt.Errorf("sandbox.cpus %d exceeds the parent's %d", sb.CPUs, ps.Sandbox.CPUs))
	}
	if sb.MemoryMiB > ps.Sandbox.MemoryMiB {
		errs = append(errs, fmt.Errorf("sandbox.memoryMiB %d exceeds the parent's %d", sb.MemoryMiB, ps.Sandbox.MemoryMiB))
	}
	if sb.DiskMiB > ps.Sandbox.DiskMiB {
		errs = append(errs, fmt.Errorf("sandbox.diskMiB %d exceeds the parent's %d", sb.DiskMiB, ps.Sandbox.DiskMiB))
	}
	if limit < time.Minute {
		errs = append(errs, errors.New("the parent has less than a minute left"))
	} else if sb.Timeout.Duration > limit {
		errs = append(errs, fmt.Errorf("sandbox.timeout %s exceeds the parent's remaining time %s", sb.Timeout, limit))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}

	perms := req.Permissions
	perms.Network.Egress = slices.Clone(perms.Network.Egress)
	perms.Credentials = slices.Clone(perms.Credentials)
	var model *Model
	if ps.Model != nil {
		m := *ps.Model
		model = &m
		withModelAccess(&perms, &m, ps.Permissions.Network.AllowPrivateNetworks)
	}
	child := &Harness{
		APIVersion: parent.APIVersion,
		Kind:       parent.Kind,
		Metadata:   Metadata{Name: name, Description: "Helper agent of " + parent.Metadata.Name},
		Spec: Spec{
			Runtime:     ps.Runtime,
			Prompt:      req.Prompt,
			Model:       model,
			Target:      ps.Target,
			Sandbox:     sb,
			Permissions: perms,
			Budget:      ps.Budget,
			Approvals:   Approvals{RequireFor: slices.Clone(ps.Approvals.RequireFor)},
		},
	}
	// The one path every harness takes: strict parsing, defaults,
	// validation.
	b, err := yaml.Marshal(child)
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return Parse(b)
}

// withModelAccess adds what reaching the model needs: its host and port,
// its credential and, for an internal host, private networks (only if
// the parent has them, so the policy can still refuse nothing the parent
// lacks).
func withModelAccess(p *Permissions, m *Model, parentPrivate bool) {
	_, host, port := m.EndpointHostPort()
	if !slices.ContainsFunc(p.Network.Egress, func(r EgressRule) bool {
		return NormalizeHost(r.Host) == host && (slices.Contains(r.Ports, port) || len(r.Ports) == 0 && port == DefaultEgressPort)
	}) {
		p.Network.Egress = append(p.Network.Egress, EgressRule{Host: host, Ports: []int{port}})
	}
	if m.Credential != "" && !slices.Contains(p.Credentials, m.Credential) {
		p.Credentials = append(p.Credentials, m.Credential)
	}
	if parentPrivate && ValidateEgressHost(host, false) != nil {
		p.Network.AllowPrivateNetworks = true
	}
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
