// Package runner executes harnesses in sandboxes: it queues jobs in
// submission order, enforces a concurrency limit, tracks each job through
// its state machine, and offers a per-job cancel and a global kill switch.
//
// It boots the harness's image and treats a clean guest shutdown as
// success. Network access and credentials go through a per-job egress
// proxy (Config.Egress). Agent runtimes and schedules are not wired in
// yet: harnesses that need them are rejected (fail closed) rather than
// run with different behaviour than the user approved.
package runner

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

	"github.com/first-it-consulting/harness-control-center/internal/budget"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/results"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Errors returned by the runner.
var (
	ErrKilled   = errors.New("runner: kill switch is engaged; call Resume to accept jobs again")
	ErrClosed   = errors.New("runner: shut down")
	ErrNotFound = errors.New("runner: no such job")
	// ErrTriggered: a harness with a schedule or loop is added as a
	// schedule (AddSchedule), not run directly.
	ErrTriggered = errors.New("runner: this harness has a trigger (schedule or loop): add it as a schedule")
)

// Config configures a Runner.
type Config struct {
	Provider sandbox.Provider
	// Target is this runner's own target name as used in spec.target:
	// "local" (default) or "server:<name>". Harnesses addressed to any
	// other target are rejected.
	Target string
	// Images are the spec.sandbox.image names the provider can boot.
	// Unknown names are rejected.
	Images []string
	// MaxConcurrent bounds running jobs (default 1).
	MaxConcurrent int
	// Console returns where a job's console output goes (default:
	// discard). If the writer is also an io.Closer, it is closed when the
	// job ends.
	Console func(jobID string) io.Writer
	// Egress returns the egress proxy for a job whose harness grants
	// network access; the proxy enforces meter (the job's budget) for the
	// model traffic it brokers. Without it, such harnesses are rejected.
	// approve decides the requests its approval gates hold.
	Egress func(h *spec.Harness, jobID string, meter *budget.Meter, approve Approver) (sandbox.Proxy, error)
	// ApprovalTimeout bounds how long a gated request waits for a
	// decision (default DefaultApprovalTimeout).
	ApprovalTimeout time.Duration
	// Pricing gives models a price (USD per million tokens), so that
	// budget.maxCostUSD can be enforced for them; unpriced models are
	// limited by tokens only.
	Pricing map[string]budget.Price
	// Results stores what a job hands back: the raw contents of its disk
	// (spec.sandbox.diskMiB), read after the guest shut itself down. They
	// are hostile input (see internal/results); at most maxBytes of files
	// may be kept. Without it, jobs get no disk.
	Results func(jobID string, disk io.Reader, maxBytes int64) (results.Summary, error)
	// MaxDiskMiB bounds spec.sandbox.diskMiB (default DefaultMaxDiskMiB).
	MaxDiskMiB int
	// Store, if set, persists every job (on each state change), so the
	// history survives a restart. Jobs that were not finished when the
	// runner stopped are marked failed on load.
	Store Store
	// Recover, if set, is called once with those unfinished jobs' IDs, to
	// clean up whatever their sandboxes left behind.
	Recover func(ctx context.Context, ids []string) error
	// Log receives problems that do not belong to a single caller, such
	// as a job that could not be saved (default: discarded).
	Log *slog.Logger
	// ScheduleStore, if set, persists schedules.
	ScheduleStore ScheduleStore
	// MinScheduleInterval bounds how often a cron schedule may fire
	// (default DefaultMinScheduleInterval).
	MinScheduleInterval time.Duration
	// Now and NewID are injectable for tests.
	Now   func() time.Time
	NewID func() string
}

type job struct {
	Job
	ctx     context.Context
	cancel  context.CancelFunc
	spec    sandbox.Spec
	history []Event // every event of this job, oldest first (at most one per state)
	meter   *budget.Meter
	// overBudget is why the meter stopped the job, if it did.
	overBudget string
}

