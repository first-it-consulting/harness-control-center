//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

func TestRunnerSettingsCommand(t *testing.T) {
	config := credentialRunner(t)
	restarts := 0
	orig := restartRunner
	restartRunner = func(context.Context) error { restarts++; return nil }
	t.Cleanup(func() { restartRunner = orig })

	code, out, errOut := runCLI("runner", "settings", "get", "--config", config)
	var s runnerd.Settings
	if code != exitOK || json.Unmarshal([]byte(out), &s) != nil || s.MaxConcurrent != 1 {
		t.Fatalf("get: %d %q %q", code, out, errOut)
	}
	s.RetentionJobs = 42
	in, _ := json.Marshal(s)
	if code, out, errOut := runCLIWithInput(string(in), "runner", "settings", "set", "--config", config); code != exitOK || restarts != 0 || !strings.Contains(out, "when the runner restarts") {
		t.Fatalf("set: %d %q %q", code, out, errOut)
	}
	if code, out, _ := runCLIWithInput(string(in), "runner", "settings", "set", "--restart", "--config", config); code != exitOK || restarts != 1 || !strings.Contains(out, "restarted") {
		t.Errorf("set --restart: %d %q", code, out)
	}
	if data, _ := os.ReadFile(config); !strings.Contains(string(data), "maxJobs: 42") {
		t.Errorf("runner.yaml:\n%s", data)
	}

	// "off" only as the installer's restore.
	off := strings.Replace(string(in), `"contactTimeout":"72h0m0s"`, `"contactTimeout":"off"`, 1)
	if off == string(in) {
		t.Fatalf("no contactTimeout in %s", in)
	}
	if code, _, errOut := runCLIWithInput(off, "runner", "settings", "set", "--config", config); code != exitInvalid || !strings.Contains(errOut, "contactTimeout") || strings.Contains(errOut, "\n ") {
		t.Errorf("off: %d %q", code, errOut)
	}
	if code, _, errOut := runCLIWithInput(off, "runner", "settings", "set", "--restore", "--config", config); code != exitOK {
		t.Errorf("restore off: %d %q", code, errOut)
	}

	// Unknown fields, a failed restart.
	if code, _, _ := runCLIWithInput(`{"socket":"/tmp/x"}`, "runner", "settings", "set", "--config", config); code != exitInvalid {
		t.Errorf("unknown field: %d", code)
	}
	restartRunner = func(context.Context) error { return errors.New("no systemd") }
	if code, _, errOut := runCLIWithInput(off, "runner", "settings", "set", "--restart", "--config", config); code != exitInvalid || !strings.Contains(errOut, "saved, but") {
		t.Errorf("failed restart: %d %q", code, errOut)
	}
	if code, _, _ := runCLI("runner", "settings", "show"); code != exitUsage {
		t.Errorf("unknown subcommand: %d", code)
	}
}
