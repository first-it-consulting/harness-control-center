// Package runner executes harnesses in sandboxes: it queues jobs,
// enforces a concurrency limit, tracks each job through its state
// machine, and offers a per-job cancel and a global kill switch.
//
// This is the runner skeleton. It boots the harness's image and treats a
// clean guest shutdown as success; agent runtimes, the egress proxy,
// credentials and schedules are not wired in yet. Harnesses that need any
// of those are rejected (fail closed) rather than run with different
// behaviour than the user approved.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Errors returned by the runner.
var (
	ErrKilled   = errors.New("runner: kill switch is engaged; call Resume to accept jobs again")
	ErrClosed   = errors.New("runner: shut down")
	ErrNotFound = errors.New("runner: no such job")
)

// Image is a bootable sandbox image.
type Image struct {
	Kernel string
	RootFS string
}

// Config configures a Runner.
type Config struct {
	Provider sandbox.Provider
	// Images maps spec.sandbox.image names to files. Unknown names are
	// rejected.
	Images map[string]Image
	// MaxConcurrent bounds running jobs (default 1).
	MaxConcurrent int
	// Console returns where a job's console output goes (default: discard).
	Console func(jobID string) io.Writer
	// Now and NewID are injectable for tests.
	Now   func() time.Time
	NewID func() string
}

type job struct {
	Job
	cancel context.CancelFunc
}

// Runner runs jobs. It is safe for concurrent use.
type Runner struct {
	cfg  Config
	sem  chan struct{}
	wg   sync.WaitGroup
	mu   sync.Mutex
	jobs map[string]*job
	subs map[chan Event]struct{}

	killed bool
	closed bool
	// root is cancelled by KillAll and Shutdown; every job derives from
	// the root that was current when it was submitted.
	root       context.Context
	cancelRoot context.CancelFunc
}

// New returns a Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Provider == nil {
		return nil, errors.New("runner: Provider is required")
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.Console == nil {
		cfg.Console = func(string) io.Writer { return io.Discard }
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewID == nil {
		cfg.NewID = randomID
	}
	r := &Runner{
		cfg:  cfg,
		sem:  make(chan struct{}, cfg.MaxConcurrent),
		jobs: map[string]*job{},
		subs: map[chan Event]struct{}{},
	}
	r.root, r.cancelRoot = context.WithCancel(context.Background())
	return r, nil
}

// Submit validates h against what this runner can enforce, queues it and
// returns the job ID.
func (r *Runner) Submit(h *spec.Harness) (string, error) {
	if h == nil {
		return "", errors.New("runner: nil harness")
	}
	if err := h.Validate(); err != nil {
		return "", fmt.Errorf("runner: invalid harness: %w", err)
	}
	if err := checkSupported(h); err != nil {
		return "", err
	}
	img, ok := r.cfg.Images[h.Spec.Sandbox.Image]
	if !ok {
		return "", fmt.Errorf("runner: unknown image %q", h.Spec.Sandbox.Image)
	}

	r.mu.Lock()
	switch {
	case r.closed:
		r.mu.Unlock()
		return "", ErrClosed
	case r.killed:
		r.mu.Unlock()
		return "", ErrKilled
	}
	id := r.cfg.NewID()
	ctx, cancel := context.WithCancel(r.root)
	j := &job{Job: Job{ID: id, Harness: h.Metadata.Name, State: StateQueued, Created: r.cfg.Now()}, cancel: cancel}
	r.jobs[id] = j
	r.wg.Add(1)
	r.mu.Unlock()

	r.emit(Event{JobID: id, Harness: j.Harness, To: StateQueued, At: j.Created})
	sbx := sandbox.Spec{
		ID:        id,
		Kernel:    img.Kernel,
		RootFS:    img.RootFS,
		CPUs:      h.Spec.Sandbox.CPUs,
		MemoryMiB: h.Spec.Sandbox.MemoryMiB,
		Timeout:   h.Spec.Sandbox.Timeout.Duration,
	}
	go r.execute(ctx, j, sbx)
	return id, nil
}

