package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
	"github.com/first-it-consulting/harness-control-center/internal/tools"
)

// Defaults for the manager.
const (
	DefaultMaxVMs = 2
	// batchDelay collects events before a notifier runs.
	batchDelay = 5 * time.Second
	// maxQueue bounds a notifier's waiting events; the oldest are
	// dropped beyond it.
	maxQueue = 1000
	// maxBatch bounds the events of one run.
	maxBatch = 200
	// defaultScheduleCheck is how often paused schedules are looked for.
	defaultScheduleCheck = 30 * time.Second
)

// Event is what a notifier gets, one JSON object per line on stdin.
// Messages may come from agents: plugins must treat them as untrusted.
type Event struct {
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Runner   string    `json:"runner"`
	Job      string    `json:"job,omitempty"`
	Harness  string    `json:"harness,omitempty"`
	State    string    `json:"state,omitempty"`
	Schedule string    `json:"schedule,omitempty"`
	Message  string    `json:"message,omitempty"`
}

// Runner is what the manager needs of the runner.
type Runner interface {
	Subscribe(buffer int) (<-chan runner.Event, func())
	Schedules() []runner.Schedule
	Status() runner.Status
	// OnKill registers a function the kill switch calls.
	OnKill(func())
}

// Config configures a Manager.
type Config struct {
	Store    *Store
	Provider sandbox.Provider
	// Egress builds a plugin sandbox's egress proxy, exactly as for a job.
	// Without it, plugins with network access cannot run.
	Egress func(runner.EgressJob) (sandbox.Proxy, error)
	// Images the provider can boot, and their tool catalogs.
	Images []string
	Tools  map[string]*tools.Catalog
	// Target is the runner's target name.
	Target string
	// MaxVMs bounds the plugin sandboxes running at once (default
	// DefaultMaxVMs); they come out of their own share of the provider's
	// identities, not the jobs'.
	MaxVMs  int
	Console func(id string) io.Writer
	Log     *slog.Logger
	Now     func() time.Time
	// ScheduleCheck is how often paused schedules are looked for
	// (default 30s).
	ScheduleCheck time.Duration
}

// Status is a plugin's recent activity.
type Status struct {
	Name    string    `json:"name"`
	Pending int       `json:"pending,omitempty"`
	Dropped int       `json:"dropped,omitempty"`
	Running bool      `json:"running,omitempty"`
	LastRun time.Time `json:"lastRun,omitzero"`
	// LastRunID names the last run's console log.
	LastRunID  string `json:"lastRunId,omitempty"`
	LastResult string `json:"lastResult,omitempty"`
}

// Manager runs plugins.
type Manager struct {
	cfg  Config
	vms  chan struct{}
	wake chan struct{}

	mu     sync.Mutex
	queues map[string][]Event
	status map[string]*Status
	wg     sync.WaitGroup
	// runs is cancelled by the kill switch: every plugin run derives from
	// the one that was current when it started.
	base       context.Context
	runs       context.Context
	cancelRuns context.CancelFunc
}

// NewManager returns a manager; Run starts it.
func NewManager(cfg Config) *Manager {
	if cfg.MaxVMs <= 0 {
		cfg.MaxVMs = DefaultMaxVMs
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ScheduleCheck <= 0 {
		cfg.ScheduleCheck = defaultScheduleCheck
	}
	if cfg.Console == nil {
		cfg.Console = func(string) io.Writer { return io.Discard }
	}
	return &Manager{cfg: cfg, vms: make(chan struct{}, cfg.MaxVMs), wake: make(chan struct{}, 1),
		queues: map[string][]Event{}, status: map[string]*Status{}}
}

// Run passes r's events to the notifiers until ctx ends, then waits for
// running plugins to stop. The kill switch stops running plugins, and
// none start while it is engaged.
func (m *Manager) Run(ctx context.Context, r Runner) {
	m.mu.Lock()
	m.base = ctx
	m.runs, m.cancelRuns = context.WithCancel(ctx)
	m.mu.Unlock()
	r.OnKill(m.kill)
	events, stop := r.Subscribe(1024)
	defer stop()
	defer m.wg.Wait()
	schedTick := time.NewTicker(m.cfg.ScheduleCheck)
	defer schedTick.Stop()
	paused := pausedSchedules(r.Schedules())
	var flush <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if ev, ok := m.fromJob(e); ok && m.enqueue(ev) && flush == nil {
				flush = time.After(batchDelay)
			}
		case <-schedTick.C:
			now := pausedSchedules(r.Schedules())
			for id, s := range now {
				if _, was := paused[id]; !was {
					ev := Event{Type: spec.EventSchedulePaused, At: m.cfg.Now().UTC(), Runner: m.cfg.Target,
						Schedule: id, Harness: s.Harness, Message: s.Message}
					if m.enqueue(ev) && flush == nil {
						flush = time.After(batchDelay)
					}
				}
			}
			paused = now
		case <-flush:
			flush = nil
			if m.dispatch(r) {
				flush = time.After(batchDelay) // killed, or some had to wait for a slot
			}
		case <-m.wake:
			if flush == nil {
				flush = time.After(0)
			}
		}
	}
}

