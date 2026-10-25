package runner

import (
	"fmt"
	"slices"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/budget"
	"github.com/first-it-consulting/harness-control-center/internal/results"
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
	// StateOverBudget: stopped because its model usage reached the
	// harness's budget.
	StateOverBudget State = "over_budget"
)

var transitions = map[State][]State{
	StateQueued:       {StateProvisioning, StateCancelled},
	StateProvisioning: {StateRunning, StateFailed, StateCancelled},
	StateRunning:      {StateSucceeded, StateFailed, StateTimedOut, StateCancelled, StateOverBudget},
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
	// Usage is the job's metered model usage (jobs with network access).
	Usage *budget.Usage `json:"usage,omitempty"`
	// Results is what the job handed back, once collected.
	Results *Results `json:"results,omitempty"`
}

// DefaultMaxDiskMiB is the default for Config.MaxDiskMiB.
const DefaultMaxDiskMiB = 8192

// Results describes what a job handed back: the files of its disk
// archive that passed sanitizing.
type Results struct {
	results.Summary
	// Error says why the results are missing or incomplete.
	Error string `json:"error,omitempty"`
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
