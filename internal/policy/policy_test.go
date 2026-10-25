package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

const base = `
apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: test}
spec:
  runtime: claude-code
  prompt: do things
`

const granted = base + `
  permissions:
    network:
      egress:
        - host: gitlab.com
        - host: "*.npmjs.org"
          ports: [443, 80]
    credentials: [gitlab-main]
    tools:
      install: [ripgrep, node@22]
    subagents: {max: 2}
  approvals:
    requireFor: [git.push, delete.*]
`

func mustEngine(t *testing.T, doc string) *Engine {
	t.Helper()
	h, err := spec.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	e, err := New(h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestNewRejectsInvalid(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Error("New(nil) succeeded")
	}
	if _, err := New(&spec.Harness{}); err == nil {
		t.Error("New(unvalidated empty harness) succeeded")
	}
}

func TestDefaultDeniesEverything(t *testing.T) {
	e := mustEngine(t, base)
	checks := map[string]Decision{
		"egress":     e.CheckEgress("example.com", 443),
		"credential": e.CheckCredential("gitlab-main"),
		"tool":       e.CheckToolInstall("ripgrep"),
		"subagent":   e.CheckSpawnSubagent(0),
	}
	for name, d := range checks {
		if d.Allowed {
			t.Errorf("%s allowed by default: %s", name, d.Reason)
		}
		if d.Reason == "" {
			t.Errorf("%s: empty reason", name)
		}
	}
	if risk, _ := e.Risk(); risk != RiskLow {
		t.Errorf("Risk = %v, want low", risk)
	}
}

func TestCheckEgress(t *testing.T) {
	e := mustEngine(t, granted)
	tests := []struct {
		host string
		port int
		want bool
	}{
		{"gitlab.com", 443, true},
		{"GITLAB.com.", 443, true},
		{"gitlab.com", 80, false},
		{"gitlab.com", 22, false},
		{"evil.gitlab.com", 443, false}, // exact rule does not cover subdomains
		{"registry.npmjs.org", 443, true},
		{"registry.npmjs.org", 80, true},
		{"a.b.npmjs.org", 443, true},
		{"npmjs.org", 443, false},     // wildcard does not cover the apex
		{"evilnpmjs.org", 443, false}, // suffix must be on a label boundary
		{"npmjs.org.evil.com", 443, false},
		{"140.82.112.3", 443, false},
		{"[::1]", 443, false},
		{"169.254.169.254", 80, false},
		{"", 443, false},
		// Parser differentials: these suffix-match "*.npmjs.org" but a URL
		// or socket parser could resolve them to a different host.
		{"evil.com#.npmjs.org", 443, false},
		{"evil.com/.npmjs.org", 443, false},
		{"evil.com?.npmjs.org", 443, false},
		{"user@evil.com.npmjs.org", 443, false},
		{"evil.com:443.npmjs.org", 443, false},
		{"evil.com\x00.npmjs.org", 443, false},
		{"evil com.npmjs.org", 443, false},
		{"evil..npmjs.org", 443, false},
		{"*.npmjs.org", 443, false},
		{"gitlab.com:443", 443, false},
	}
	for _, tt := range tests {
		if got := e.CheckEgress(tt.host, tt.port); got.Allowed != tt.want {
			t.Errorf("CheckEgress(%q, %d) = %+v, want allowed=%v", tt.host, tt.port, got, tt.want)
		}
	}
}

func TestCheckCredentialAndTools(t *testing.T) {
	e := mustEngine(t, granted)
	if !e.CheckCredential("gitlab-main").Allowed || e.CheckCredential("github-main").Allowed {
		t.Error("credential checks wrong")
	}
	tools := map[string]bool{
		"ripgrep":    true,
		"ripgrep@14": true, // unpinned grant allows any version
		"node@22":    true,
		"node":       false, // pinned grant does not allow unpinned
		"node@23":    false,
		"curl":       false,
		// Malformed references are denied even when the name is granted.
		"ripgrep@14;rm -rf /": false,
		"ripgrep@$(id)":       false,
		"ripgrep@":            false,
		"ripgrep@14@15":       false,
		"ripgrep ":            false,
		"":                    false,
	}
	for pkg, want := range tools {
		if got := e.CheckToolInstall(pkg).Allowed; got != want {
			t.Errorf("CheckToolInstall(%q) = %v, want %v", pkg, got, want)
		}
	}
}

func TestCheckSpawnSubagent(t *testing.T) {
	e := mustEngine(t, granted)
	for running, want := range []bool{true, true, false, false} {
		if got := e.CheckSpawnSubagent(running).Allowed; got != want {
			t.Errorf("CheckSpawnSubagent(%d) = %v, want %v", running, got, want)
		}
	}
}

func TestRequiresApproval(t *testing.T) {
	e := mustEngine(t, granted)
	tests := map[string]bool{
		"git.push":        true,
		"git.push.force":  false,
		"git.commit":      false,
		"delete":          true,
		"delete.file":     true,
		"delete.repo.all": true,
		"deleted.file":    false,
	}
	for action, want := range tests {
		if got := e.RequiresApproval(action); got != want {
			t.Errorf("RequiresApproval(%q) = %v, want %v", action, got, want)
		}
	}
}

func TestCheckChildCanOnlyNarrow(t *testing.T) {
	e := mustEngine(t, granted)
	egress := func(host string, ports ...int) spec.Permissions {
		return spec.Permissions{Network: spec.Network{Egress: []spec.EgressRule{{Host: host, Ports: ports}}}}
	}
	tests := []struct {
		name  string
		child spec.Permissions
		want  bool
	}{
		{"nothing", spec.Permissions{}, true},
		{"same exact host", egress("gitlab.com", 443), true},
		{"default port", egress("gitlab.com"), true},
		{"other port", egress("gitlab.com", 80), false},
		{"host under wildcard", egress("registry.npmjs.org", 80), true},
		{"same wildcard", egress("*.npmjs.org", 443), true},
		{"narrower wildcard", egress("*.cdn.npmjs.org", 443), true},
		{"wildcard over exact", egress("*.gitlab.com", 443), false},
		{"other host", egress("example.com", 443), false},
		{"invalid host", egress("*", 443), false},
		{"private networks", spec.Permissions{Network: spec.Network{AllowPrivateNetworks: true}}, false},
		{"granted credential", spec.Permissions{Credentials: []string{"gitlab-main"}}, true},
		{"other credential", spec.Permissions{Credentials: []string{"aws-root"}}, false},
		{"granted tool", spec.Permissions{Tools: spec.Tools{Install: []string{"node@22"}}}, true},
		{"other tool", spec.Permissions{Tools: spec.Tools{Install: []string{"curl"}}}, false},
		{"fewer subagents", spec.Permissions{Subagents: spec.Subagents{Max: 1}}, true},
		{"more subagents", spec.Permissions{Subagents: spec.Subagents{Max: 3}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := e.CheckChild(tt.child); got.Allowed != tt.want {
				t.Errorf("CheckChild = %+v, want allowed=%v", got, tt.want)
			}
		})
	}
}

func TestRisk(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want Risk
	}{
		{"offline", base, RiskLow},
		{"network only", base + "  permissions: {network: {egress: [{host: a.com}]}}\n", RiskMedium},
		{"credentials only", base + "  permissions: {credentials: [x]}\n", RiskMedium},
		{"network and credentials", base + "  permissions: {network: {egress: [{host: a.com}]}, credentials: [x]}\n", RiskHigh},
		{"private networks", base + "  permissions: {network: {allowPrivateNetworks: true, egress: [{host: nas.lan}]}}\n", RiskHigh},
		{"everything", granted, RiskHigh},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, why := mustEngine(t, tt.doc).Risk()
			if got != tt.want {
				t.Errorf("Risk = %v (%v), want %v", got, why, tt.want)
			}
			if got != RiskLow && len(why) == 0 {
				t.Error("non-low risk without reasons")
			}
		})
	}
}