// Runner runs jobs. It is safe for concurrent use.
type Runner struct {
	cfg Config
	wg  sync.WaitGroup // one per job not yet terminal

	mu      sync.Mutex
	jobs    map[string]*job
	queue   []*job // queued jobs, oldest first
	running int
	subs    map[chan Event]struct{}
	killed  bool
	closed  bool
	// root is cancelled by KillAll and Shutdown; every job derives from
	// the root that was current when it was submitted.
	root       context.Context
	cancelRoot context.CancelFunc

	approvals     map[string]*pendingApproval
	schedules     map[string]*schedule
	wake          chan struct{}
	stopScheduler chan struct{}
	schedulerDone chan struct{}
	stopOnce      sync.Once
}

// New returns a Runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Provider == nil {
		return nil, errors.New("runner: Provider is required")
	}
	if cfg.Target == "" {
		cfg.Target = spec.DefaultTarget
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.MaxDiskMiB <= 0 {
		cfg.MaxDiskMiB = DefaultMaxDiskMiB
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
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.ApprovalTimeout <= 0 {
		cfg.ApprovalTimeout = DefaultApprovalTimeout
	}
	if cfg.MinScheduleInterval <= 0 {
		cfg.MinScheduleInterval = DefaultMinScheduleInterval
	}
	r := &Runner{cfg: cfg, jobs: map[string]*job{}, subs: map[chan Event]struct{}{},
		schedules: map[string]*schedule{}, wake: make(chan struct{}, 1), approvals: map[string]*pendingApproval{},
		stopScheduler: make(chan struct{}), schedulerDone: make(chan struct{})}
	r.root, r.cancelRoot = context.WithCancel(context.Background())
	if cfg.Store != nil {
		if err := r.load(); err != nil {
			return nil, err
		}
	}
	if cfg.ScheduleStore != nil {
		if err := r.loadSchedules(); err != nil {
			return nil, err
		}
	}
	go r.scheduler()
	return r, nil
}

// load restores the stored jobs. A job that was not finished cannot be
// resumed (its sandbox died with the previous process, or must be
// stopped now): it is marked failed, and Recover cleans up after it.
func (r *Runner) load() error {
	jobs, err := r.cfg.Store.Load()
	if err != nil {
		return fmt.Errorf("runner: load jobs: %w", err)
	}
	var unfinished []string
	for _, sj := range jobs {
		if sandbox.ValidateID(sj.ID) != nil || r.jobs[sj.ID] != nil {
			continue
		}
		if !sj.State.Terminal() {
			unfinished = append(unfinished, sj.ID)
			sj.Error = fmt.Sprintf("the runner stopped while the job was %s", sj.State)
			sj.State = StateFailed
			sj.Finished = r.cfg.Now()
			r.save(sj)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		j := &job{Job: sj, ctx: ctx, cancel: cancel}
		// The detailed event history is not kept: one event stands for it.
		j.history = []Event{{JobID: sj.ID, Harness: sj.Harness, To: sj.State, At: sj.Finished, Message: sj.Error}}
		r.jobs[sj.ID] = j
	}
	if len(unfinished) > 0 && r.cfg.Recover != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := r.cfg.Recover(ctx, unfinished); err != nil {
			r.cfg.Log.Error("cleaning up after unfinished jobs failed", "jobs", unfinished, "err", err)
		} else {
			r.cfg.Log.Warn("cleaned up after jobs that were unfinished when the runner stopped", "jobs", unfinished)
		}
	}
	return nil
}

// save persists a job. Failing to save does not stop the job; it is
// logged, and the next state change tries again.
func (r *Runner) save(j Job) {
	if r.cfg.Store == nil {
		return
	}
	if err := r.cfg.Store.Save(j); err != nil {
		r.cfg.Log.Error("job not saved", "job", j.ID, "err", err)
	}
}

// Submit validates h against what this runner can enforce, queues it and
// returns the job ID.
func (r *Runner) Submit(h *spec.Harness) (string, error) {
	if err := r.check(h); err != nil {
		return "", err
	}
	if triggered(h) {
		return "", ErrTriggered
	}
	return r.enqueue(h, "", nil)
}

func triggered(h *spec.Harness) bool {
	return h.Spec.Trigger.Schedule != "" || h.Spec.Trigger.Loop != nil
}

// check validates h against what this runner can enforce.
func (r *Runner) check(h *spec.Harness) error {
	if h == nil {
		return errors.New("runner: nil harness")
	}
	if err := h.Validate(); err != nil {
		return fmt.Errorf("runner: invalid harness: %w", err)
	}
	if h.Spec.Target != r.cfg.Target {
		return fmt.Errorf("runner: harness targets %q, this runner is %q", h.Spec.Target, r.cfg.Target)
	}
	// Credentials only ever travel through the egress proxy, so they need
	// it too (and the factory checks their hosts against the allowlist).
	if needsNetwork(h) && r.cfg.Egress == nil {
		return errors.New("runner: network access is not configured on this runner")
	}
	if err := checkSupported(h); err != nil {
		return err
	}
	if !slices.Contains(r.cfg.Images, h.Spec.Sandbox.Image) {
		return fmt.Errorf("runner: unknown image %q", h.Spec.Sandbox.Image)
	}
	if r.cfg.Results != nil && h.Spec.Sandbox.DiskMiB > r.cfg.MaxDiskMiB {
		return fmt.Errorf("runner: spec.sandbox.diskMiB %d exceeds this runner's limit of %d", h.Spec.Sandbox.DiskMiB, r.cfg.MaxDiskMiB)
	}
	return nil
}

func needsNetwork(h *spec.Harness) bool {
	return len(h.Spec.Permissions.Network.Egress) > 0 || len(h.Spec.Permissions.Credentials) > 0
}

// enqueue queues a checked harness as a job; scheduleID and loop are set
// for runs started by a schedule.
func (r *Runner) enqueue(h *spec.Harness, scheduleID string, loop *TaskLoop) (string, error) {
	needsNet := needsNetwork(h)
	if needsNet && r.cfg.Egress == nil { // check does this too; never call a missing factory
		return "", errors.New("runner: network access is not configured on this runner")
	}
	diskMiB := 0
	if r.cfg.Results != nil {
		diskMiB = h.Spec.Sandbox.DiskMiB
	}
	task, err := taskFor(h, loop)
	if err != nil {
		return "", err
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
	for r.jobs[id] != nil { // a stored job may hold it
		id = r.cfg.NewID()
	}
	var (
		network *sandbox.Network
		meter   *budget.Meter
		j       *job
	)
	if needsNet {
		meter = budget.New(budget.Limits{MaxTokens: h.Spec.Budget.MaxTokens, MaxCostUSD: h.Spec.Budget.MaxCostUSD},
			r.price(h), func(reason string) { r.stopOverBudget(j, reason) })
		approve := func(ctx context.Context, req ApprovalRequest) error { return r.awaitApproval(ctx, j, req) }
		proxy, err := r.cfg.Egress(h, id, meter, approve)
		if err != nil {
			r.mu.Unlock()
			return "", fmt.Errorf("runner: egress proxy: %w", err)
		}
		network = &sandbox.Network{Proxy: proxy}
	}
	ctx, cancel := context.WithCancel(r.root)
	j = &job{
		Job:    Job{ID: id, Harness: h.Metadata.Name, State: StateQueued, Created: r.cfg.Now(), Schedule: scheduleID},
		ctx:    ctx,
		cancel: cancel,
		spec: sandbox.Spec{
			ID:        id,
			Image:     h.Spec.Sandbox.Image,
			Task:      task,
			CPUs:      h.Spec.Sandbox.CPUs,
			MemoryMiB: h.Spec.Sandbox.MemoryMiB,
			Timeout:   h.Spec.Sandbox.Timeout.Duration,
			Network:   network,
			DiskMiB:   diskMiB,
		},
		meter: meter,
	}
	if diskMiB > 0 {
		j.spec.Collect = func(disk io.Reader) error { return r.collect(j, disk) }
	}
	r.jobs[id] = j
	r.queue = append(r.queue, j)
	r.wg.Add(1)
	r.emitLocked(j, Event{JobID: id, Harness: j.Harness, To: StateQueued, At: j.Created})
	r.save(j.snapshot())
	r.mu.Unlock()

	r.dispatch()
	return id, nil
}

// dispatch starts queued jobs, oldest first, while slots are free.
func (r *Runner) dispatch() {
	r.mu.Lock()
	var start []*job
	for r.running < r.cfg.MaxConcurrent && len(r.queue) > 0 {
		j := r.queue[0]
		r.queue = r.queue[1:]
		r.running++
		start = append(start, j)
	}
	r.mu.Unlock()
	for _, j := range start {
		go r.execute(j)
	}
}

func (r *Runner) execute(j *job) {
	defer func() {
		j.cancel()
		r.mu.Lock()
		r.running--
		r.mu.Unlock()
		r.wg.Done()
		r.dispatch()
	}()

	r.transition(j, StateProvisioning, "")
	if j.ctx.Err() != nil {
		r.transition(j, StateCancelled, "cancelled before start")
		return
	}
	r.transition(j, StateRunning, "")

	result := &resultWatcher{}
	console := r.cfg.Console(j.ID)
	res, err := r.cfg.Provider.Run(j.ctx, j.spec, io.MultiWriter(console, result))
	if c, ok := console.(io.Closer); ok {
		if cerr := c.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("console: %w", cerr)
		}
	}
	r.mu.Lock()
	j.LoopDone = result.loopDone()
	r.mu.Unlock()
	switch {
	case err != nil:
		r.transition(j, StateFailed, err.Error())
	case res.Outcome == sandbox.OutcomeExited && result.exitCode() > 0:
		r.transition(j, StateFailed, fmt.Sprintf("agent exited with status %d", result.exitCode()))
	case res.Outcome == sandbox.OutcomeExited:
		r.transition(j, StateSucceeded, "")
	case res.Outcome == sandbox.OutcomeTimedOut:
		r.transition(j, StateTimedOut, fmt.Sprintf("stopped after %s", j.spec.Timeout))
	case res.Outcome == sandbox.OutcomeKilled && r.overBudgetReason(j) != "":
		r.transition(j, StateOverBudget, "budget exceeded: "+r.overBudgetReason(j))
	case res.Outcome == sandbox.OutcomeKilled:
		r.transition(j, StateCancelled, "stopped")
	default:
		r.transition(j, StateFailed, "sandbox reported "+res.Outcome.String())
	}
}

func (r *Runner) transition(j *job, to State, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitionLocked(j, to, msg)
}

// transitionLocked moves j to state if allowed (otherwise it is a no-op)
// and emits the event. Emitting under the lock keeps every job's events
// in order.
func (r *Runner) transitionLocked(j *job, to State, msg string) {
	from := j.State
	if !canTransition(from, to) {
		return
	}
	now := r.cfg.Now()
	j.State = to
	if to == StateRunning {
		j.Started = now
	}
	if to.Terminal() {
		j.Finished = now
		if to != StateSucceeded {
			j.Error = msg
		}
	}
	r.emitLocked(j, Event{JobID: j.ID, Harness: j.Harness, From: from, To: to, At: now, Message: msg})
	r.save(j.snapshot())
	if to.Terminal() && j.Schedule != "" {
		r.runEndedLocked(j)
	}
}

// Prune forgets finished jobs, oldest first, that finished more than
// maxAge ago or exceed maxJobs (0: no limit), deleting them from the
// Store. Unfinished jobs are never pruned. It returns the pruned IDs.
func (r *Runner) Prune(maxJobs int, maxAge time.Duration) []string {
	r.mu.Lock()
	var done []*job
	for _, j := range r.jobs {
		if j.State.Terminal() {
			done = append(done, j)
		}
	}
	slices.SortFunc(done, func(a, b *job) int { return a.Created.Compare(b.Created) })
	now := r.cfg.Now()
	var pruned []string
	for i, j := range done {
		tooMany := maxJobs > 0 && len(done)-i > maxJobs
		tooOld := maxAge > 0 && now.Sub(j.Finished) > maxAge
		if tooMany || tooOld {
			delete(r.jobs, j.ID)
			pruned = append(pruned, j.ID)
		}
	}
	r.mu.Unlock()
	if r.cfg.Store != nil {
		for _, id := range pruned {
			if err := r.cfg.Store.Delete(id); err != nil {
				r.cfg.Log.Error("pruned job not deleted", "job", id, "err", err)
			}
		}
	}
	return pruned
}

// cancelQueuedLocked removes the queued jobs for which match returns true
// and cancels them. They never reach the provider.
func (r *Runner) cancelQueuedLocked(match func(*job) bool, msg string) {
	remaining := r.queue[:0]
	for _, j := range r.queue {
		if !match(j) {
			remaining = append(remaining, j)
			continue
		}
		j.cancel()
		r.transitionLocked(j, StateCancelled, msg)
		r.wg.Done()
	}
	r.queue = remaining
}

// Cancel stops one job, queued or running.
func (r *Runner) Cancel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return ErrNotFound
	}
	// Always cancel the context: a job dispatch already took off the queue
	// still reads "queued" until execute moves it on, and execute checks
	// the context before starting the sandbox.
	j.cancel()
	r.cancelQueuedLocked(func(q *job) bool { return q == j }, "cancelled while queued")
	return nil
}

