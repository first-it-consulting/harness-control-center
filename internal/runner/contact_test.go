package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func contactEnv(t *testing.T, timeout time.Duration, last time.Time, save func(time.Time) error) *schedEnv {
	t.Helper()
	clock := &fakeClock{}
	clock.set("2026-10-25 10:30")
	r, err := New(Config{
		Provider: newFakeProvider(), Images: []string{"base"}, Now: clock.Now,
		ContactTimeout: timeout, LastContact: last, SaveContact: save,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Shutdown(context.Background()) })
	return &schedEnv{t: t, r: r, p: newFakeProvider(), clock: clock}
}

func TestLostContactPausesSchedules(t *testing.T) {
	e := contactEnv(t, 24*time.Hour, time.Time{}, nil)
	s, err := e.r.AddSchedule(scheduleDoc("{schedule: '0 * * * *'}"))
	if err != nil {
		t.Fatal(err)
	}
	e.at("2026-10-25 11:00") // contact 10:30 is recent
	if n := len(e.r.List()); n != 1 {
		t.Fatalf("jobs = %d, want 1", n)
	}
	e.waitAll()
	e.at("2026-10-26 10:00") // 23h30m without contact: still runs
	if n := len(e.r.List()); n != 2 {
		t.Fatalf("jobs = %d, want 2", n)
	}
	e.waitAll()
	e.at("2026-10-26 11:00") // 24h30m: paused, and this run does not start
	if s = e.sched(s.ID); s.State != SchedulePaused || !strings.Contains(s.Message, "nobody contacted") {
		t.Fatalf("schedule = %+v", s)
	}
	if n := len(e.r.List()); n != 2 {
		t.Errorf("a run started after contact lapsed (%d jobs)", n)
	}
	// Contact again does not unpause; resuming does.
	e.r.Contact()
	e.at("2026-10-26 12:00")
	if s = e.sched(s.ID); s.State != SchedulePaused {
		t.Errorf("contact unpaused the schedule: %+v", s)
	}
	if err := e.r.ResumeSchedule(s.ID); err != nil {
		t.Fatal(err)
	}
	e.waitAll()
	e.at("2026-10-26 13:00")
	if n := len(e.r.List()); n != 3 {
		t.Errorf("jobs after resume = %d, want 3", n)
	}
	// It lapses again later.
	e.at("2026-10-27 13:00")
	if s = e.sched(s.ID); s.State != SchedulePaused {
		t.Errorf("second lapse: %+v", s)
	}
}

func TestContactSurvivesRestart(t *testing.T) {
	// A runner that restarts after a long silence must not count its own
	// start as contact.
	last, _ := time.Parse("2006-01-02 15:04", "2026-10-22 10:00")
	e := contactEnv(t, 24*time.Hour, last, nil)
	if _, err := e.r.AddSchedule(scheduleDoc("{schedule: '0 * * * *'}")); err != nil {
		t.Fatal(err)
	}
	// AddSchedule came in through code, not the socket: no contact yet.
	e.at("2026-10-25 11:00")
	if n := len(e.r.List()); n != 0 {
		t.Errorf("a schedule ran although contact lapsed days ago (%d jobs)", n)
	}
	if st := e.r.Status(); !st.LastContact.Equal(last) || st.ContactTimeout != "24h0m0s" {
		t.Errorf("status = %v %q", st.LastContact, st.ContactTimeout)
	}
}

func TestContactIsSavedThrottled(t *testing.T) {
	var mu sync.Mutex
	var saved []time.Time
	e := contactEnv(t, time.Hour, time.Time{}, func(at time.Time) error {
		mu.Lock()
		saved = append(saved, at)
		mu.Unlock()
		return nil
	})
	e.r.Contact() // nothing stored yet: saved at once
	e.r.Contact() // same minute: not again
	e.clock.set("2026-10-25 10:31")
	e.r.Contact()
	e.r.Contact()
	mu.Lock()
	defer mu.Unlock()
	if len(saved) != 2 || saved[0].Format("15:04") != "10:30" || saved[1].Format("15:04") != "10:31" {
		t.Errorf("saved = %v", saved)
	}
}

func TestNoContactTimeoutNeverPauses(t *testing.T) {
	e := contactEnv(t, 0, time.Time{}, nil)
	s, _ := e.r.AddSchedule(scheduleDoc("{schedule: '0 * * * *'}"))
	e.at("2027-10-25 11:00")
	if s = e.sched(s.ID); s.State != ScheduleActive {
		t.Errorf("schedule = %+v", s)
	}
}

// waitAll waits until every job has ended.
func (e *schedEnv) waitAll() {
	e.t.Helper()
	for _, j := range e.r.List() {
		e.waitJobEnd(j.ID)
	}
}