func TestExplain(t *testing.T) {
	offline := strings.Join(mustEngine(t, base).Explain(), "\n")
	for _, want := range []string{"this computer", "no network access", "no credentials", "Cannot start other agents", "$1.00", "1,000,000 tokens", "only when you start it", "hand back up to 4,096 MiB of files"} {
		if !strings.Contains(offline, want) {
			t.Errorf("offline explanation missing %q:\n%s", want, offline)
		}
	}
	full := strings.Join(mustEngine(t, granted).Explain(), "\n")
	for _, want := range []string{"gitlab.com,", "any subdomain of npmjs.org (ports 443, 80)", "gitlab-main", "never sees these secrets", "up to 2 helper agents", "Asks you before: git.push, delete.*"} {
		if !strings.Contains(full, want) {
			t.Errorf("full explanation missing %q:\n%s", want, full)
		}
	}
}

func TestGroupDigits(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := groupDigits(in); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", in, got, want)
		}
	}
}

// FuzzCheckEgress asserts the core wildcard property: an allowed host is
// always either an exact rule or a strict subdomain of a wildcard rule.
func FuzzCheckEgress(f *testing.F) {
	e, err := New(mustParse(f, granted))
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{"gitlab.com", "x.npmjs.org", "npmjs.org", "evilnpmjs.org", "1.1.1.1", ""} {
		f.Add(s, 443)
	}
	f.Fuzz(func(t *testing.T, host string, port int) {
		d := e.CheckEgress(host, port)
		if !d.Allowed {
			return
		}
		h := spec.NormalizeHost(host)
		if err := spec.ValidateHostName(h); err != nil {
			t.Fatalf("allowed host %q that is not a plain DNS name: %v", host, err)
		}
		if h != "gitlab.com" && !strings.HasSuffix(h, ".npmjs.org") {
			t.Fatalf("allowed unexpected host %q (%q): %s", host, h, d.Reason)
		}
		if port != 443 && port != 80 {
			t.Fatalf("allowed unexpected port %d", port)
		}
	})
}

