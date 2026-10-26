package runner

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/cron"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Schedules start jobs without anyone watching, so they are careful:
//
//   - runs never overlap: a cron run that is due while the previous one
//     is still going is skipped (and said so),
//   - missed runs are not caught up: after downtime, the next run is the
//     next one due from now,
//   - a cron schedule must not fire more often than MinScheduleInterval,
//   - a loop stops at its first run that does not succeed, when a run
//     reports the task done, or after maxIterations runs,
//   - the kill switch pauses every schedule; Resume does not unpause
//     them, each must be resumed on purpose,
//   - every run is an ordinary job with the harness's own limits.

// Schedule states.
const (
	ScheduleActive   = "active"
	SchedulePaused   = "paused"
	ScheduleFinished = "finished"
)

// Errors for schedules.
var (
	ErrNoSchedule   = errors.New("runner: no such schedule")
	ErrNotTriggered = errors.New("runner: this harness has no trigger (schedule or loop): run it as a job")
)

// Defaults for schedules.
const (
	DefaultMinScheduleInterval = 15 * time.Minute
	MaxSchedules               = 100
)

// Schedule is a standing order to run a harness: on a cron schedule, or
// in a loop.
type Schedule struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	// Cron is set for cron schedules (times in UTC).
	Cron string `json:"cron,omitempty"`
	// MaxRuns and Interval are set for loops.
	MaxRuns  int           `json:"maxRuns,omitempty"`
	Interval time.Duration `json:"interval,omitempty"`
	State    string        `json:"state"`
	Created  time.Time     `json:"created"`
	// NextRun is when the next run is due (zero while a loop's run is
	// going, and when not active).
	NextRun time.Time `json:"nextRun,omitzero"`
	Runs    int       `json:"runs"`
	LastJob string    `json:"lastJob,omitempty"`
	// Message says why the schedule is paused or finished, or what
	// happened at the last due time.
	Message string `json:"message,omitempty"`
	// Document is the harness as submitted.
	Document []byte `json:"document"`
}

// ScheduleStore persists schedules, like Store does jobs.
type ScheduleStore interface {
	SaveSchedule(Schedule) error
	LoadSchedules() ([]Schedule, error)
	DeleteSchedule(id string) error
}

type schedule struct {
	Schedule
	h    *spec.Harness
	cron *cron.Schedule
}

func (s *schedule) loop() bool { return s.cron == nil }

