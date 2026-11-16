package runnerd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// Settings are the parts of runner.yaml a person may change from the
// Control Center. Everything else in the file (paths, provider, images,
// identities, network) is the installer's.
type Settings struct {
	MaxConcurrent       int                     `json:"maxConcurrent"`
	MaxPluginVMs        int                     `json:"maxPluginVMs"`
	MaxDiskMiB          int                     `json:"maxDiskMiB"`
	RetentionJobs       int                     `json:"retentionJobs"`
	RetentionAge        string                  `json:"retentionAge"`
	ApprovalTimeout     string                  `json:"approvalTimeout"`
	MinScheduleInterval string                  `json:"minScheduleInterval"`
	ContactTimeout      string                  `json:"contactTimeout"`
	Pricing             map[string]budget.Price `json:"pricing"`
}

// SettingsBounds are the largest values the Control Center may set on
// this host. A value above its bound is accepted only if it is the one
// already configured: the UI never widens a limit past its bound.
type SettingsBounds struct {
	MaxConcurrent int `json:"maxConcurrent"`
	MaxPluginVMs  int `json:"maxPluginVMs"`
	MaxDiskMiB    int `json:"maxDiskMiB"`
	// ContactTimeoutOff: contactTimeout may be set to "off" (schedules
	// never pause for want of a person). Only the file's own bounds
	// allow it.
	ContactTimeoutOff bool `json:"contactTimeoutOff"`
}

// SettingsView is what GET /v1/settings answers.
type SettingsView struct {
	Settings Settings       `json:"settings"`
	Bounds   SettingsBounds `json:"bounds"`
	// Config is the runner's configuration file, for a Control Center on
	// the same computer (the Mac's runner); never shown in the UI.
	Config string `json:"config,omitempty"`
}

// MaxPricedModels bounds the pricing table.
const MaxPricedModels = 64

// maxPrice bounds a price in USD per million tokens.
const maxPrice = 1e4

// pricedModelRe is the syntax of a model name in the pricing table.
var pricedModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)

// Settings returns c's settings (after LoadConfig's defaults).
func (c *Config) Settings() Settings {
	p := map[string]budget.Price{}
	for k, v := range c.Pricing {
		p[k] = v
	}
	return Settings{
		MaxConcurrent:       c.MaxConcurrent,
		MaxPluginVMs:        c.PluginVMs(),
		MaxDiskMiB:          c.MaxDiskMiB,
		RetentionJobs:       c.Retention.MaxJobs,
		RetentionAge:        c.Retention.MaxAge,
		ApprovalTimeout:     c.ApprovalTimeout,
		MinScheduleInterval: c.MinScheduleInterval,
		ContactTimeout:      c.ContactTimeout,
		Pricing:             p,
	}
}

// HostBounds are the bounds on this host: no more VMs at once than it
// has CPUs.
func HostBounds() SettingsBounds {
	cpus := min(max(runtime.NumCPU(), 1), 64)
	return SettingsBounds{MaxConcurrent: cpus, MaxPluginVMs: cpus, MaxDiskMiB: sandbox.MaxDiskMiB}
}

// FileBounds are the bounds runner.yaml itself allows, for putting back
// settings that were in the file before (an update rewrites it).
func FileBounds() SettingsBounds {
	return SettingsBounds{MaxConcurrent: 64, MaxPluginVMs: 64, MaxDiskMiB: sandbox.MaxDiskMiB, ContactTimeoutOff: true}
}

// Check reports whether s may replace current within b. The full
// configuration is validated again when it is written (ApplySettings).
func (s Settings) Check(current Settings, b SettingsBounds) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	bounded := func(name string, v, lo, hi, cur int) {
		if v == cur && v >= lo {
			return
		}
		if v < lo || v > hi {
			add("%s must be between %d and %d on this runner", name, lo, hi)
		}
	}
	bounded("maxConcurrent", s.MaxConcurrent, 1, b.MaxConcurrent, current.MaxConcurrent)
	bounded("maxPluginVMs", s.MaxPluginVMs, 0, b.MaxPluginVMs, current.MaxPluginVMs)
	bounded("maxDiskMiB", s.MaxDiskMiB, 1, b.MaxDiskMiB, current.MaxDiskMiB)
	// Never pausing schedules for want of a person is a decision for
	// whoever edits the file as root.
	if s.ContactTimeout == "off" && current.ContactTimeout != "off" && !b.ContactTimeoutOff {
		add("contactTimeout can be turned off only in the runner's configuration file")
	}
	if len(s.Pricing) > MaxPricedModels {
		add("at most %d models can have a price", MaxPricedModels)
	}
	for model, p := range s.Pricing {
		if !pricedModelRe.MatchString(model) {
			add("invalid model name %q in pricing", model)
		}
		if !validPrice(p.InputPerMTok) || !validPrice(p.OutputPerMTok) {
			add("pricing for %q must be between 0 and %g USD per million tokens", model, maxPrice)
		}
	}
	return errors.Join(errs...)
}

