package runnerd

import (
	"math"
	"os"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
)

// commentedConfig is goodConfig with comments and settings to keep.
const commentedConfig = `# Managed by install-runner.sh.
` + "target: server:hcc-dev\n" + `# How many jobs at once.
maxConcurrent: 2 # keep me
runnerUser: hcc-runner
socket: /run/hcc/runner.sock
socketGroup: hcc
stateDir: /var/lib/hcc/state
firecracker:
  helper: /usr/local/lib/hcc/hcc-helper
  jailer: /usr/local/lib/hcc/jailer
  firecracker: /usr/local/lib/hcc/firecracker
  chrootBase: /var/lib/hcc-jails
  stagingDir: /var/lib/hcc/staging
  imageRoot: /var/lib/hcc/images
  uid: 64000
  gid: 64000
images:
  base:
    kernel: /var/lib/hcc/images/base/vmlinux
    rootfs: /var/lib/hcc/images/base/rootfs.ext4
retention:
  maxJobs: 50
pricing:
  old-model: {input: 1, output: 2}
`

var wide = SettingsBounds{MaxConcurrent: 8, MaxPluginVMs: 8, MaxDiskMiB: 16384}

func loadSettings(t *testing.T, path string) Settings {
	t.Helper()
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return c.Settings()
}