// AddSchedule adds a harness that has a trigger as a schedule.
func (r *Runner) AddSchedule(doc []byte) (Schedule, error) {
	h, err := spec.Parse(doc)
	if err != nil {
		return Schedule{}, fmt.Errorf("runner: invalid harness: %w", err)
	}
	if err := r.check(h); err != nil {
		return Schedule{}, err
	}
	if !triggered(h) {
		return Schedule{}, ErrNotTriggered
	}
	s, err := r.newSchedule(Schedule{Harness: h.Metadata.Name, Document: doc, State: ScheduleActive}, h)
	if err != nil {
		return Schedule{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.closed:
		return Schedule{}, ErrClosed
	case r.killed:
		return Schedule{}, ErrKilled
	case len(r.schedules) >= MaxSchedules:
		return Schedule{}, fmt.Errorf("runner: at most %d schedules", MaxSchedules)
	}
	s.ID = scheduleID(r.cfg.NewID())
	for r.schedules[s.ID] != nil || r.jobs[s.ID] != nil {
		s.ID = scheduleID(r.cfg.NewID())
	}
	s.Created = r.cfg.Now()
	r.planLocked(s, s.Created)
	r.schedules[s.ID] = s
	r.saveScheduleLocked(s)
	r.wakeScheduler()
	return s.Schedule, nil
}

// scheduleID turns a new job ID into a schedule ID ("sch-..."), so the
// two are never confused.
func scheduleID(jobID string) string {
	return "sch-" + strings.TrimPrefix(jobID, "job-")
}

// newSchedule fills in a schedule's trigger from its harness.
func (r *Runner) newSchedule(sc Schedule, h *spec.Harness) (*schedule, error) {
	s := &schedule{Schedule: sc, h: h}
	t := h.Spec.Trigger
	if t.Schedule != "" {
		c, err := cron.Parse(t.Schedule)
		if err != nil {
			return nil, err
		}
		if gap := c.MinGap(r.cfg.Now()); gap < r.cfg.MinScheduleInterval {
			return nil, fmt.Errorf("runner: the schedule %q runs every %s; this runner allows at most one run per %s",
				t.Schedule, gap, r.cfg.MinScheduleInterval)
		}
		s.cron, s.Cron = c, c.String()
	} else {
		s.MaxRuns, s.Interval = t.Loop.MaxIterations, t.Loop.Interval.Duration
	}
	return s, nil
}

// planLocked sets an active schedule's next run, from now.
func (r *Runner) planLocked(s *schedule, now time.Time) {
	switch {
	case s.State != ScheduleActive:
		s.NextRun = time.Time{}
	case !s.loop():
		s.NextRun = s.cron.Next(now)
	case s.Runs == 0:
		s.NextRun = now
	default:
		if j := r.jobs[s.LastJob]; j != nil && !j.State.Terminal() {
			s.NextRun = time.Time{} // the run's end plans the next one
		} else {
			s.NextRun = now.Add(s.Interval)
		}
	}
}

// Schedules returns all schedules, oldest first.
func (r *Runner) Schedules() []Schedule {
	r.mu.Lock()
	out := make([]Schedule, 0, len(r.schedules))
	for _, s := range r.schedules {
		out = append(out, s.Schedule)
	}
	r.mu.Unlock()
	slices.SortStableFunc(out, func(a, b Schedule) int { return a.Created.Compare(b.Created) })
	return out
}

// GetSchedule returns one schedule.
func (r *Runner) GetSchedule(id string) (Schedule, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.schedules[id]
	if !ok {
		return Schedule{}, false
	}
	return s.Schedule, true
}

// PauseSchedule stops a schedule from starting runs (a running one goes
// on; cancel it separately).
func (r *Runner) PauseSchedule(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.schedules[id]
	if !ok {
		return ErrNoSchedule
	}
	if s.State == ScheduleActive {
		s.State, s.Message = SchedulePaused, "paused"
		r.planLocked(s, r.cfg.Now())
		r.saveScheduleLocked(s)
	}
	return nil
}

// ResumeSchedule makes a paused schedule active again.
func (r *Runner) ResumeSchedule(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.schedules[id]
	switch {
	case !ok:
		return ErrNoSchedule
	case s.State == ScheduleFinished:
		return errors.New("runner: the schedule has finished; add the harness again")
	case r.killed:
		return ErrKilled
	case s.State == SchedulePaused:
		s.State, s.Message = ScheduleActive, ""
		r.planLocked(s, r.cfg.Now())
		r.saveScheduleLocked(s)
		r.wakeScheduler()
	}
	return nil
}

// DeleteSchedule removes a schedule. Its jobs stay (and a running one
// goes on; cancel it separately).
func (r *Runner) DeleteSchedule(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.schedules[id]; !ok {
		return ErrNoSchedule
	}
	delete(r.schedules, id)
	if r.cfg.ScheduleStore != nil {
		if err := r.cfg.ScheduleStore.DeleteSchedule(id); err != nil {
			r.cfg.Log.Error("schedule not deleted from the store", "schedule", id, "err", err)
		}
	}
	return nil
}

// pauseAllLocked pauses every active schedule (kill switch).
func (r *Runner) pauseAllLocked(why string) {
	for _, s := range r.schedules {
		if s.State == ScheduleActive {
			s.State, s.Message = SchedulePaused, why
			r.planLocked(s, r.cfg.Now())
			r.saveScheduleLocked(s)
		}
	}
}

func (r *Runner) saveScheduleLocked(s *schedule) {
	if r.cfg.ScheduleStore == nil {
		return
	}
	if err := r.cfg.ScheduleStore.SaveSchedule(s.Schedule); err != nil {
		r.cfg.Log.Error("schedule not saved", "schedule", s.ID, "err", err)
	}
}

func (r *Runner) wakeScheduler() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// scheduler starts due runs until the runner shuts down.
func (r *Runner) scheduler() {
	defer close(r.schedulerDone)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		r.fireDue()
		wait := time.Hour
		r.mu.Lock()
		now := r.cfg.Now()
		for _, s := range r.schedules {
			if !s.NextRun.IsZero() {
				wait = min(wait, max(s.NextRun.Sub(now), 0))
			}
		}
		r.mu.Unlock()
		timer.Reset(wait)
		select {
		case <-r.stopScheduler:
			return
		case <-r.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

// fireDue starts the runs that are due.
func (r *Runner) fireDue() {
	r.mu.Lock()
	now := r.cfg.Now()
	var due []*schedule
	for _, s := range r.schedules {
		if s.State == ScheduleActive && !s.NextRun.IsZero() && !now.Before(s.NextRun) {
			due = append(due, s)
		}
	}
	r.mu.Unlock()
	slices.SortFunc(due, func(a, b *schedule) int { return a.NextRun.Compare(b.NextRun) })
	for _, s := range due {
		r.fire(s, now)
	}
}

func (r *Runner) fire(s *schedule, now time.Time) {
	r.mu.Lock()
	if r.schedules[s.ID] != s || s.State != ScheduleActive || s.NextRun.IsZero() || now.Before(s.NextRun) {
		r.mu.Unlock()
		return // changed meanwhile, or already started
	}
	if j := r.jobs[s.LastJob]; j != nil && !j.State.Terminal() {
		s.Message = fmt.Sprintf("skipped the run due at %s: the previous run (%s) was still going", s.NextRun.UTC().Format(time.RFC3339), j.ID)
		r.planLocked(s, now)
		r.saveScheduleLocked(s)
		r.mu.Unlock()
		return
	}
	var loop *TaskLoop
	if s.loop() {
		loop = &TaskLoop{Iteration: s.Runs + 1, MaxIterations: s.MaxRuns}
	}
	s.NextRun = time.Time{} // not due again while this run starts
	h := s.h
	r.firing++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.firing--
		r.mu.Unlock()
	}()

	// Checked again for every run: the runner's configuration may have
	// changed since the schedule was added.
	err := r.check(h)
	if err != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.schedules[s.ID] == s {
			s.State, s.Message = SchedulePaused, "paused: this runner can no longer run it: "+err.Error()
			r.planLocked(s, now)
			r.saveScheduleLocked(s)
		}
		return
	}
	id, err := r.enqueue(h, enqueueOpts{scheduleID: s.ID, loop: loop})

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.schedules[s.ID] != s {
		return // deleted meanwhile; its job (if any) runs on
	}
	if err != nil {
		s.Message = "a run could not start: " + err.Error()
		if s.loop() {
			s.State = ScheduleFinished
		}
	} else {
		s.Runs++
		s.LastJob, s.Message = id, ""
		// A quick run may have ended before it was recorded here; its end
		// was not counted yet.
		if j := r.jobs[id]; j != nil && j.State.Terminal() {
			r.runEndedLocked(j)
			return
		}
	}
	r.planLocked(s, now)
	r.saveScheduleLocked(s)
}

// runEndedLocked plans a loop's next run when one of its runs ends.
func (r *Runner) runEndedLocked(j *job) {
	s := r.schedules[j.Schedule]
	if s == nil || !s.loop() || s.LastJob != j.ID {
		return
	}
	switch {
	case j.State != StateSucceeded:
		s.State, s.Message = ScheduleFinished, fmt.Sprintf("stopped: run %d ended %s", s.Runs, j.State)
	case j.LoopDone:
		s.State, s.Message = ScheduleFinished, fmt.Sprintf("done: run %d reported the task complete", s.Runs)
	case s.Runs >= s.MaxRuns:
		s.State, s.Message = ScheduleFinished, fmt.Sprintf("stopped after %d runs (maxIterations) without the task reported complete", s.Runs)
	}
	r.planLocked(s, r.cfg.Now())
	r.saveScheduleLocked(s)
	r.wakeScheduler()
}

// loadSchedules restores stored schedules (after the jobs). Nothing is
// caught up: cron schedules continue from now; a loop whose run was
// interrupted has failed and stops.
func (r *Runner) loadSchedules() error {
	stored, err := r.cfg.ScheduleStore.LoadSchedules()
	if err != nil {
		return fmt.Errorf("runner: load schedules: %w", err)
	}
	now := r.cfg.Now()
	for _, sc := range stored {
		h, err := spec.Parse(sc.Document)
		var s *schedule
		if err == nil {
			s, err = r.newSchedule(sc, h)
		}
		if err != nil {
			r.cfg.Log.Error("skipping a stored schedule that is no longer valid", "schedule", sc.ID, "err", err)
			continue
		}
		if s.loop() && s.State == ScheduleActive {
			if j := r.jobs[s.LastJob]; j != nil && j.State != StateSucceeded {
				s.State, s.Message = ScheduleFinished, fmt.Sprintf("stopped: run %d ended %s", s.Runs, j.State)
			}
		}
		if s.State == ScheduleActive {
			if err := r.check(h); err != nil {
				s.State, s.Message = SchedulePaused, "paused: this runner can no longer run it: "+err.Error()
			}
		}
		r.planLocked(s, now)
		r.schedules[s.ID] = s
		r.saveScheduleLocked(s)
	}
	return nil
}
