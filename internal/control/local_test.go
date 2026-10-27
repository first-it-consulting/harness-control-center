package control

import "testing"

func TestWithLocalRunner(t *testing.T) {
	c := &Config{Runners: []RunnerRef{{Name: "server", SSH: "hcc-dev"}}}
	next, added, err := c.WithLocalRunner("/Users/me/Library/Application Support/hcc/local/runner.sock")
	if err != nil || !added || len(next.Runners) != 2 || next.Runners[1].Name != LocalRunnerName || len(c.Runners) != 1 {
		t.Fatalf("%+v %v %v (original %+v)", next, added, err, c)
	}
	// Again: already there.
	if again, added, err := next.WithLocalRunner(next.Runners[1].Socket); err != nil || added || again != next {
		t.Errorf("added twice: %v %v", added, err)
	}
	// The name is taken by another runner.
	taken := &Config{Runners: []RunnerRef{{Name: LocalRunnerName, SSH: "elsewhere"}}}
	if _, _, err := taken.WithLocalRunner("/x/runner.sock"); err == nil {
		t.Error("replaced another runner")
	}
	// The socket must be a clean absolute path.
	if _, _, err := (&Config{}).WithLocalRunner("relative/runner.sock"); err == nil {
		t.Error("accepted a relative socket")
	}
}
