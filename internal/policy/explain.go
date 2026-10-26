package policy

import (
	"fmt"
	"strconv"
	"strings"
)

// Risk is a coarse, explainable rating shown to the user before a
// harness starts. It is a UX aid, not a security boundary.
type Risk int

// Risk levels, from least to most dangerous.
const (
	RiskLow Risk = iota
	RiskMedium
	RiskHigh
)

func (r Risk) String() string {
	switch r {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	default:
		return "high"
	}
}

// Risk scores the harness's permissions. The rules are intentionally
// simple so the UI can show why a harness got its rating:
//
//	+1 any network egress     +1 any wildcard egress
//	+1 any credential         +1 network AND credentials (exfiltration path)
//	+1 any subagents          +2 private networks
//	+1 http model endpoint    +1 runs unattended (schedule or loop)
//
// 0 is low, 1-2 medium, 3+ high.
func (e *Engine) Risk() (Risk, []string) {
	p := e.h.Spec.Permissions
	score := 0
	var why []string
	bump := func(n int, reason string) {
		score += n
		why = append(why, reason)
	}
	if len(p.Network.Egress) > 0 {
		bump(1, "can reach the internet")
		for _, r := range p.Network.Egress {
			if strings.HasPrefix(r.Host, "*.") {
				bump(1, "uses wildcard hosts")
				break
			}
		}
	}
	if len(p.Credentials) > 0 {
		bump(1, "can act with your credentials")
		if len(p.Network.Egress) > 0 {
			bump(1, "combines network access with credentials")
		}
	}
	if p.Subagents.Max > 0 {
		bump(1, "can start subagents")
	}
	if p.Network.AllowPrivateNetworks {
		bump(2, "can reach your private network")
	}
	if m := e.h.Spec.Model; m != nil {
		if scheme, _, _ := m.EndpointHostPort(); scheme == "http" {
			bump(1, "talks to its model unencrypted (http)")
		}
	}
	if t := e.h.Spec.Trigger; t.Schedule != "" || t.Loop != nil {
		bump(1, "runs again and again without anyone watching")
	}
	switch {
	case score == 0:
		return RiskLow, why
	case score <= 2:
		return RiskMedium, why
	default:
		return RiskHigh, why
	}
}

// Explain describes in plain language what the harness can do. It is
// shown to the user before every start and must stay understandable
// for someone who has never read the spec.
func (e *Engine) Explain() []string {
	s := e.h.Spec
	p := s.Permissions
	lines := []string{
		fmt.Sprintf("Runs the %s agent on %s.", s.Runtime, describeTarget(s.Target)),
		fmt.Sprintf("Stops after %s per run, or once its model has used %s tokens (or cost $%.2f, for models the runner has a price for).",
			s.Sandbox.Timeout, groupDigits(s.Budget.MaxTokens), s.Budget.MaxCostUSD),
	}
	lines = append(lines, fmt.Sprintf("When it finishes, it can hand back up to %s MiB of files (plain files and folders only; nothing else is kept).",
		groupDigits(int64(s.Sandbox.DiskMiB))))

	if m := s.Model; m != nil {
		line := fmt.Sprintf("Uses the model %s at %s", m.Name, m.Endpoint)
		if m.Credential != "" {
			line += ", authenticated with " + m.Credential
		}
		line += "."
		if scheme, _, _ := m.EndpointHostPort(); scheme == "http" {
			line += " UNENCRYPTED: prompts, code and the credential cross the network in clear text."
		}
		lines = append(lines, line)
	}

	switch {
	case s.Trigger.Schedule != "":
		lines = append(lines, fmt.Sprintf("Starts automatically on the cron schedule %q (times in UTC), without anyone watching, "+
			"until you pause or delete the schedule. A run is skipped while the previous one is still going. Every run has the limits above.",
			s.Trigger.Schedule))
	case s.Trigger.Loop != nil:
		n := s.Trigger.Loop.MaxIterations
		gap := "back to back"
		if d := s.Trigger.Loop.Interval.Duration; d > 0 {
			gap = d.String() + " apart"
		}
		lines = append(lines, fmt.Sprintf("Runs up to %d times in a row (%s), without anyone watching, until the agent reports the task complete; "+
			"it stops at the first run that does not succeed. Every run has the limits above, so all runs together may use up to %s tokens (or $%.2f).",
			n, gap, groupDigits(int64(n)*s.Budget.MaxTokens), float64(n)*s.Budget.MaxCostUSD))
	default:
		lines = append(lines, "Starts only when you start it.")
	}

	if len(p.Network.Egress) == 0 {
		lines = append(lines, "Has no network access at all.")
	} else {
		hosts := make([]string, 0, len(p.Network.Egress))
		for _, r := range p.Network.Egress {
			hosts = append(hosts, describeEgress(r.Host, r.Ports))
		}
		lines = append(lines, "Can connect only to: "+strings.Join(hosts, ", ")+".")
	}
	if p.Network.AllowPrivateNetworks {
		lines = append(lines, "Can reach machines on your private network.")
	}

	if len(p.Credentials) == 0 {
		lines = append(lines, "Has no credentials.")
	} else {
		lines = append(lines, fmt.Sprintf("Can act as you using: %s. The agent never sees these secrets.",
			strings.Join(p.Credentials, ", ")))
	}

	if len(p.Tools.Install) > 0 {
		lines = append(lines, "Gets these tools installed before it starts: "+strings.Join(p.Tools.Install, ", ")+
			". They come from the runner's own catalog of signed packages. The agent cannot install system packages itself; what it can download is limited by its network access.")
	}

	if p.Subagents.Max == 0 {
		lines = append(lines, "Cannot start other agents.")
	} else {
		helpers := fmt.Sprintf("up to %d helper agents", p.Subagents.Max)
		if p.Subagents.Max == 1 {
			helpers = "one helper agent"
		}
		lines = append(lines, "May start "+helpers+" in total (helpers of helpers included). Each runs in its own sandbox "+
			"with at most these permissions, no bigger sandbox and no more time, asks you before the same actions, "+
			"shares this budget, and stops when this agent ends.")
	}

	if len(s.Approvals.RequireFor) > 0 {
		lines = append(lines, "Asks you before: "+strings.Join(s.Approvals.RequireFor, ", ")+
			". To catch these, every connection it makes is inspected (only HTTPS works then). While it waits, the job pauses but its time limit keeps running; "+
			"if nobody decides in time (30 minutes by default), the action is refused.")
	}
	return lines
}

func describeTarget(t string) string {
	if name, ok := strings.CutPrefix(t, "server:"); ok {
		return "server " + name
	}
	return "this computer"
}

func describeEgress(host string, ports []int) string {
	if suffix, ok := strings.CutPrefix(host, "*."); ok {
		host = "any subdomain of " + suffix
	}
	if len(ports) == 1 && ports[0] == 443 {
		return host
	}
	ps := make([]string, len(ports))
	for i, p := range ports {
		ps[i] = strconv.Itoa(p)
	}
	return host + " (ports " + strings.Join(ps, ", ") + ")"
}

func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
