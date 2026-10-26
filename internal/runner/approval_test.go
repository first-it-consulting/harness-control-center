package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

func newApprovalRunner(t *testing.T, timeout time.Duration) (*Runner, chan Approver) {
	t.Helper()
	got := make(chan Approver, 4)
	p := newFakeProvider()
	p.block = true
	r, err := New(Config{
		Provider: p, Images: []string{"base"}, ApprovalTimeout: timeout,
		Egress: func(ej EgressJob) (sandbox.Proxy, error) {
			got <- ej.Approve
			return nopProxy{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(p.release); _ = r.Shutdown(context.Background()) })
	return r, got
}

const gatedHarness = "  approvals: {requireFor: [git.push, http.delete]}\n  permissions: {network: {egress: [{host: example.com}]}}\n"

func waitPending(t *testing.T, r *Runner, n int) []Approval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := r.Approvals(); len(a) == n {
			return a
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("pending approvals: %v, want %d", r.Approvals(), n)
	return nil
}

func TestApprovalApproveAndDeny(t *testing.T) {
	r, got := newApprovalRunner(t, time.Minute)
	id, err := r.Submit(harness(t, gatedHarness))
	if err != nil {
		t.Fatal(err)
	}
	approve := <-got
	waitState(t, r, id, StateRunning)

	result := make(chan error, 1)
	go func() {
		result <- approve(context.Background(), ApprovalRequest{Actions: []string{"http.post", "git.push"}, Method: "POST", Host: "example.com", Path: "/r.git/git-receive-pack", Details: []string{"refs/heads/main: a → b"}})
	}()
	pending := waitPending(t, r, 1)
	j := waitState(t, r, id, StateAwaitingApproval)
	if pending[0].JobID != id || pending[0].Host != "example.com" || !strings.HasPrefix(pending[0].ID, "apr-") {
		t.Errorf("pending = %+v", pending[0])
	}
	_ = j
	if err := r.Decide(pending[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Errorf("approved request: %v", err)
	}
	waitState(t, r, id, StateRunning)
	if err := r.Decide(pending[0].ID, true); !errors.Is(err, ErrNoApproval) {
		t.Errorf("decided twice: %v", err)
	}

	go func() {
		result <- approve(context.Background(), ApprovalRequest{Actions: []string{"http.delete"}, Method: "DELETE", Host: "example.com", Path: "/x"})
	}()
	pending = waitPending(t, r, 1)
	if err := r.Decide(pending[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("denied request: %v", err)
	}
}

func TestApprovalTimesOutAndEndsWithTheJob(t *testing.T) {
	r, got := newApprovalRunner(t, 50*time.Millisecond)
	id, err := r.Submit(harness(t, gatedHarness))
	if err != nil {
		t.Fatal(err)
	}
	approve := <-got
	waitState(t, r, id, StateRunning)
	if err := approve(context.Background(), ApprovalRequest{Method: "DELETE"}); err == nil || !strings.Contains(err.Error(), "nobody decided") {
		t.Errorf("timeout: %v", err)
	}
	if len(r.Approvals()) != 0 {
		t.Error("expired approval still pending")
	}

	r2, got2 := newApprovalRunner(t, time.Hour)
	id2, _ := r2.Submit(harness(t, gatedHarness))
	approve2 := <-got2
	waitState(t, r2, id2, StateRunning)
	result := make(chan error, 1)
	go func() { result <- approve2(context.Background(), ApprovalRequest{Method: "DELETE"}) }()
	waitPending(t, r2, 1)
	r2.KillAll()
	if err := <-result; err == nil {
		t.Error("the kill switch left a request approvable")
	}
	waitState(t, r2, id2, StateCancelled)
	if len(r2.Approvals()) != 0 {
		t.Error("approval of a killed job still pending")
	}
}

func TestApprovalsPerJobAreBounded(t *testing.T) {
	r, got := newApprovalRunner(t, time.Hour)
	id, _ := r.Submit(harness(t, gatedHarness))
	approve := <-got
	waitState(t, r, id, StateRunning)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for range maxPendingPerJob {
		go func() { _ = approve(ctx, ApprovalRequest{Method: "DELETE"}) }()
	}
	waitPending(t, r, maxPendingPerJob)
	if err := approve(ctx, ApprovalRequest{Method: "DELETE"}); err == nil || !strings.Contains(err.Error(), "already waiting") {
		t.Errorf("over the bound: %v", err)
	}
	cancel()
	waitPending(t, r, 0)
	waitState(t, r, id, StateRunning)
}

func TestUnenforceableApprovalsAreRejected(t *testing.T) {
	r, _ := newApprovalRunner(t, time.Minute)
	_, err := r.Submit(harness(t, "  approvals: {requireFor: [git.push.default-branch]}\n"))
	if err == nil || !strings.Contains(err.Error(), "cannot be enforced") {
		t.Errorf("Submit = %v", err)
	}
}
