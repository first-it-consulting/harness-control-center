package runner

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Approvals: a harness's approval gates hold matching requests at its
// egress proxy until a human decides (ADR-0009). While any is pending
// the job is awaiting_approval; its time limit keeps running. A request
// nobody decides on within ApprovalTimeout is refused, as is one whose
// job ends.

// Defaults and bounds for approvals.
const (
	DefaultApprovalTimeout = 30 * time.Minute
	maxPendingPerJob       = 8
)

// ErrNoApproval is returned for an approval that is not pending.
var ErrNoApproval = errors.New("runner: no such pending approval")

// ApprovalRequest is what a gate asks about. Path and Details come from
// the agent.
type ApprovalRequest struct {
	Actions []string `json:"actions"`
	Method  string   `json:"method"`
	Host    string   `json:"host"`
	Path    string   `json:"path"`
	Details []string `json:"details,omitempty"`
}

// Approver asks for a decision; nil means approved. It blocks.
type Approver func(ctx context.Context, req ApprovalRequest) error

// Approval is a pending decision.
type Approval struct {
	ID      string `json:"id"`
	JobID   string `json:"jobId"`
	Harness string `json:"harness"`
	ApprovalRequest
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

type pendingApproval struct {
	Approval
	decided chan error
	done    bool
}

func (a ApprovalRequest) describe() string {
	return fmt.Sprintf("%s %s%s (%s)", a.Method, a.Host, a.Path, strings.Join(a.Actions, ", "))
}

// awaitApproval holds one gated request of j until it is decided.
func (r *Runner) awaitApproval(ctx context.Context, j *job, req ApprovalRequest) error {
	r.mu.Lock()
	n := 0
	for _, a := range r.approvals {
		if a.JobID == j.ID {
			n++
		}
	}
	if n >= maxPendingPerJob {
		r.mu.Unlock()
		return fmt.Errorf("%d requests of this job are already waiting for approval", n)
	}
	now := r.cfg.Now()
	a := &pendingApproval{decided: make(chan error, 1), Approval: Approval{
		ID: "apr-" + strings.TrimPrefix(r.cfg.NewID(), "job-"), JobID: j.ID, Harness: j.Harness,
		ApprovalRequest: req, Created: now, Expires: now.Add(r.cfg.ApprovalTimeout),
	}}
	r.approvals[a.ID] = a
	if j.State == StateRunning {
		r.transitionLocked(j, StateAwaitingApproval, "waiting for approval: "+req.describe())
	}
	r.mu.Unlock()

	timer := time.NewTimer(r.cfg.ApprovalTimeout)
	defer timer.Stop()
	var result error
	select {
	case result = <-a.decided:
	case <-ctx.Done():
		result = errors.New("the request was abandoned")
	case <-j.ctx.Done():
		result = errors.New("the job ended")
	case <-timer.C:
		result = fmt.Errorf("nobody decided within %s", r.cfg.ApprovalTimeout)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	a.done = true
	delete(r.approvals, a.ID)
	still := slices.ContainsFunc(r.pendingLocked(), func(p Approval) bool { return p.JobID == j.ID })
	if !still && j.State == StateAwaitingApproval {
		msg := "approved: " + req.describe()
		if result != nil {
			msg = "not approved (" + result.Error() + "): " + req.describe()
		}
		r.transitionLocked(j, StateRunning, msg)
	}
	return result
}

// Decide approves or denies a pending request.
func (r *Runner) Decide(id string, approve bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.approvals[id]
	if !ok || a.done {
		return ErrNoApproval
	}
	a.done = true
	if approve {
		a.decided <- nil
	} else {
		a.decided <- errors.New("denied")
	}
	return nil
}

// Approvals returns the pending approvals, oldest first.
func (r *Runner) Approvals() []Approval {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pendingLocked()
}

func (r *Runner) pendingLocked() []Approval {
	out := make([]Approval, 0, len(r.approvals))
	for _, a := range r.approvals {
		if !a.done {
			out = append(out, a.Approval)
		}
	}
	slices.SortFunc(out, func(a, b Approval) int { return a.Created.Compare(b.Created) })
	return out
}
