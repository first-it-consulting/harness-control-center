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
	// StateAwaitingApproval: running, with a request held by an approval
	// gate until a human decides.
	StateAwaitingApproval State = "awaiting_approval"
)

var transitions = map[State][]State{
	StateQueued:           {StateProvisioning, StateCancelled},
	StateProvisioning:     {StateRunning, StateFailed, StateCancelled},
	StateRunning:          {StateSucceeded, StateFailed, StateTimedOut, StateCancelled, StateOverBudget, StateAwaitingApproval},
	StateAwaitingApproval: {StateRunning, StateSucceeded, StateFailed, StateTimedOut, StateCancelled, StateOverBudget},
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
	// Schedule is the schedule that started this job, if any.
	Schedule string `json:"schedule,omitempty"`
	// LoopDone: the run reported that its loop's task is complete.
	LoopDone bool `json:"loopDone,omitempty"`
	// Parent is the job whose agent started this one (a subagent). Its
	// Usage is the whole tree's: they share one budget.
	Parent string `json:"parent,omitempty"`
}

// Status describes a runner.
type Status struct {
	Target        string   `json:"target"`
	Images        []string `json:"images"`
	MaxConcurrent int      `json:"maxConcurrent"`
	MaxDiskMiB    int      `json:"maxDiskMiB"`
	Running       int      `json:"running"`
	Queued        int      `json:"queued"`
	// Killed: the kill switch is engaged and new jobs are refused.
	Killed    bool   `json:"killed"`
	Schedules int    `json:"schedules"`
	Version   string `json:"version,omitempty"`
	// Tools are the tools each image offers ("name@version"), for images
	// with a tool catalog.
	Tools map[string][]string `json:"tools,omitempty"`
	// LastContact is when a person last reached the runner; after
	// ContactTimeout ("" if never) without, its schedules are paused.
	LastContact    time.Time `json:"lastContact"`
	ContactTimeout string    `json:"contactTimeout,omitempty"`
}

// Store persists jobs. Save is called with the runner's lock held, so it
// must not call back into the runner.
type Store interface {
	Save(Job) error
	Load() ([]Job, error)
	// Delete removes a job and everything stored with it.
	Delete(id string) error
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
