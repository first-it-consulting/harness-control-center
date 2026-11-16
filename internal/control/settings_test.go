//go:build unix

package control

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// startSettingsRunner starts a runner on a socket whose runner.yaml (the
// user's own, as on the Mac) it serves the settings of.
func startSettingsRunner(t *testing.T, provider uiProvider) (sock, config string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	config = filepath.Join(dir, "runner.yaml")
	cfgText := "# the user's own\ntarget: local\nmaxConcurrent: 1\nrunnerUser: me\nsocket: " + dir + "/r.sock\nstateDir: " + dir + "/state\nprovider: vz\n" +
		"vz: {stagingDir: " + dir + "/staging, imageRoot: " + dir + "/images}\n" +
		"images: {base: {kernel: " + dir + "/images/Image, rootfs: " + dir + "/images/base.ext4}}\n"
	if err := os.WriteFile(config, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := runnerd.LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	state := filepath.Join(dir, "state")
	r, err := runner.New(runner.Config{Provider: provider, Images: []string{"base"}, MaxConcurrent: 1,
		Console: runnerd.ConsoleFunc(state, log), Results: runnerd.ResultsFunc(state)})
	if err != nil {
		t.Fatal(err)
	}
	sock = filepath.Join(dir, "r.sock")
	l, err := runnerd.Listen(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	srv := runnerd.NewServer(r, state, log)
	srv.EnableSettings(config, cfg)
	go func() { _ = srv.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return sock, config
}

func TestSettingsThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	sock, config := startSettingsRunner(t, uiProvider{})
	restarts := 0
	ts.s.local = &localRunner{restart: func(context.Context) error { restarts++; return nil }, config: config}
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "mac", Socket: sock}, nil); code != http.StatusCreated {
		t.Fatalf("add runner: %d", code)
	}
	var v runnerd.SettingsView
	if code := ts.do(http.MethodGet, "/api/runners/mac/settings", nil, &v); code != http.StatusOK {
		t.Fatalf("GET: %d", code)
	}
	if v.Config != "" || v.Settings.MaxConcurrent != 1 || v.Bounds.MaxConcurrent < 1 || v.Bounds.ContactTimeoutOff {
		t.Fatalf("view = %+v", v)
	}

	s := v.Settings
	s.ApprovalTimeout = "2h"
	s.Pricing = map[string]budget.Price{"local-model": {}}
	rec := ts.rawJSON(http.MethodPut, "/api/runners/mac/settings", map[string]any{"settings": s})
	if rec.Code != http.StatusOK || restarts != 1 {
		t.Fatalf("PUT: %d %s (restarts %d)", rec.Code, rec.Body.String(), restarts)
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "approvalTimeout: 2h") || !strings.Contains(string(data), "local-model") || !strings.Contains(string(data), "# the user's own") {
		t.Errorf("runner.yaml:\n%s", data)
	}

	// Refused, with the file left alone: past the bounds, "off", unknown
	// fields, unknown runners.
	for name, body := range map[string]any{
		"past the bound": map[string]any{"settings": withField(s, "maxConcurrent", v.Bounds.MaxConcurrent+1)},
		"contact off":    map[string]any{"settings": withField(s, "contactTimeout", "off")},
		"unknown field":  map[string]any{"settings": s, "configPath": "/etc/passwd"},
	} {
		if rec := ts.rawJSON(http.MethodPut, "/api/runners/mac/settings", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if rec := ts.rawJSON(http.MethodPut, "/api/runners/nope/settings", map[string]any{"settings": s}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown runner: %d", rec.Code)
	}
	if after, _ := os.ReadFile(config); string(after) != string(data) {
		t.Errorf("a refused change wrote the file:\n%s", after)
	}
	if restarts != 1 {
		t.Errorf("restarts = %d", restarts)
	}

	// Another runner's file, as its socket names it, is not written.
	ts.s.local.config = config + ".other"
	if rec := ts.rawJSON(http.MethodPut, "/api/runners/mac/settings", map[string]any{"settings": s}); rec.Code != http.StatusBadRequest {
		t.Errorf("not this computer's runner: %d %s", rec.Code, rec.Body.String())
	}
	ts.s.local.config = config

	// A failed restart says the settings are saved but not applied.
	ts.s.local.restart = func(context.Context) error { return errors.New("launchctl: no") }
	rec = ts.rawJSON(http.MethodPut, "/api/runners/mac/settings", map[string]any{"settings": s})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "saved") {
		t.Errorf("failed restart: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSettingsWaitForJobs(t *testing.T) {
	ts := newTestServer(t)
	sock, config := startSettingsRunner(t, uiProvider{})
	ts.s.local = &localRunner{restart: func(context.Context) error { t.Error("restarted with a job running"); return nil }, config: config}
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "mac", Socket: sock}, nil); code != http.StatusCreated {
		t.Fatalf("add runner: %d", code)
	}
	// uiProvider keeps a job with 256 MiB running until it is stopped.
	long := strings.Replace(okHarness, "prompt: hi", "prompt: hi\n  sandbox: {memoryMiB: 256}", 1)
	var started map[string]any
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "mac", YAML: long, Confirmed: true}, &started); code != http.StatusCreated {
		t.Fatalf("run: %d %v", code, started)
	}
	before, _ := os.ReadFile(config)
	var v runnerd.SettingsView
	ts.do(http.MethodGet, "/api/runners/mac/settings", nil, &v)
	rec := ts.rawJSON(http.MethodPut, "/api/runners/mac/settings", map[string]any{"settings": v.Settings})
	if rec.Code != http.StatusConflict {
		t.Errorf("with a job running: %d %s", rec.Code, rec.Body.String())
	}
	if after, _ := os.ReadFile(config); string(after) != string(before) {
		t.Error("the file changed")
	}
}

func TestSettingsOverSSH(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // test programs must be executable
			t.Fatal(err)
		}
	}
	script("ssh", "while [ \"$1\" != -- ]; do shift; done\nshift 2\nexec sh -c \"$1\"\n")
	script("sudo", "[ \"$1\" = -n ] && shift 2 && exec \"$@\"\nexit 2\n")
	script("hcc", "printf '%s\\n' \"$@\" >\"$FAKE/args\"; cat >\"$FAKE/stdin\"\n")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE", dir)

	ts := newTestServer(t)
	s := runnerd.Settings{MaxConcurrent: 3, RetentionAge: "720h"}
	if err := ts.s.settingsOverSSH(context.Background(), "server", settingsChange{Settings: s}); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if got := strings.Fields(string(args)); strings.Join(got, " ") != "runner settings set --config /etc/hcc/runner.yaml --restart" {
		t.Errorf("args = %q", got)
	}
	var sent runnerd.Settings
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if err := json.Unmarshal(stdin, &sent); err != nil || sent.MaxConcurrent != 3 || sent.RetentionAge != "720h" {
		t.Errorf("stdin = %s (%v)", stdin, err)
	}
}

// withField returns s as a JSON object with one field changed.
func withField(s runnerd.Settings, field string, value any) map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	m[field] = value
	return m
}
