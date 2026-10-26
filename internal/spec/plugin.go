package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// PluginKind is the only kind of plugin manifest (plugin.yaml).
const PluginKind = "Plugin"

// Plugin types (ADR-0013). Every plugin runs in a sandbox of its own,
// with exactly the permissions its manifest grants.
const (
	// PluginNotifier gets runner events as JSON lines on stdin, per
	// batch, and passes them on (a webhook, a chat, mail).
	PluginNotifier = "notifier"
	// PluginMCPServer is a stdio MCP server that agents may use
	// (spec.mcp), started for each job that does.
	PluginMCPServer = "mcp-server"
)

// Notifier events.
const (
	EventJobEnded            = "job.ended"
	EventJobAwaitingApproval = "job.awaiting_approval"
	EventSchedulePaused      = "schedule.paused"
)

// PluginEvents are the events a notifier may subscribe to.
var PluginEvents = []string{EventJobEnded, EventJobAwaitingApproval, EventSchedulePaused}

// Limits for plugins.
const (
	MaxPluginArgs          = 32
	DefaultPluginTimeout   = 2 * time.Minute
	MaxPluginTimeout       = 30 * time.Minute
	DefaultPluginMemoryMiB = 512
)

var versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.-]{1,64})?$`)

// Plugin is a plugin manifest.
type Plugin struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   PluginMetadata `yaml:"metadata"`
	Spec       PluginSpec     `yaml:"spec"`
}

// PluginMetadata identifies a plugin.
type PluginMetadata struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Description string `yaml:"description,omitempty"`
}

// PluginSpec is what a plugin is and may do.
type PluginSpec struct {
	Type string `yaml:"type"`
	// Command is run in the plugin's directory as an unprivileged user:
	// a file of the plugin ("./notify.py") or a program of the image or
	// its tools ("python3"), then its arguments.
	Command []string `yaml:"command"`
	// Events a notifier gets.
	Events  []string      `yaml:"events,omitempty"`
	Sandbox PluginSandbox `yaml:"sandbox,omitempty"`
	// Permissions are those of a harness, without subagents.
	Permissions Permissions `yaml:"permissions,omitempty"`
}

// PluginSandbox sizes a plugin's sandbox.
type PluginSandbox struct {
	Image     string `yaml:"image,omitempty"`
	CPUs      int    `yaml:"cpus,omitempty"`
	MemoryMiB int    `yaml:"memoryMiB,omitempty"`
	// Timeout bounds one notifier run (an MCP server lives as long as its
	// job).
	Timeout Duration `yaml:"timeout,omitempty"`
}

// ParsePlugin decodes, defaults and validates a plugin manifest, as
// strictly as a harness.
func ParsePlugin(data []byte) (*Plugin, error) {
	if len(data) > MaxDocumentBytes {
		return nil, ErrTooLarge
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Plugin
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("spec: empty plugin manifest")
		}
		return nil, fmt.Errorf("spec: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("spec: multiple documents are not allowed")
	}
	p.applyDefaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Plugin) applyDefaults() {
	sb := &p.Spec.Sandbox
	if sb.Image == "" {
		sb.Image = "claude-code"
	}
	if sb.CPUs == 0 {
		sb.CPUs = DefaultCPUs
	}
	if sb.MemoryMiB == 0 {
		sb.MemoryMiB = DefaultPluginMemoryMiB
	}
	if sb.Timeout.Duration == 0 {
		sb.Timeout.Duration = DefaultPluginTimeout
	}
	for i := range p.Spec.Permissions.Network.Egress {
		r := &p.Spec.Permissions.Network.Egress[i]
		r.Host = NormalizeHost(r.Host)
		if len(r.Ports) == 0 {
			r.Ports = []int{DefaultEgressPort}
		}
	}
}

// Validate checks every invariant of a defaulted manifest.
func (p *Plugin) Validate() error {
	var errs []error
	add := func(path, format string, args ...any) {
		errs = append(errs, &FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
	}
	if p.APIVersion != APIVersion {
		add("apiVersion", "must be %q, got %q", APIVersion, p.APIVersion)
	}
	if p.Kind != PluginKind {
		add("kind", "must be %q, got %q", PluginKind, p.Kind)
	}
	if !nameRe.MatchString(p.Metadata.Name) {
		add("metadata.name", "must be 1-63 lowercase letters, digits or '-', starting and ending with a letter or digit")
	}
	if !versionRe.MatchString(p.Metadata.Version) {
		add("metadata.version", "must be a version like 1.2.3, got %q", p.Metadata.Version)
	}
	s := &p.Spec
	switch s.Type {
	case PluginNotifier:
		if len(s.Events) == 0 {
			add("spec.events", "a notifier needs at least one of %s", strings.Join(PluginEvents, ", "))
		}
		for i, e := range s.Events {
			if !slices.Contains(PluginEvents, e) {
				add(fmt.Sprintf("spec.events[%d]", i), "unknown event %q (known: %s)", e, strings.Join(PluginEvents, ", "))
			}
		}
	case PluginMCPServer:
		if len(s.Events) > 0 {
			add("spec.events", "only notifiers get events")
		}
	default:
		add("spec.type", "must be %q or %q, got %q", PluginNotifier, PluginMCPServer, s.Type)
	}
	if len(s.Command) == 0 || len(s.Command) > MaxPluginArgs {
		add("spec.command", "must have 1 to %d elements", MaxPluginArgs)
	}
	for i, a := range s.Command {
		if a == "" || len(a) > 1024 || strings.ContainsAny(a, "\x00\n\r") {
			add(fmt.Sprintf("spec.command[%d]", i), "must be 1-1024 characters, without line breaks")
		}
	}
	if len(s.Command) > 0 && !validProgram(s.Command[0]) {
		add("spec.command[0]", "must be a file of the plugin (\"./run.sh\", \"bin/server\") or a program name (\"python3\")")
	}
	sb := &s.Sandbox
	if !imageRe.MatchString(sb.Image) || strings.Contains(sb.Image, "..") {
		add("spec.sandbox.image", "invalid image reference %q", sb.Image)
	}
	checkRange(add, "spec.sandbox.cpus", sb.CPUs, 1, MaxCPUs)
	checkRange(add, "spec.sandbox.memoryMiB", sb.MemoryMiB, 128, MaxMemoryMiB)
	if sb.Timeout.Duration <= 0 || sb.Timeout.Duration > MaxPluginTimeout {
		add("spec.sandbox.timeout", "must be greater than 0 and at most %s", MaxPluginTimeout)
	}
	validatePermissions(&s.Permissions, add)
	if s.Permissions.Subagents.Max != 0 {
		add("spec.permissions.subagents", "plugins cannot start agents")
	}
	return errors.Join(errs...)
}

// validProgram: a relative path inside the plugin (no "..", not
// absolute) or a bare program name.
func validProgram(p string) bool {
	if strings.HasPrefix(p, "-") || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	clean := path.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	return clean != "."
}

// Harness is the harness a plugin's sandbox is built from, so that its
// egress proxy, credential broker and tools are exactly an agent's. It
// is validated like any harness; the prompt only labels it.
func (p *Plugin) Harness(target string) (*Harness, error) {
	h := &Harness{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: p.Metadata.Name, Description: "Plugin " + p.Metadata.Name + " " + p.Metadata.Version},
		Spec: Spec{
			Runtime: "plugin",
			Prompt:  "plugin " + p.Metadata.Name,
			Target:  target,
			Sandbox: Sandbox{
				Image: p.Spec.Sandbox.Image, CPUs: p.Spec.Sandbox.CPUs, MemoryMiB: p.Spec.Sandbox.MemoryMiB,
				Timeout: p.Spec.Sandbox.Timeout,
			},
			Permissions: p.Spec.Permissions,
		},
	}
	b, err := yaml.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	return Parse(b)
}
