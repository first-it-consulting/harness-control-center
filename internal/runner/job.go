package runner

import (
	"fmt"
	"slices"
	"time"
)

// State is a job's lifecycle state.
type State string

// Job states. A job only ever moves forward, along the transitions below.
const (
	StateQueued       State = "queued"
	StateProvisioning State = "provisioning"
	StateRunning      State = "running"
	StateSucceeded    State = "succeeded"
	StateFailed       State = "failed"
	StateTimedOut     State = "timed_out"
	StateCancelled    State = "cancelled"
)

var transitions = map[State][]State{
	StateQueued:       {StateProvisioning, StateCancelled},
	StateProvisioning: {StateRunning, StateFailed, StateCancelled},
	StateRunning:      {StateSucceeded, StateFailed, StateTimedOut, StateCancelled},
}

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool { return len(transitions[s]) == 0 }

func canTransition(from, to State) bool {
	return slices.Contains(transitions[from], to)
}

// Job is a snapshot of one harness run.
type Job struct {
	ID       string    `json:"id"`
	Harness  string    `json:"harness"`
	State    State     `json:"state"`
	Error    string    `json:"error,omitempty"`
	Created  time.Time `json:"created"`
	Started  time.Time `json:"started,omitzero"`  // zero until running
	Finished time.Time `json:"finished,omitzero"` // zero until terminal
}

// Event reports a state change. Events are the runner's observability
// stream: every transition of every job produces exactly one.
type Event struct {
	JobID   string    `json:"jobId"`
	Harness string    `json:"harness"`
	From    State     `json:"from,omitempty"`
	To      State     `json:"to"`
	At      time.Time `json:"at"`
	Message string    `json:"message,omitempty"`
}

func (e Event) String() string {
	s := fmt.Sprintf("%s %s %s → %s", e.At.Format(time.RFC3339), e.JobID, e.From, e.To)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}