func pausedSchedules(list []runner.Schedule) map[string]runner.Schedule {
	out := map[string]runner.Schedule{}
	for _, s := range list {
		if s.State == runner.SchedulePaused {
			out[s.ID] = s
		}
	}
	return out
}

func (m *Manager) fromJob(e runner.Event) (Event, bool) {
	ev := Event{At: e.At.UTC(), Runner: m.cfg.Target, Job: e.JobID, Harness: e.Harness, State: string(e.To), Message: e.Message}
	switch {
	case e.To.Terminal():
		ev.Type = spec.EventJobEnded
	case e.To == runner.StateAwaitingApproval:
		ev.Type = spec.EventJobAwaitingApproval
	default:
		return Event{}, false
	}
	return ev, true
}

// enqueue queues ev for every notifier that wants it.
func (m *Manager) enqueue(ev Event) bool {
	list, err := m.cfg.Store.List()
	if err != nil {
		m.cfg.Log.Error("plugins unreadable", "err", err)
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	queued := false
	for _, in := range list {
		_, p, _, err := m.cfg.Store.Get(in.Name)
		if err != nil || p.Spec.Type != spec.PluginNotifier || !slices.Contains(p.Spec.Events, ev.Type) {
			continue
		}
		m.queues[in.Name] = append(m.queues[in.Name], ev)
		st := m.statusLocked(in.Name)
		if over := len(m.queues[in.Name]) - maxQueue; over > 0 {
			m.queues[in.Name] = m.queues[in.Name][over:]
			st.Dropped += over
		}
		st.Pending = len(m.queues[in.Name])
		queued = true
	}
	return queued
}

func (m *Manager) statusLocked(name string) *Status {
	st := m.status[name]
	if st == nil {
		st = &Status{Name: name}
		m.status[name] = st
	}
	return st
}

// kill stops every running plugin (the kill switch).
func (m *Manager) kill() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancelRuns == nil {
		return
	}
	m.cancelRuns()
	m.runs, m.cancelRuns = context.WithCancel(m.base)
	m.cfg.Log.Warn("kill switch: plugins stopped")
}

// dispatch starts a run for every notifier with events that is not
// running yet, while slots are free and the kill switch is not engaged.
// It reports whether some are left. The kill switch is checked under the
// same lock kill takes: either it is seen here, or kill cancels the runs
// started here.
func (m *Manager) dispatch(r Runner) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Status().Killed {
		return true
	}
	ctx := m.runs
	left := false
	names := make([]string, 0, len(m.queues))
	for name := range m.queues {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		q := m.queues[name]
		st := m.statusLocked(name)
		if len(q) == 0 || st.Running {
			continue
		}
		select {
		case m.vms <- struct{}{}:
		default:
			left = true
			continue
		}
		n := min(len(q), maxBatch)
		batch := slices.Clone(q[:n])
		m.queues[name] = q[n:]
		st.Pending, st.Running = len(m.queues[name]), true
		m.wg.Go(func() {
			defer func() { <-m.vms }()
			id, res := m.notify(ctx, name, batch)
			m.mu.Lock()
			st.Running, st.LastRun, st.LastRunID, st.LastResult = false, m.cfg.Now().UTC(), id, res
			more := len(m.queues[name]) > 0
			m.mu.Unlock()
			if more {
				select {
				case m.wake <- struct{}{}:
				default:
				}
			}
		})
	}
	return left
}

// Statuses reports every installed plugin's activity.
func (m *Manager) Statuses() []Status {
	list, _ := m.cfg.Store.List()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(list))
	for _, in := range list {
		out = append(out, *m.statusLocked(in.Name))
	}
	return out
}

