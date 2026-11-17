package control

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Network policies: named sets of hosts an agent may reach ("GitHub",
// "Python packages"), defined once and attached to harnesses. Attaching
// copies the policy's hosts into the harness's egress allowlist, so a
// harness stays complete on its own and every runner enforces exactly
// what it says; a harness "has" a policy when all of the policy's hosts
// are in its allowlist.

// Bounds.
const (
	MaxPolicies          = 64
	maxPolicyDescription = 200
)

// PolicyHost is a host a policy allows, on its ports (none: 443).
type PolicyHost struct {
	Host  string `yaml:"host" json:"host"`
	Ports []int  `yaml:"ports,omitempty" json:"ports,omitempty"`
}

// Policy is one named set of hosts.
type Policy struct {
	Name        string       `yaml:"name" json:"name"`
	Description string       `yaml:"description,omitempty" json:"description,omitempty"`
	Hosts       []PolicyHost `yaml:"hosts" json:"hosts"`
	// Private: the hosts are in the person's own network (a harness with
	// the policy then needs network.allowPrivateNetworks).
	Private bool `yaml:"private,omitempty" json:"private,omitempty"`
}

// Policies is policies.yaml.
type Policies struct {
	Policies []Policy `yaml:"policies" json:"policies"`
}

// Validate checks every policy's hosts as a harness's egress rules are
// checked.
func (ps *Policies) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if len(ps.Policies) > MaxPolicies {
		add("at most %d policies", MaxPolicies)
	}
	seen := map[string]bool{}
	for _, p := range ps.Policies {
		what := "policy " + p.Name
		switch {
		case !nameRe.MatchString(p.Name):
			add("policy name %q must be 1-63 lowercase letters, digits or '-'", p.Name)
		case seen[p.Name]:
			add("%s: there is another policy with that name", what)
		}
		seen[p.Name] = true
		if len(p.Description) > maxPolicyDescription || strings.ContainsAny(p.Description, "\r\n") {
			add("%s: the description must be one line of at most %d characters", what, maxPolicyDescription)
		}
		if len(p.Hosts) == 0 {
			add("%s: it needs at least one host", what)
		}
		if len(p.Hosts) > spec.MaxEgressRules {
			add("%s: at most %d hosts", what, spec.MaxEgressRules)
		}
		for _, r := range p.Hosts {
			if err := spec.ValidateEgressHost(r.Host, p.Private); err != nil {
				add("%s: %v", what, err)
			}
			for _, port := range r.Ports {
				if port < 1 || port > 65535 {
					add("%s: %s: port %d must be between 1 and 65535", what, r.Host, port)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (s *Server) policiesPath() string {
	return filepath.Join(filepath.Dir(s.cfgPath), "policies.yaml")
}

// LoadPolicies reads a policies file; a missing one is empty.
func LoadPolicies(path string) (*Policies, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own file
	if errors.Is(err, os.ErrNotExist) {
		return &Policies{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ps Policies
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&ps); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := ps.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &ps, nil
}

// PolicyView is a policy as the UI shows it.
type PolicyView struct {
	Policy
	// UsedBy are the saved harnesses that have all of its hosts.
	UsedBy []string `json:"usedBy"`
}

func (s *Server) policiesGet(w http.ResponseWriter, _ *http.Request) {
	s.modelsMu.Lock()
	ps, err := LoadPolicies(s.policiesPath())
	s.modelsMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": s.policyViews(ps)})
}

func (s *Server) policiesPut(w http.ResponseWriter, r *http.Request) {
	var ps Policies
	if !readJSON(w, r, &ps) {
		return
	}
	for i := range ps.Policies {
		for j := range ps.Policies[i].Hosts {
			ps.Policies[i].Hosts[j].Host = spec.NormalizeHost(ps.Policies[i].Hosts[j].Host)
		}
	}
	if err := ps.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the policies are not valid", "errors": splitLines(err)})
		return
	}
	data, err := yaml.Marshal(&ps)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.modelsMu.Lock()
	err = writeFileAtomic(s.policiesPath(), data)
	s.modelsMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("network policies saved", "policies", len(ps.Policies))
	writeJSON(w, http.StatusOK, map[string]any{"policies": s.policyViews(&ps)})
}

// Covers reports whether egress allows all of p's hosts (on their ports).
func (p Policy) Covers(egress []spec.EgressRule) bool {
	for _, want := range p.Hosts {
		if !slices.ContainsFunc(egress, func(r spec.EgressRule) bool { return r.Host == want.Host && samePorts(r.Ports, want.Ports) }) {
			return false
		}
	}
	return true
}

// samePorts compares port lists, none meaning 443.
func samePorts(a, b []int) bool {
	norm := func(p []int) []int {
		if len(p) == 0 {
			return []int{spec.DefaultEgressPort}
		}
		out := slices.Clone(p)
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(norm(a), norm(b))
}

func (s *Server) policyViews(ps *Policies) []PolicyView {
	out := make([]PolicyView, len(ps.Policies))
	for i, p := range ps.Policies {
		out[i] = PolicyView{Policy: p, UsedBy: []string{}}
	}
	entries, err := s.lib.List()
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.Error != "" {
			continue
		}
		data, err := s.lib.Get(e.Name)
		if err != nil {
			continue
		}
		h, err := spec.Parse(data)
		if err != nil {
			continue
		}
		for i := range out {
			if out[i].Covers(h.Spec.Permissions.Network.Egress) {
				out[i].UsedBy = append(out[i].UsedBy, e.Name)
			}
		}
	}
	return out
}