// KillAll is the kill switch: it stops every queued and running job and
// refuses new ones until Resume is called.
func (r *Runner) KillAll() {
	r.mu.Lock()
	r.killed = true
	r.pauseAllLocked("paused by the kill switch")
	r.cancelQueuedLocked(func(*job) bool { return true }, "cancelled by kill switch")
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
	r.stopOnce.Do(func() { close(r.stopScheduler) })
	<-r.schedulerDone
	// Not KillAll: shutting down must not pause the schedules, which
	// continue when the runner starts again.
	r.mu.Lock()
	r.killed = true
	r.cancelQueuedLocked(func(*job) bool { return true }, "cancelled: the runner is shutting down")
	cancel := r.cancelRoot
	r.root, r.cancelRoot = context.WithCancel(context.Background())
	r.mu.Unlock()
	cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("runner: shutdown: %w", ctx.Err())
	}
}

// Status reports the runner's configuration and load.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	images := slices.Clone(r.cfg.Images)
	slices.Sort(images)
	return Status{
		Target: r.cfg.Target, Images: images, MaxConcurrent: r.cfg.MaxConcurrent, MaxDiskMiB: r.cfg.MaxDiskMiB,
		Running: r.running, Queued: len(r.queue), Killed: r.killed, Schedules: len(r.schedules),
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
	return j.snapshot(), true
}