// notify runs a notifier on a batch of events. Failures are logged and
// reported in its status; the events are not retried (a broken plugin
// must not keep sandboxes busy).
func (m *Manager) notify(ctx context.Context, name string, batch []Event) (string, string) {
	_, p, bundle, err := m.cfg.Store.Get(name)
	if err != nil {
		return "", "failed: " + err.Error()
	}
	id := newRunID()
	err = m.runPlugin(ctx, id, p, bundle, func(t *pluginTask) { t.Events = batch })
	if err != nil {
		m.cfg.Log.Warn("notifier failed", "plugin", name, "run", id, "events", len(batch), "err", err)
		return id, "failed: " + err.Error()
	}
	m.cfg.Log.Info("notifier ran", "plugin", name, "run", id, "events", len(batch))
	return id, "ok"
}

// pluginTask is what a plugin's sandbox gets as its task: like a job's
// (see runner.Task), with the plugin's command and, for a notifier, the
// events.
type pluginTask struct {
	Runtime string            `json:"runtime"`
	Prompt  string            `json:"prompt"`
	Tools   *runner.TaskTools `json:"tools,omitempty"`
	Plugin  taskPlugin        `json:"plugin"`
	Events  []Event           `json:"events,omitempty"`
}

type taskPlugin struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Command []string `json:"command"`
}

// runPlugin runs a plugin in its own sandbox until its command exits
// (or its time is up), with the egress, credentials and tools of its
// manifest. The caller holds a slot.
func (m *Manager) runPlugin(ctx context.Context, id string, p *spec.Plugin, bundle []byte, fill func(*pluginTask)) error {
	h, err := p.Harness(m.cfg.Target)
	if err != nil {
		return err
	}
	img := h.Spec.Sandbox.Image
	if !slices.Contains(m.cfg.Images, img) {
		return fmt.Errorf("unknown image %q", img)
	}
	t := pluginTask{Runtime: "plugin", Prompt: h.Spec.Prompt,
		Plugin: taskPlugin{Name: p.Metadata.Name, Type: p.Spec.Type, Command: p.Spec.Command}}
	if grants := p.Spec.Permissions.Tools.Install; len(grants) > 0 {
		cat := m.cfg.Tools[img]
		if cat == nil {
			return fmt.Errorf("image %q offers no tools", img)
		}
		pkgs, files, err := cat.Resolve(grants)
		if err != nil {
			return err
		}
		t.Tools = &runner.TaskTools{Packages: pkgs, Files: files}
	}
	if fill != nil {
		fill(&t)
	}
	task, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if len(task) > sandbox.MaxTaskBytes {
		return fmt.Errorf("task is %d bytes; the limit is %d", len(task), sandbox.MaxTaskBytes)
	}
	s := sandbox.Spec{
		ID: id, Image: img, Task: task, Bundle: bundle, Tools: t.Tools != nil,
		CPUs: h.Spec.Sandbox.CPUs, MemoryMiB: h.Spec.Sandbox.MemoryMiB, Timeout: h.Spec.Sandbox.Timeout.Duration,
	}
	perms := h.Spec.Permissions
	if len(perms.Network.Egress) > 0 || len(perms.Credentials) > 0 {
		if m.cfg.Egress == nil {
			return errors.New("network access is not configured on this runner")
		}
		proxy, err := m.cfg.Egress(runner.EgressJob{Harness: h, JobID: id})
		if err != nil {
			return fmt.Errorf("egress proxy: %w", err)
		}
		s.Network = &sandbox.Network{Proxy: proxy}
	}
	exit := &exitWatcher{}
	console := m.cfg.Console(id)
	res, err := m.cfg.Provider.Run(ctx, s, io.MultiWriter(console, exit))
	if c, ok := console.(io.Closer); ok {
		_ = c.Close()
	}
	switch {
	case err != nil:
		return err
	case res.Outcome != sandbox.OutcomeExited:
		return fmt.Errorf("sandbox %s", res.Outcome)
	case exit.code() != 0:
		return fmt.Errorf("exited with status %d", exit.code())
	}
	return nil
}

func newRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "plg-" + hex.EncodeToString(b)
}

// exitWatcher reads init's exit marker ("HCC-RESULT-END exit=N"; the
// last one counts) from the console.
type exitWatcher struct {
	mu   sync.Mutex
	line []byte
	n    int
	seen bool
}

const exitMarker = "HCC-RESULT-END exit="

func (w *exitWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b != '\n' {
			if len(w.line) < 256 {
				w.line = append(w.line, b)
			}
			continue
		}
		line := strings.TrimSpace(string(w.line))
		w.line = w.line[:0]
		if rest, ok := strings.CutPrefix(line, exitMarker); ok {
			if n, err := strconv.Atoi(rest); err == nil {
				w.n, w.seen = n, true
			}
		}
	}
	return len(p), nil
}

// code is the plugin's exit status; no marker counts as a failure.
func (w *exitWatcher) code() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.seen {
		return -1
	}
	return w.n
}
