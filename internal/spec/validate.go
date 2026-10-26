package spec

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/cron"
)

var (
	nameRe       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	labelRe      = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	imageRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}(:[A-Za-z0-9._-]{1,128})?$`)
	packageRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,127}(@[A-Za-z0-9._-]{1,64})?$`)
	actionRe     = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*(\.\*)?$`)
	privateNames = []string{".localhost", ".local", ".internal", ".lan", ".home.arpa", ".corp", ".fritz.box"}
)

// FieldError reports a problem with one field, addressed by its YAML path.
type FieldError struct {
	Path string
	Msg  string
}

func (e *FieldError) Error() string { return e.Path + ": " + e.Msg }

// Validate checks every invariant of a defaulted harness and returns all
// problems at once (joined), so users can fix them in a single pass.
func (h *Harness) Validate() error {
	var errs []error
	add := func(path, format string, args ...any) {
		errs = append(errs, &FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
	}

	if h.APIVersion != APIVersion {
		add("apiVersion", "must be %q, got %q", APIVersion, h.APIVersion)
	}
	if h.Kind != Kind {
		add("kind", "must be %q, got %q", Kind, h.Kind)
	}
	if !nameRe.MatchString(h.Metadata.Name) {
		add("metadata.name", "must be 1-63 lowercase letters, digits or '-', starting and ending with a letter or digit")
	}

	s := &h.Spec
	if !nameRe.MatchString(s.Runtime) {
		add("spec.runtime", "must name an agent runtime, e.g. \"claude-code\"")
	}
	if strings.TrimSpace(s.Prompt) == "" {
		add("spec.prompt", "must not be empty")
	} else if len(s.Prompt) > MaxPromptBytes {
		add("spec.prompt", "must be at most %d bytes", MaxPromptBytes)
	}
	validateTarget(s.Target, add)
	validateTrigger(&s.Trigger, add)
	validateSandbox(&s.Sandbox, add)
	validatePermissions(&s.Permissions, add)
	if s.Model != nil {
		validateModel(s.Model, &s.Permissions, add)
	}
	validateBudget(&s.Budget, add)
	for i, a := range s.Approvals.RequireFor {
		if !actionRe.MatchString(a) {
			add(fmt.Sprintf("spec.approvals.requireFor[%d]", i), "invalid action %q (expected e.g. \"git.push\" or \"git.*\")", a)
		}
	}
	return errors.Join(errs...)
}

type addFunc func(path, format string, args ...any)

func validateTarget(t string, add addFunc) {
	if t == "local" {
		return
	}
	name, ok := strings.CutPrefix(t, "server:")
	if !ok || !nameRe.MatchString(name) {
		add("spec.target", "must be \"local\" or \"server:<name>\", got %q", t)
	}
}

func validateTrigger(t *Trigger, add addFunc) {
	if t.Schedule != "" && t.Loop != nil {
		add("spec.trigger", "schedule and loop are mutually exclusive")
	}
	if t.Schedule != "" {
		if _, err := cron.Parse(t.Schedule); err != nil {
			add("spec.trigger.schedule", "must be a 5-field cron expression in UTC, e.g. \"0 2 * * *\": %v", err)
		}
	}
	if t.Loop != nil {
		if t.Loop.MaxIterations < 1 || t.Loop.MaxIterations > MaxLoopIteration {
			add("spec.trigger.loop.maxIterations", "must be between 1 and %d", MaxLoopIteration)
		}
		if t.Loop.Interval.Duration < 0 || t.Loop.Interval.Duration > MaxLoopInterval {
			add("spec.trigger.loop.interval", "must be between 0 and %s", MaxLoopInterval)
		}
	}
}

func validateSandbox(sb *Sandbox, add addFunc) {
	// ".." is rejected because providers may map image names to paths.
	if !imageRe.MatchString(sb.Image) || strings.Contains(sb.Image, "..") {
		add("spec.sandbox.image", "invalid image reference %q", sb.Image)
	}
	checkRange(add, "spec.sandbox.cpus", sb.CPUs, 1, MaxCPUs)
	checkRange(add, "spec.sandbox.memoryMiB", sb.MemoryMiB, 128, MaxMemoryMiB)
	checkRange(add, "spec.sandbox.diskMiB", sb.DiskMiB, 256, MaxDiskMiB)
	if sb.Timeout.Duration <= 0 || sb.Timeout.Duration > MaxTimeout {
		add("spec.sandbox.timeout", "must be greater than 0 and at most %s", MaxTimeout)
	}
}

func validatePermissions(p *Permissions, add addFunc) {
	if len(p.Network.Egress) > MaxEgressRules {
		add("spec.permissions.network.egress", "at most %d rules allowed", MaxEgressRules)
	}
	for i, r := range p.Network.Egress {
		path := fmt.Sprintf("spec.permissions.network.egress[%d]", i)
		if err := ValidateEgressHost(r.Host, p.Network.AllowPrivateNetworks); err != nil {
			add(path+".host", "%v", err)
		}
		for j, port := range r.Ports {
			checkRange(add, fmt.Sprintf("%s.ports[%d]", path, j), port, 1, 65535)
		}
	}
	checkUnique(add, "spec.permissions.credentials", p.Credentials, nameRe, "a credential name")
	checkUnique(add, "spec.permissions.tools.install", p.Tools.Install, packageRe, "a package name, optionally with @version")
	checkRange(add, "spec.permissions.subagents.max", p.Subagents.Max, 0, MaxSubagents)
}

var (
	modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	urlPathRe   = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)*/?$`)
)