func (j *job) snapshot() Job {
	s := j.Job
	if j.meter != nil {
		u := j.meter.Usage()
		s.Usage = &u
	}
	return s
}

// collect stores a job's results. A problem with them is reported on the
// job, not as a failed run: the agent did finish.
func (r *Runner) collect(j *job, disk io.Reader) error {
	sum, err := r.cfg.Results(j.ID, disk, int64(j.spec.DiskMiB)<<20)
	res := &Results{Summary: sum}
	if err != nil {
		res.Error = err.Error()
	}
	r.mu.Lock()
	j.Results = res
	r.mu.Unlock()
	return nil
}

// price is the price of h's model, if the runner knows it.
func (r *Runner) price(h *spec.Harness) *budget.Price {
	if h.Spec.Model == nil {
		return nil
	}
	if p, ok := r.cfg.Pricing[h.Spec.Model.Name]; ok {
		return &p
	}
	return nil
}

// stopOverBudget stops a job whose budget is exhausted.
func (r *Runner) stopOverBudget(j *job, reason string) {
	if j == nil {
		return
	}
	r.mu.Lock()
	if j.overBudget == "" {
		j.overBudget = reason
	}
	r.mu.Unlock()
	j.cancel()
}

func (r *Runner) overBudgetReason(j *job) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return j.overBudget
}

