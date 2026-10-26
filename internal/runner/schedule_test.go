package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(s string) {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type memSchedules struct {
	mu sync.Mutex
	m  map[string]Schedule
}

func (s *memSchedules) SaveSchedule(sc Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sc.ID] = sc
	return nil
}

func (s *memSchedules) LoadSchedules() ([]Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Schedule
	for _, sc := range s.m {
		out = append(out, sc)
	}
	return out, nil
}

func (s *memSchedules) DeleteSchedule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
	return nil
}

type schedEnv struct {
	t      *testing.T
	r      *Runner
	p      *fakeProvider
	clock  *fakeClock
	jobs   *memStore
	scheds *memSchedules
}

func newSchedEnv(t *testing.T, p *fakeProvider, jobs *memStore, scheds *memSchedules, clock *fakeClock) *schedEnv {
	t.Helper()
	var n atomic.Int32
	r, err := New(Config{
		Provider: p, Images: []string{"base"}, Now: clock.Now, Store: jobs, ScheduleStore: scheds,
		NewID: func() string { return fmt.Sprintf("id-%d-%d", time.Now().UnixNano(), n.Add(1)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	return &schedEnv{t: t, r: r, p: p, clock: clock, jobs: jobs, scheds: scheds}
}

func defaultSchedEnv(t *testing.T) *schedEnv {
	clock := &fakeClock{}
	clock.set("2026-10-25 10:30")
	return newSchedEnv(t, newFakeProvider(), newMemStore(), &memSchedules{m: map[string]Schedule{}}, clock)
}

func scheduleDoc(trigger string) []byte {
	return []byte(baseHarness + "  trigger: " + trigger + "\n")
}

// at moves the clock and starts whatever is due (here, or in the
// background scheduler, which may win the race).
func (e *schedEnv) at(when string) {
	e.clock.set(when)
	e.r.fireDue()
	e.settle()
}

// settle waits until no schedule run is being started.
func (e *schedEnv) settle() {
	for range 1000 {
		e.r.mu.Lock()
		n := e.r.firing
		e.r.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	e.t.Fatal("a schedule run did not finish starting")
}

func (e *schedEnv) sched(id string) Schedule {
	e.t.Helper()
	s, ok := e.r.GetSchedule(id)
	if !ok {
		e.t.Fatalf("schedule %s gone", id)
	}
	return s
}

func (e *schedEnv) waitJobEnd(id string) Job {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := e.r.Get(id); ok && j.State.Terminal() {
			// The schedule hears of the end in the same transition.
			return j
		}
		time.Sleep(2 * time.Millisecond)
	}
	e.t.Fatalf("job %s did not end", id)
	return Job{}
}

func TestTriggerSeparatesJobsAndSchedules(t *testing.T) {
	e := defaultSchedEnv(t)
	if _, err := e.r.Submit(harness(t, "  trigger: {schedule: '0 * * * *'}\n")); !errors.Is(err, ErrTriggered) {
		t.Errorf("Submit of a scheduled harness = %v", err)
	}
	if _, err := e.r.AddSchedule([]byte(baseHarness)); !errors.Is(err, ErrNotTriggered) {
		t.Errorf("AddSchedule without trigger = %v", err)
	}
	if _, err := e.r.AddSchedule(scheduleDoc("{schedule: '*/5 * * * *'}")); err == nil || !strings.Contains(err.Error(), "at most one run per 15m0s") {
		t.Errorf("too frequent schedule = %v", err)
	}
	if _, err := e.r.AddSchedule([]byte(strings.Replace(string(scheduleDoc("{schedule: '0 * * * *'}")), "image: base", "image: nope", 1))); err == nil {
		t.Error("a schedule the runner could never run was accepted")
	}
}

func TestCronScheduleRunsAndNeverOverlaps(t *testing.T) {
	e := defaultSchedEnv(t)
	e.p.block = true
	s, err := e.r.AddSchedule(scheduleDoc("{schedule: '0 * * * *'}"))
	if err != nil {
		t.Fatal(err)
	}
	if s.State != ScheduleActive || s.Cron != "0 * * * *" || !s.NextRun.Equal(time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("schedule = %+v", s)
	}
	e.at("2026-10-25 10:59")
	if len(e.r.List()) != 0 {
		t.Fatal("ran early")
	}
	e.at("2026-10-25 11:00")
	s = e.sched(s.ID)
	if s.Runs != 1 || s.LastJob == "" || !s.NextRun.Equal(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("after first run: %+v", s)
	}
	first := s.LastJob
	if j, _ := e.r.Get(first); j.Schedule != s.ID {
		t.Errorf("job not linked to its schedule: %+v", j)
	}
	e.r.fireDue() // the same due time again: no second start
	waitState(t, e.r, first, StateRunning)
	e.at("2026-10-25 12:00")
	s = e.sched(s.ID)
	if s.Runs != 1 || !strings.Contains(s.Message, "still going") || len(e.r.List()) != 1 {
		t.Errorf("overlapping run not skipped: %+v (%d jobs)", s, len(e.r.List()))
	}
	close(e.p.release)
	e.waitJobEnd(first)
	e.at("2026-10-25 13:00")
	if s = e.sched(s.ID); s.Runs != 2 || s.LastJob == first {
		t.Errorf("after the overlap: %+v", s)
	}
}

func TestLoopRunsUntilMaxIterations(t *testing.T) {
	e := defaultSchedEnv(t)
	s, err := e.r.AddSchedule(scheduleDoc("{loop: {maxIterations: 3, interval: 10m}}"))
	if err != nil {
		t.Fatal(err)
	}
	e.r.fireDue() // the first run is due at once
	for i := 1; i <= 3; i++ {
		s = e.sched(s.ID)
		if s.Runs != i {
			t.Fatalf("run %d: schedule %+v", i, s)
		}
		e.waitJobEnd(s.LastJob)
		s = e.sched(s.ID)
		if i < 3 {
			want := e.clock.Now().Add(10 * time.Minute)
			if s.State != ScheduleActive || !s.NextRun.Equal(want) {
				t.Fatalf("after run %d: %+v", i, s)
			}
			e.at(want.Format("2006-01-02 15:04"))
		}
	}
	if s.State != ScheduleFinished || !strings.Contains(s.Message, "after 3 runs") {
		t.Errorf("final = %+v", s)
	}
	// Each run knew where it was in the loop.
	specs := e.p.startedSpecs()
	var task Task
	if err := json.Unmarshal(specs[2].Task, &task); err != nil || task.Loop == nil || task.Loop.Iteration != 3 || task.Loop.MaxIterations != 3 {
		t.Errorf("task of run 3 = %+v, %v", task, err)
	}
}

func TestLoopStopsOnFailureOrDone(t *testing.T) {
	e := defaultSchedEnv(t)
	e.p.err = errors.New("boom")
	s, _ := e.r.AddSchedule(scheduleDoc("{loop: {maxIterations: 5}}"))
	e.r.fireDue()
	e.waitJobEnd(e.sched(s.ID).LastJob)
	if s = e.sched(s.ID); s.State != ScheduleFinished || !strings.Contains(s.Message, "ended failed") || s.Runs != 1 {
		t.Errorf("after a failed run: %+v", s)
	}

	e2 := defaultSchedEnv(t)
	e2.p.console = "agent output\n" + LoopDoneMarker + "\nHCC-RESULT-END exit=0\n"
	s, _ = e2.r.AddSchedule(scheduleDoc("{loop: {maxIterations: 5}}"))
	e2.r.fireDue()
	j := e2.waitJobEnd(e2.sched(s.ID).LastJob)
	if s = e2.sched(s.ID); s.State != ScheduleFinished || !strings.Contains(s.Message, "done") || !j.LoopDone {
		t.Errorf("after a done run: %+v (job %+v)", s, j)
	}
}

// The agent cannot end its loop by printing the marker itself: only a
// marker directly before init's result line counts.
func TestLoopDoneCannotBeFaked(t *testing.T) {
	for name, console := range map[string]string{
		"marker mid-output":           LoopDoneMarker + "\nmore agent output\nHCC-RESULT-END exit=0\n",
		"agent fakes both, then init": LoopDoneMarker + "\nHCC-RESULT-END exit=0\nHCC-RESULT-END exit=0\n",
	} {
		e := defaultSchedEnv(t)
		e.p.console = console
		s, _ := e.r.AddSchedule(scheduleDoc("{loop: {maxIterations: 1}}"))
		e.r.fireDue()
		if j := e.waitJobEnd(e.sched(s.ID).LastJob); j.LoopDone {
			t.Errorf("%s: counted as done", name)
		}
		if s = e.sched(s.ID); strings.Contains(s.Message, "done") {
			t.Errorf("%s: schedule = %+v", name, s.Message)
		}
	}
}

func TestKillSwitchPausesSchedules(t *testing.T) {
	e := defaultSchedEnv(t)
	s, _ := e.r.AddSchedule(scheduleDoc("{schedule: '0 * * * *'}"))
	e.r.KillAll()
	if s = e.sched(s.ID); s.State != SchedulePaused || !strings.Contains(s.Message, "kill switch") || !s.NextRun.IsZero() {
		t.Fatalf("after kill: %+v", s)
	}
	if err := e.r.ResumeSchedule(s.ID); !errors.Is(err, ErrKilled) {
		t.Errorf("resume while killed = %v", err)
	}
	e.r.Resume()
	e.at("2026-10-25 11:00")
	if len(e.r.List()) != 0 {
		t.Error("a paused schedule ran after Resume")
	}
	if err := e.r.ResumeSchedule(s.ID); err != nil {
		t.Fatal(err)
	}
	e.at("2026-10-25 12:00")
	if len(e.r.List()) != 1 {
		t.Errorf("jobs after resuming the schedule: %d", len(e.r.List()))
	}
	if err := e.r.PauseSchedule(s.ID); err != nil {
		t.Fatal(err)
	}
	e.at("2026-10-25 13:00")
	if len(e.r.List()) != 1 {
		t.Error("a paused schedule ran")
	}
	if err := e.r.DeleteSchedule(s.ID); err != nil || len(e.r.Schedules()) != 0 || len(e.scheds.m) != 0 {
		t.Errorf("delete: %v, %v", err, e.r.Schedules())
	}
}

func TestSchedulesSurviveRestartWithoutCatchUp(t *testing.T) {
	e := defaultSchedEnv(t)
	cronS, _ := e.r.AddSchedule(scheduleDoc("{schedule: '0 * * * *'}"))
	e.p.block = true
	loopS, _ := e.r.AddSchedule(scheduleDoc("{loop: {maxIterations: 5}}"))
	e.r.fireDue()
	waitState(t, e.r, e.sched(loopS.ID).LastJob, StateRunning)
	// The runner dies with the loop's run going; it comes back hours later.
	stored := newMemStore()
	for _, j := range e.jobs.jobs {
		stored.jobs[j.ID] = j
	}
	snap := &memSchedules{m: map[string]Schedule{}}
	for k, v := range e.scheds.m {
		snap.m[k] = v
	}
	close(e.p.release)
	clock := &fakeClock{}
	clock.set("2026-10-25 15:20")
	e2 := newSchedEnv(t, newFakeProvider(), stored, snap, clock)
	c := e2.sched(cronS.ID)
	if c.State != ScheduleActive || !c.NextRun.Equal(time.Date(2026, 10, 25, 16, 0, 0, 0, time.UTC)) {
		t.Errorf("cron after restart = %+v (want the next hour, no catch-up)", c)
	}
	e2.r.fireDue()
	if len(e2.r.List()) != 1 { // only the interrupted loop run, marked failed
		t.Errorf("jobs right after restart: %v", e2.r.List())
	}
	if l := e2.sched(loopS.ID); l.State != ScheduleFinished || !strings.Contains(l.Message, "ended failed") {
		t.Errorf("interrupted loop = %+v", l)
	}
}

func TestStatusCountsSchedules(t *testing.T) {
	e := defaultSchedEnv(t)
	_, _ = e.r.AddSchedule(scheduleDoc("{schedule: '0 2 * * *'}"))
	if st := e.r.Status(); st.Schedules != 1 {
		t.Errorf("status = %+v", st)
	}
	_ = sandbox.OutcomeExited
}

// A schedule is checked against the runner's current configuration for
// every run and on load: one that no longer fits is paused, not run.
func TestSchedulesAreRecheckedAgainstTheCurrentConfig(t *testing.T) {
	e := defaultSchedEnv(t)
	s, err := e.r.AddSchedule(scheduleDoc("{schedule: '0 * * * *'}"))
	if err != nil {
		t.Fatal(err)
	}
	e.r.cfg.Images = []string{"other"} // the image was removed
	e.at("2026-10-25 11:00")
	got := e.sched(s.ID)
	if got.State != SchedulePaused || !strings.Contains(got.Message, "can no longer run it") || len(e.r.List()) != 0 {
		t.Fatalf("after the image was removed: %+v (%d jobs)", got, len(e.r.List()))
	}

	// On load, too: a runner restarted without networking pauses the
	// schedules that need it.
	clock := &fakeClock{}
	clock.set("2026-10-25 10:30")
	scheds := &memSchedules{m: map[string]Schedule{}}
	netDoc := scheduleDoc("{schedule: '0 * * * *'}\n  permissions: {network: {egress: [{host: example.com}]}}")
	scheds.m["sch-1"] = Schedule{ID: "sch-1", Harness: "hello", Cron: "0 * * * *", State: ScheduleActive, Document: netDoc}
	e2 := newSchedEnv(t, newFakeProvider(), newMemStore(), scheds, clock)
	if got := e2.sched("sch-1"); got.State != SchedulePaused || !strings.Contains(got.Message, "network access is not configured") {
		t.Errorf("loaded schedule = %+v", got)
	}
}

// A cron run that ended before the scheduler recorded its start (a run
// that fails at once can) must not leave the schedule without a next run.
// It used to: the schedule then silently never ran again.
func TestQuickRunKeepsCronSchedulePlanned(t *testing.T) {
	e := defaultSchedEnv(t)
	s, err := e.r.AddSchedule(scheduleDoc("{schedule: '0 * * * *'}"))
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse("2006-01-02 15:04", "2026-10-25 11:00")
	e.r.mu.Lock()
	sc := e.r.schedules[s.ID]
	sc.NextRun = time.Time{} // as fire leaves it while the run starts
	e.r.jobs["job-quick"] = &job{Job: Job{ID: "job-quick", State: StateFailed, Schedule: s.ID}}
	e.r.recordRunLocked(sc, "job-quick", nil, now)
	e.r.mu.Unlock()
	if got := e.sched(s.ID); got.State != ScheduleActive || got.NextRun.IsZero() || got.LastJob != "job-quick" {
		t.Errorf("schedule = %+v", got)
	}
}