func validPrice(p float64) bool { return !math.IsNaN(p) && p >= 0 && p <= maxPrice }

// ApplySettings writes s into the runner configuration at path, keeping
// everything else in the file (and its comments). It checks s against
// the file's current settings and b, validates the whole result as
// LoadConfig would, and replaces the file atomically with the same mode
// and owner.
func ApplySettings(path string, s Settings, b SettingsBounds) error {
	f, err := os.Open(path) //nolint:gosec // the operator chooses the config file
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes))
	fi, statErr := f.Stat()
	_ = f.Close() // read-only
	if err != nil {
		return err
	}
	if statErr != nil {
		return statErr
	}
	cur, err := parseConfig(data, path)
	if err != nil {
		return err
	}
	if err := s.Check(cur.Settings(), b); err != nil {
		return err
	}
	out, err := withSettings(data, s)
	if err != nil {
		return err
	}
	if _, err := parseConfig(out, path); err != nil {
		return err
	}
	return replaceFile(path, out, fi)
}

// withSettings returns the YAML document data with s's keys set.
func withSettings(data []byte, s Settings) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("runnerd: the configuration is not a YAML mapping")
	}
	root := doc.Content[0]
	setScalar(root, "maxConcurrent", strconv.Itoa(s.MaxConcurrent), "!!int")
	setScalar(root, "maxPluginVMs", strconv.Itoa(s.MaxPluginVMs), "!!int")
	setScalar(root, "maxDiskMiB", strconv.Itoa(s.MaxDiskMiB), "!!int")
	ret := mappingAt(root, "retention")
	setScalar(ret, "maxJobs", strconv.Itoa(s.RetentionJobs), "!!int")
	setScalar(ret, "maxAge", s.RetentionAge, "!!str")
	setScalar(root, "approvalTimeout", s.ApprovalTimeout, "!!str")
	setScalar(root, "minScheduleInterval", s.MinScheduleInterval, "!!str")
	setScalar(root, "contactTimeout", s.ContactTimeout, "!!str")
	if len(s.Pricing) == 0 {
		deleteKey(root, "pricing")
	} else {
		pricing := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		models := make([]string, 0, len(s.Pricing))
		for m := range s.Pricing {
			models = append(models, m)
		}
		slices.Sort(models)
		for _, m := range models {
			p := s.Pricing[m]
			price := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
			// Untagged: "1" reads as a float too, without "!!float".
			setScalar(price, "input", strconv.FormatFloat(p.InputPerMTok, 'f', -1, 64), "")
			setScalar(price, "output", strconv.FormatFloat(p.OutputPerMTok, 'f', -1, 64), "")
			pricing.Content = append(pricing.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: m}, price)
		}
		setNode(root, "pricing", pricing)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func setScalar(m *yaml.Node, key, value, tag string) {
	setNode(m, key, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
}

// setNode sets key in the mapping m, keeping the old value's comments.
func setNode(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			old := m.Content[i+1]
			v.HeadComment, v.LineComment, v.FootComment = old.HeadComment, old.LineComment, old.FootComment
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

// mappingAt returns the mapping under key in m, creating (or replacing a
// non-mapping value with) an empty one.
func mappingAt(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setNode(m, key, n)
	return n
}

func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = slices.Delete(m.Content, i, i+2)
			return
		}
	}
}

// replaceFile atomically replaces path with data, keeping old's mode and
// owner.
func replaceFile(path string, data []byte, old os.FileInfo) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(old.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := sameOwner(tmp, old); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// EnableSettings serves GET /v1/settings: cfg's settings (those the
// runner runs with) and this host's bounds. config is cfg's file.
func (s *Server) EnableSettings(config string, cfg *Config) {
	view := SettingsView{Settings: cfg.Settings(), Bounds: HostBounds(), Config: config}
	s.mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, view)
	})
}

// Settings returns the runner's settings and bounds.
func (c *Client) Settings(ctx context.Context) (SettingsView, error) {
	var v SettingsView
	return v, c.do(ctx, http.MethodGet, "/v1/settings", nil, &v)
}