// List returns snapshots of all jobs, oldest first.
func (r *Runner) List() []Job {
	r.mu.Lock()
	out := make([]Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j.snapshot())
	}
	r.mu.Unlock()
	slices.SortStableFunc(out, func(a, b Job) int { return a.Created.Compare(b.Created) })
	return out
}

// Subscribe returns a stream of events and a function to stop it.
// Delivery never blocks the runner: a subscriber that falls more than
// buffer events behind loses events.
func (r *Runner) Subscribe(buffer int) (<-chan Event, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subscribeLocked(buffer)
}

func (r *Runner) subscribeLocked(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, max(buffer, 1))
	r.subs[ch] = struct{}{}
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

// Watch returns job id's events so far and, atomically with that, a
// subscription to all later events (of every job; filter by JobID). A
// caller therefore sees each of the job's events exactly once, however
// late it starts watching.
func (r *Runner) Watch(id string, buffer int) (history []Event, events <-chan Event, stop func(), err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, nil, nil, ErrNotFound
	}
	ch, stop := r.subscribeLocked(buffer)
	return slices.Clone(j.history), ch, stop, nil
}

func (r *Runner) emitLocked(j *job, e Event) {
	j.history = append(j.history, e)
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
	if len(p.Tools.Install) > 0 {
		notYet("installing tools")
	}
	if p.Subagents.Max > 0 {
		notYet("subagents")
	}
	if err := policy.CheckApprovalPatterns(s.Approvals.RequireFor); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("runner: %w", errors.Join(errs...))
	}
	return nil
}