func mustParse(tb testing.TB, doc string) *spec.Harness {
	tb.Helper()
	h, err := spec.Parse([]byte(doc))
	if err != nil {
		tb.Fatal(err)
	}
	return h
}

const modelHarness = base + `
  model:
    name: Ornith-1.5-35B-A3B-MLX
    endpoint: http://llm.example:4000
    credential: llm
  permissions:
    credentials: [llm]
    network:
      allowPrivateNetworks: true
      egress:
        - host: llm.example
          ports: [4000]
`

func TestModelEndpointMustBeAllowed(t *testing.T) {
	e := mustEngine(t, modelHarness)
	risk, why := e.Risk()
	if risk != RiskHigh || !slices.Contains(why, "talks to its model unencrypted (http)") {
		t.Errorf("risk %v %v", risk, why)
	}
	explained := strings.Join(e.Explain(), "\n")
	for _, want := range []string{"Uses the model Ornith-1.5-35B-A3B-MLX at http://llm.example:4000, authenticated with llm.", "UNENCRYPTED"} {
		if !strings.Contains(explained, want) {
			t.Errorf("explanation missing %q:\n%s", want, explained)
		}
	}
	notAllowed := strings.Replace(modelHarness, "ports: [4000]", "ports: [443]", 1)
	h, err := spec.Parse([]byte(notAllowed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(h); err == nil || !strings.Contains(err.Error(), "not in the egress allowlist") {
		t.Errorf("New with unreachable model endpoint = %v", err)
	}
}

func TestExplainTriggers(t *testing.T) {
	e := mustEngine(t, base+"  trigger: {loop: {maxIterations: 4, interval: 10m}}\n  budget: {maxTokens: 1000, maxCostUSD: 2}\n")
	text := strings.Join(e.Explain(), "\n")
	for _, want := range []string{"up to 4 times in a row (10m0s apart)", "4,000 tokens (or $8.00)", "first run that does not succeed"} {
		if !strings.Contains(text, want) {
			t.Errorf("loop explanation missing %q:\n%s", want, text)
		}
	}
	if risk, why := e.Risk(); risk != RiskMedium || !slices.Contains(why, "runs again and again without anyone watching") {
		t.Errorf("loop risk = %s %v", risk, why)
	}
	e = mustEngine(t, base+"  trigger: {schedule: '0 2 * * *'}\n")
	if text := strings.Join(e.Explain(), "\n"); !strings.Contains(text, `cron schedule "0 2 * * *" (times in UTC)`) || !strings.Contains(text, "skipped while the previous one") {
		t.Errorf("schedule explanation:\n%s", text)
	}
}
