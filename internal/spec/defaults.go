package spec

import "time"

// Defaults applied when a field is omitted. They are deliberately small:
// a newcomer who writes only a prompt gets a short, cheap, offline run.
const (
	DefaultTarget     = "local"
	DefaultImage      = "base"
	DefaultCPUs       = 1
	DefaultMemoryMiB  = 1024
	DefaultDiskMiB    = 4096
	DefaultTimeout    = 30 * time.Minute
	DefaultMaxCostUSD = 1.0
	DefaultMaxTokens  = 1_000_000
	DefaultEgressPort = 443
)

// Hard ceilings. A harness exceeding them is rejected rather than clamped,
// so the user always sees exactly what will run.
const (
	MaxCPUs          = 16
	MaxMemoryMiB     = 64 * 1024
	MaxDiskMiB       = 256 * 1024
	MaxTimeout       = 24 * time.Hour
	MaxCostUSD       = 1000.0
	MaxTokens        = 1_000_000_000
	MaxSubagents     = 16
	MaxLoopIteration = 1000
	MaxLoopInterval  = 7 * 24 * time.Hour
	MaxPromptBytes   = 256 * 1024
	MaxEgressRules   = 64
	MaxMCPServers    = 16
	// Bounds of spec.model.contextWindow, in tokens.
	MinContextWindow = 8 * 1024
	MaxContextWindow = 1_000_000
	MaxA2AAgents     = 16
)

// ApplyDefaults fills omitted fields in place. It never widens a
// permission: every permission default is "none".
func (h *Harness) ApplyDefaults() {
	s := &h.Spec
	if s.Target == "" {
		s.Target = DefaultTarget
	}
	if s.Sandbox.Image == "" {
		s.Sandbox.Image = DefaultImage
	}
	if s.Sandbox.CPUs == 0 {
		s.Sandbox.CPUs = DefaultCPUs
	}
	if s.Sandbox.MemoryMiB == 0 {
		s.Sandbox.MemoryMiB = DefaultMemoryMiB
	}
	if s.Sandbox.DiskMiB == 0 {
		s.Sandbox.DiskMiB = DefaultDiskMiB
	}
	if s.Sandbox.Timeout.Duration == 0 {
		s.Sandbox.Timeout.Duration = DefaultTimeout
	}
	if s.Budget.MaxCostUSD == 0 {
		s.Budget.MaxCostUSD = DefaultMaxCostUSD
	}
	if s.Budget.MaxTokens == 0 {
		s.Budget.MaxTokens = DefaultMaxTokens
	}
	for i := range s.Permissions.Network.Egress {
		r := &s.Permissions.Network.Egress[i]
		r.Host = NormalizeHost(r.Host)
		if len(r.Ports) == 0 {
			r.Ports = []int{DefaultEgressPort}
		}
	}
}