func (r *Runner) execute(ctx context.Context, j *job, sbx sandbox.Spec) {
	defer r.wg.Done()
	defer j.cancel()

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		r.transition(j, StateCancelled, "cancelled while queued")
		return
	}
	if !r.transition(j, StateProvisioning, "") {
		return // cancelled in the meantime
	}
	if ctx.Err() != nil {
		r.transition(j, StateCancelled, "cancelled before start")
		return
	}
	r.transition(j, StateRunning, "")

	res, err := r.cfg.Provider.Run(ctx, sbx, r.cfg.Console(j.ID))
	switch {
	case err != nil:
		r.transition(j, StateFailed, err.Error())
	case res.Outcome == sandbox.OutcomeExited:
		r.transition(j, StateSucceeded, "")
	case res.Outcome == sandbox.OutcomeTimedOut:
		r.transition(j, StateTimedOut, fmt.Sprintf("stopped after %s", sbx.Timeout))
	case res.Outcome == sandbox.OutcomeKilled:
		r.transition(j, StateCancelled, "stopped")
	default:
		r.transition(j, StateFailed, "sandbox reported "+res.Outcome.String())
	}
}

// transition moves j to state if allowed and emits the event.
func (r *Runner) transition(j *job, to State, msg string) bool {
	r.mu.Lock()
	from := j.State
	if !canTransition(from, to) {
		r.mu.Unlock()
		return false
	}
	now := r.cfg.Now()
	j.State = to
	if to == StateRunning {
		j.Started = now
	}
	if to.Terminal() {
		j.Finished = now
		j.Error = msg
		if to == StateSucceeded {
			j.Error = ""
		}
	}
	r.mu.Unlock()
	r.emit(Event{JobID: j.ID, Harness: j.Harness, From: from, To: to, At: now, Message: msg})
	return true
}

// Cancel stops one job, queued or running.
func (r *Runner) Cancel(id string) error {
	r.mu.Lock()
	j, ok := r.jobs[id]
	r.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	j.cancel()
	return nil
}

// KillAll is the kill switch: it stops every queued and running job and
// refuses new ones until Resume is called.
func (r *Runner) KillAll() {
	r.mu.Lock()
	r.killed = true
	cancel := r.cancelRoot
	r.root, r.cancelRoot = context.WithCancel(context.Background())
	r.mu.Unlock()
	cancel()
}

// Resume accepts new jobs again after KillAll.
func (r *Runner) Resume() {
	r.mu.Lock()
	r.killed = false
	r.mu.Unlock()
}

// Shutdown stops all jobs, refuses new ones and waits for every job to
// finish (or ctx to end).
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.KillAll()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("runner: shutdown: %w", ctx.Err())
	}
}

// Get returns a snapshot of one job.
func (r *Runner) Get(id string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return Job{}, false
	}
	return j.Job, true
}

// List returns snapshots of all jobs, oldest first.
func (r *Runner) List() []Job {
	r.mu.Lock()
	out := make([]Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j.Job)
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b Job) int { return a.Created.Compare(b.Created) })
	return out
}

// Subscribe returns a stream of events and a function to stop it.
// Delivery never blocks the runner: a subscriber that falls more than
// buffer events behind loses events.
func (r *Runner) Subscribe(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, max(buffer, 1))
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, ch)
			r.mu.Unlock()
			close(ch)
		})
	}
}

func (r *Runner) emit(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// checkSupported rejects harnesses that grant something this runner
// cannot enforce yet. Running them anyway would mean running something
// other than what the user approved.
func checkSupported(h *spec.Harness) error {
	s := h.Spec
	p := s.Permissions
	var errs []error
	notYet := func(what string) {
		errs = append(errs, fmt.Errorf("%s is not supported by this runner yet", what))
	}
	if len(p.Network.Egress) > 0 || p.Network.AllowPrivateNetworks {
		notYet("network access (needs the egress proxy)")
	}
	if len(p.Credentials) > 0 {
		notYet("credentials (needs the credential broker)")
	}
	if len(p.Tools.Install) > 0 {
		notYet("installing tools")
	}
	if p.Subagents.Max > 0 {
		notYet("subagents")
	}
	if len(s.Approvals.RequireFor) > 0 {
		notYet("approval gates")
	}
	if s.Trigger.Schedule != "" || s.Trigger.Loop != nil {
		notYet("scheduled and looping triggers")
	}
	if len(errs) > 0 {
		return fmt.Errorf("runner: %w", errors.Join(errs...))
	}
	return nil
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "job-" + hex.EncodeToString(b)
}