func validateModel(m *Model, p *Permissions, add addFunc) {
	if !modelNameRe.MatchString(m.Name) {
		add("spec.model.name", "invalid model name %q", m.Name)
	}
	u, err := url.Parse(m.Endpoint)
	switch {
	case err != nil:
		add("spec.model.endpoint", "invalid URL: %v", err)
		return
	case u.Scheme != "https" && u.Scheme != "http":
		add("spec.model.endpoint", "must be an http:// or https:// URL")
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		add("spec.model.endpoint", "must not contain credentials, a query or a fragment")
	case !urlPathRe.MatchString(u.EscapedPath()):
		add("spec.model.endpoint", "invalid path %q", u.Path)
	}
	if err := ValidateEgressHost(NormalizeHost(u.Hostname()), p.Network.AllowPrivateNetworks); err != nil {
		add("spec.model.endpoint", "%v", err)
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			add("spec.model.endpoint", "invalid port %q", port)
		}
	}
	if m.Credential != "" && !slices.Contains(p.Credentials, m.Credential) {
		add("spec.model.credential", "%q is not in permissions.credentials", m.Credential)
	}
}

func validateBudget(b *Budget, add addFunc) {
	if math.IsNaN(b.MaxCostUSD) || b.MaxCostUSD <= 0 || b.MaxCostUSD > MaxCostUSD {
		add("spec.budget.maxCostUSD", "must be greater than 0 and at most %.0f", MaxCostUSD)
	}
	if b.MaxTokens <= 0 || b.MaxTokens > MaxTokens {
		add("spec.budget.maxTokens", "must be greater than 0 and at most %d", MaxTokens)
	}
}

func checkRange(add addFunc, path string, v, lo, hi int) {
	if v < lo || v > hi {
		add(path, "must be between %d and %d, got %d", lo, hi, v)
	}
}

func checkUnique(add addFunc, path string, items []string, re *regexp.Regexp, what string) {
	seen := make(map[string]bool, len(items))
	for i, it := range items {
		p := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case !re.MatchString(it):
			add(p, "%q is not %s", it, what)
		case seen[it]:
			add(p, "duplicate %q", it)
		}
		seen[it] = true
	}
}

// NormalizeHost lowercases a host name and strips one trailing dot, so
// "GitLab.com." and "gitlab.com" are treated as the same host.
func NormalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// ValidateEgressHost checks that host is a fully qualified DNS name, or a
// single leading wildcard over at least two labels ("*.example.com").
// IP literals are always rejected: rules are names so they can be
// reviewed, and the egress proxy re-checks the resolved addresses.
// Names that are internal by convention are rejected unless
// allowPrivate is set.
func ValidateEgressHost(host string, allowPrivate bool) error {
	if host != NormalizeHost(host) {
		return fmt.Errorf("host %q must be lowercase without a trailing dot", host)
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return fmt.Errorf("IP addresses are not allowed, use a host name")
	}
	name, wildcard := strings.CutPrefix(host, "*.")
	if err := ValidateHostName(name); err != nil {
		if wildcard && !strings.Contains(name, ".") {
			return fmt.Errorf("wildcard %q is too broad, use e.g. \"*.example.com\"", host)
		}
		return err
	}
	if !allowPrivate && isPrivateName(name) {
		return fmt.Errorf("host %q looks internal; set network.allowPrivateNetworks to allow it", host)
	}
	return nil
}

// ValidateHostName checks that name is a normalized, fully qualified DNS
// name made only of valid labels: no wildcards, no IP literals, and none
// of the characters ('/', '#', '@', ':', NUL ...) that URL or socket
// parsers might interpret differently from the policy engine.
func ValidateHostName(name string) error {
	if _, err := netip.ParseAddr(strings.Trim(name, "[]")); err == nil {
		return fmt.Errorf("host %q is an IP address, not a name", name)
	}
	if len(name) == 0 || len(name) > 253 {
		return fmt.Errorf("host %q has an invalid length", name)
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return fmt.Errorf("host %q must be a fully qualified name", name)
	}
	for _, l := range labels {
		if !labelRe.MatchString(l) {
			return fmt.Errorf("host %q contains an invalid label %q", name, l)
		}
	}
	return nil
}

// ValidName reports whether s is a valid name (metadata.name rules).
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidPackage reports whether pkg is a well-formed package reference
// ("name" or "name@version"), using the same rules as the spec.
func ValidPackage(pkg string) bool {
	return packageRe.MatchString(pkg)
}

func isPrivateName(name string) bool {
	if name == "localhost" {
		return true
	}
	for _, suffix := range privateNames {
		if strings.HasSuffix("."+name, suffix) {
			return true
		}
	}
	return false
}