// resultMarker is printed by an image's init after its agent ends, e.g.
// "HCC-RESULT-END exit=0". The last one counts: init prints it after the
// agent (whose own output might contain the text) has exited.
const resultMarker = "HCC-RESULT-END exit="

// LoopDoneMarker is printed by an image's init, before the result marker,
// when a looping harness's agent reported that the whole task is done.
// Only a marker followed by a result marker counts, so the agent cannot
// fake it by printing it itself mid-run (init prints both after the
// agent exited).
const LoopDoneMarker = "HCC-LOOP-DONE"

// resultWatcher reads the agent's exit status from the console. It
// keeps only the current line, so memory stays bounded.
type resultWatcher struct {
	mu   sync.Mutex
	line []byte
	code int // valid once seen
	seen bool
	// doneLine: the previous line was LoopDoneMarker; done: it was
	// directly followed by the result marker.
	doneLine, done bool
}

func (w *resultWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b != '\n' {
			if len(w.line) < 256 {
				w.line = append(w.line, b)
			}
			continue
		}
		line := strings.TrimSpace(strings.TrimRight(string(w.line), "\r"))
		w.line = w.line[:0]
		if rest, ok := strings.CutPrefix(line, resultMarker); ok {
			if n, err := strconv.Atoi(rest); err == nil {
				w.code, w.seen = n, true
				w.done = w.doneLine
			}
		}
		w.doneLine = line == LoopDoneMarker
	}
	return len(p), nil
}

// loopDone reports whether init said the loop's task is done.
func (w *resultWatcher) loopDone() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.done
}

// exitCode is the agent's status, or 0 if the image reported none.
func (w *resultWatcher) exitCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.seen {
		return 0
	}
	return w.code
}

// Task is what the guest's runtime gets (as /dev/vdb). It never contains
// credentials: those are added by the egress proxy.
type Task struct {
	Runtime string     `json:"runtime"`
	Prompt  string     `json:"prompt"`
	Model   *TaskModel `json:"model,omitempty"`
	Loop    *TaskLoop  `json:"loop,omitempty"`
}

// TaskLoop tells a looping harness's runtime where it is. The runtime
// reports that the whole task is done by printing LoopDoneMarker.
type TaskLoop struct {
	Iteration     int `json:"iteration"`
	MaxIterations int `json:"maxIterations"`
}

// TaskModel tells the runtime which model to ask, and where.
type TaskModel struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

func taskFor(h *spec.Harness, loop *TaskLoop) ([]byte, error) {
	t := Task{Runtime: h.Spec.Runtime, Prompt: h.Spec.Prompt, Loop: loop}
	if m := h.Spec.Model; m != nil {
		t.Model = &TaskModel{Name: m.Name, Endpoint: m.Endpoint}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	if len(b) > sandbox.MaxTaskBytes {
		return nil, fmt.Errorf("runner: task is %d bytes; the limit is %d", len(b), sandbox.MaxTaskBytes)
	}
	return b, nil
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "job-" + hex.EncodeToString(b)
}
