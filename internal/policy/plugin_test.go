package policy

import (
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

func TestExplainPlugin(t *testing.T) {
	p, err := spec.ParsePlugin([]byte(`
apiVersion: hcc/v1alpha1
kind: Plugin
metadata: {name: webhook, version: 0.1.0}
spec:
  type: notifier
  command: [./notify.sh]
  events: [job.ended]
  permissions:
    network: {egress: [{host: hooks.example.com}]}
    credentials: [hook]
`))
	if err != nil {
		t.Fatal(err)
	}
	risk, why, lines := ExplainPlugin(p)
	text := strings.Join(lines, "\n")
	if risk != RiskHigh || len(why) != 4 {
		t.Errorf("risk %s %v", risk, why)
	}
	for _, want := range []string{"A notifier", "the end of each job", "./notify.sh", "hooks.example.com", "The plugin never sees"} {
		if !strings.Contains(text, want) {
			t.Errorf("explanation lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "agent on") || strings.Contains(text, "hand back") {
		t.Errorf("harness-only lines leaked:\n%s", text)
	}
}