func TestApplySettings(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	path := writeConfig(t, commentedConfig)
	s := loadSettings(t, path)
	if s.MaxConcurrent != 2 || s.RetentionJobs != 50 || s.RetentionAge != DefaultRetentionAge.String() || s.Pricing["old-model"].OutputPerMTok != 2 {
		t.Fatalf("settings = %+v", s)
	}
	s.MaxConcurrent, s.MaxPluginVMs, s.MaxDiskMiB = 4, 1, 4096
	s.RetentionJobs, s.RetentionAge = 200, "168h0m0s"
	s.ApprovalTimeout, s.MinScheduleInterval, s.ContactTimeout = "10m", "1h", "24h"
	s.Pricing = map[string]budget.Price{"claude-opus-5-5": {InputPerMTok: 15, OutputPerMTok: 75.5}}
	if err := ApplySettings(path, s, wide); err != nil {
		t.Fatal(err)
	}
	got := loadSettings(t, path)
	if got.MaxConcurrent != 4 || got.MaxPluginVMs != 1 || got.MaxDiskMiB != 4096 || got.RetentionJobs != 200 ||
		got.RetentionAge != "168h0m0s" || got.ApprovalTimeout != "10m" || got.MinScheduleInterval != "1h" ||
		got.ContactTimeout != "24h" || len(got.Pricing) != 1 || got.Pricing["claude-opus-5-5"].OutputPerMTok != 75.5 {
		t.Errorf("after apply: %+v", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, keep := range []string{"# Managed by install-runner.sh.", "# How many jobs at once.", "# keep me", "chrootBase: /var/lib/hcc-jails"} {
		if !strings.Contains(string(data), keep) {
			t.Errorf("lost %q:\n%s", keep, data)
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}

	// No prices: the table goes.
	got.Pricing = nil
	if err := ApplySettings(path, got, wide); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "pricing") {
		t.Errorf("pricing kept:\n%s", data)
	}
}

func TestApplySettingsRejectsAndKeepsTheFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	path := writeConfig(t, commentedConfig)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base := loadSettings(t, path)
	for name, change := range map[string]func(*Settings){
		"above the host's bound": func(s *Settings) { s.MaxConcurrent = 9 },
		"no job at all":          func(s *Settings) { s.MaxConcurrent = 0 },
		"turning contact off":    func(s *Settings) { s.ContactTimeout = "off" },
		"approval too short":     func(s *Settings) { s.ApprovalTimeout = "5s" },
		"retention not a time":   func(s *Settings) { s.RetentionAge = "forever" },
		"disk too big":           func(s *Settings) { s.MaxDiskMiB = 1 << 30 },
		"negative price":         func(s *Settings) { s.Pricing = map[string]budget.Price{"m": {InputPerMTok: -1}} },
		"NaN price":              func(s *Settings) { s.Pricing = map[string]budget.Price{"m": {InputPerMTok: math.NaN()}} },
		"bad model name":         func(s *Settings) { s.Pricing = map[string]budget.Price{"a model\n": {}} },
	} {
		s := base
		change(&s)
		if err := ApplySettings(path, s, wide); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("file changed:\n%s", after)
	}
}

func TestSettingsCheck(t *testing.T) {
	cur := Settings{MaxConcurrent: 16, MaxPluginVMs: 2, MaxDiskMiB: 8192, ContactTimeout: "72h"}
	host := SettingsBounds{MaxConcurrent: 4, MaxPluginVMs: 4, MaxDiskMiB: 8192}
	// Above the bound but as configured: fine (the UI never widens).
	if err := cur.Check(cur, host); err != nil {
		t.Errorf("unchanged: %v", err)
	}
	s := cur
	s.MaxConcurrent = 17
	if err := s.Check(cur, host); err == nil {
		t.Error("widened past the bound")
	}
	s.MaxConcurrent = 3
	if err := s.Check(cur, host); err != nil {
		t.Errorf("lowered: %v", err)
	}
	s.ContactTimeout = "off"
	if err := s.Check(cur, host); err == nil {
		t.Error("contact timeout turned off from the UI")
	}
	if err := s.Check(cur, FileBounds()); err != nil {
		t.Errorf("restore with off: %v", err)
	}
	off := cur
	off.ContactTimeout = "off"
	if err := off.Check(off, host); err != nil {
		t.Errorf("off already: %v", err)
	}
	many := cur
	many.Pricing = map[string]budget.Price{}
	for i := range MaxPricedModels + 1 {
		many.Pricing[strings.Repeat("m", i+1)] = budget.Price{}
	}
	if err := many.Check(cur, host); err == nil {
		t.Error("too many prices")
	}
}

func TestHostBounds(t *testing.T) {
	b := HostBounds()
	if b.MaxConcurrent < 1 || b.MaxConcurrent > 64 || b.MaxConcurrent > runtime.NumCPU() || b.ContactTimeoutOff {
		t.Errorf("bounds = %+v", b)
	}
}

func FuzzWithSettings(f *testing.F) {
	f.Add([]byte(commentedConfig))
	f.Add([]byte("retention: 3\npricing: [1]\n"))
	f.Add([]byte("a: &x {b: *x}\n"))
	s := Settings{MaxConcurrent: 2, MaxPluginVMs: 2, MaxDiskMiB: 8192, RetentionJobs: 1, RetentionAge: "1h",
		ApprovalTimeout: "1m", MinScheduleInterval: "1m", ContactTimeout: "off",
		Pricing: map[string]budget.Price{"m": {InputPerMTok: 1}}}
	f.Fuzz(func(t *testing.T, data []byte) {
		// ApplySettings only gets here with a valid configuration, which
		// is at least a mapping without duplicate keys.
		var m map[string]any
		if yaml.Unmarshal(data, &m) != nil {
			return
		}
		out, err := withSettings(data, s)
		if err != nil {
			return
		}
		var got struct {
			MaxConcurrent       int                     `yaml:"maxConcurrent"`
			MaxPluginVMs        int                     `yaml:"maxPluginVMs"`
			MaxDiskMiB          int                     `yaml:"maxDiskMiB"`
			ApprovalTimeout     string                  `yaml:"approvalTimeout"`
			MinScheduleInterval string                  `yaml:"minScheduleInterval"`
			ContactTimeout      string                  `yaml:"contactTimeout"`
			Retention           Retention               `yaml:"retention"`
			Pricing             map[string]budget.Price `yaml:"pricing"`
		}
		if err := yaml.Unmarshal(out, &got); err != nil {
			t.Fatalf("result does not parse: %v\n%s", err, out)
		}
		if got.MaxConcurrent != s.MaxConcurrent || got.MaxPluginVMs != s.MaxPluginVMs || got.MaxDiskMiB != s.MaxDiskMiB ||
			got.ApprovalTimeout != s.ApprovalTimeout || got.MinScheduleInterval != s.MinScheduleInterval ||
			got.ContactTimeout != s.ContactTimeout || got.Retention.MaxJobs != s.RetentionJobs ||
			got.Retention.MaxAge != s.RetentionAge || len(got.Pricing) != 1 || got.Pricing["m"] != s.Pricing["m"] {
			t.Fatalf("settings not in the result: %+v\n%s", got, out)
		}
	})
}
