package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// ExplainPlugin describes in plain language what a plugin is and may do,
// with a risk rating: shown before it is installed (ADR-0013). Its
// sandbox's permissions are rated like a harness's, and a notifier gets
// one more point: it sees every job on the runner.
func ExplainPlugin(p *spec.Plugin) (Risk, []string, []string) {
	h, err := p.Harness("local")
	if err != nil {
		return RiskHigh, []string{"invalid manifest"}, []string{err.Error()}
	}
	e := &Engine{h: h}
	_, why := e.Risk()
	score := len(why)
	s := p.Spec
	var lines []string
	switch s.Type {
	case spec.PluginNotifier:
		score++
		why = append(why, "learns about every job on the runner")
		lines = append(lines, fmt.Sprintf("A notifier: is told about %s, for every job on the runner (with harness names, "+
			"states and messages, some written by agents), and passes that on. Each batch runs for at most %s.",
			describeEvents(s.Events), s.Sandbox.Timeout))
	case spec.PluginMCPServer:
		lines = append(lines, "An MCP server: offers tools to the agents whose harness names it in spec.mcp. "+
			"Each such job gets its own copy, which stops with the job.")
	}
	lines = append(lines, fmt.Sprintf("Runs %q in its own sandbox (%s image, %d CPU, %d MiB), as an unprivileged user.",
		strings.Join(s.Command, " "), s.Sandbox.Image, s.Sandbox.CPUs, s.Sandbox.MemoryMiB))
	perm := e.Explain()
	// From the harness explanation, only what concerns permissions.
	for _, l := range perm {
		if strings.HasPrefix(l, "Can ") || strings.HasPrefix(l, "Has ") || strings.HasPrefix(l, "Gets these tools") {
			l = strings.ReplaceAll(l, "The agent never sees", "The plugin never sees")
			lines = append(lines, strings.ReplaceAll(l, "The agent cannot install", "The plugin cannot install"))
		}
	}
	risk := RiskHigh
	switch {
	case score == 0:
		risk = RiskLow
	case score <= 2:
		risk = RiskMedium
	}
	return risk, why, lines
}

func describeEvents(events []string) string {
	names := map[string]string{
		spec.EventJobEnded:            "the end of each job",
		spec.EventJobAwaitingApproval: "each request that waits for your approval",
		spec.EventSchedulePaused:      "each schedule that pauses",
	}
	var out []string
	for _, e := range spec.PluginEvents {
		if slices.Contains(events, e) {
			out = append(out, names[e])
		}
	}
	return strings.Join(out, ", ")
}
