package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Subagents (ADR-0011): a running agent asks the runner, through its
// egress proxy, for helper agents. Each runs as an ordinary job in its
// own sandbox, with at most its parent's permissions
// (policy.Engine.CheckChild), the parent's model and approval gates, a
// sandbox no larger than the parent's and no more time than the parent
// has left. The whole tree shares the parent's budget meter. A helper
// ends when its parent ends. Every job's spec.permissions.subagents.max
// bounds the helpers started in its subtree in total.

// ErrNotSubagent is returned for a job that is not the caller's
// subagent.
var ErrNotSubagent = errors.New("runner: no such subagent of this job")

// alive reports whether a job's agent is running (and may start helpers).
func (j *job) alive() bool {
	return j.State == StateRunning || j.State == StateAwaitingApproval
}

// reserveSubagentLocked counts a new subagent of p against the limit of
// p and of every ancestor, or refuses it.
func (r *Runner) reserveSubagentLocked(p *job) error {
	if !p.alive() || p.harness == nil {
		return errors.New("runner: only a running agent can start subagents")
	}
	for n := p; n != nil; n = n.parent {
		if n.harness == nil {
			return errors.New("runner: a parent of this job is gone")
		}
		eng, err := policy.New(n.harness)
		if err != nil {
			return fmt.Errorf("runner: %w", err)
		}
		if d := eng.CheckSpawnSubagent(n.spawned); !d.Allowed {
			if n == p {
				return fmt.Errorf("runner: %s", d.Reason)
			}
			return fmt.Errorf("runner: %s for the agent that started this one (%s)", d.Reason, n.ID)
		}
	}
	for n := p; n != nil; n = n.parent {
		n.spawned++
	}
	return nil
}

func (r *Runner) releaseSubagentLocked(p *job) {
	for n := p; n != nil; n = n.parent {
		n.spawned--
	}
}

// Spawn starts a subagent of the running job parentID, as req asks.
func (r *Runner) Spawn(parentID string, req *spec.Subagent) (Job, error) {
	r.mu.Lock()
	p, ok := r.jobs[parentID]
	if !ok {
		r.mu.Unlock()
		return Job{}, ErrNotFound
	}
	if !p.alive() || p.harness == nil {
		r.mu.Unlock()
		return Job{}, errors.New("runner: only a running agent can start subagents")
	}
	remaining := p.Started.Add(p.spec.Timeout).Sub(r.cfg.Now())
	ph := p.harness
	r.mu.Unlock()

	child, err := spec.Child(ph, req, remaining)
	if err != nil {
		return Job{}, fmt.Errorf("runner: %w", err)
	}
	eng, err := policy.New(ph)
	if err != nil {
		return Job{}, fmt.Errorf("runner: %w", err)
	}
	if d := eng.CheckChild(child.Spec.Permissions); !d.Allowed {
		return Job{}, fmt.Errorf("runner: %s", d.Reason)
	}
	if err := r.check(child); err != nil {
		return Job{}, err
	}
	// enqueue re-checks that the parent still runs and reserves the
	// subagent against every limit, atomically with queueing it.
	id, err := r.enqueue(child, enqueueOpts{parent: p})
	if err != nil {
		return Job{}, err
	}
	j, _ := r.Get(id)
	return j, nil
}

// Subagents lists the subagents job parentID started, oldest first.
func (r *Runner) Subagents(parentID string) ([]Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.jobs[parentID]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]Job, 0, len(p.children))
	for _, c := range p.children {
		out = append(out, c.snapshot())
	}
	return out, nil
}

// subagentLocked returns id if it is one of p's own subagents.
func subagentLocked(p *job, id string) (*job, bool) {
	i := slices.IndexFunc(p.children, func(c *job) bool { return c.ID == id })
	if i < 0 {
		return nil, false
	}
	return p.children[i], true
}

// SubagentView is what a parent agent sees of a subagent.
type SubagentView struct {
	Job
	// Output is the end of the subagent's own output (at most 64 KiB).
	Output string `json:"output,omitempty"`
}

// agentAPI is the subagent API a job's egress proxy serves to its guest.
// Everything a request names is resolved relative to the job: an agent
// only ever sees and controls its own subagents.
//
//	GET  /hcc/v1/agents               this agent's subagents
//	POST /hcc/v1/agents               start one (a spec.Subagent, JSON)
//	GET  /hcc/v1/agents/{id}          one, with the end of its output
//	POST /hcc/v1/agents/{id}/stop     stop it
//	GET  /hcc/v1/agents/{id}/results  its results (a sanitized tar archive)
type agentAPI struct {
	r      *Runner
	parent *job // set under r.mu before the guest exists
}

func (a *agentAPI) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if a.parentJob() == nil {
		apiError(w, http.StatusServiceUnavailable, errors.New("the job is not ready"))
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /hcc/v1/agents", a.list)
	mux.HandleFunc("POST /hcc/v1/agents", a.spawn)
	mux.HandleFunc("GET /hcc/v1/agents/{id}", a.get)
	mux.HandleFunc("POST /hcc/v1/agents/{id}/stop", a.stop)
	mux.HandleFunc("GET /hcc/v1/agents/{id}/results", a.results)
	mux.ServeHTTP(w, req)
}

func (a *agentAPI) parentJob() *job {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	return a.parent
}

func (a *agentAPI) list(w http.ResponseWriter, _ *http.Request) {
	jobs, err := a.r.Subagents(a.parentJob().ID)
	if err != nil {
		apiError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (a *agentAPI) spawn(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, spec.MaxSubagentBytes))
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("request too large (at most %d bytes)", spec.MaxSubagentBytes))
		return
	}
	sub, err := spec.ParseSubagent(body)
	if err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	j, err := a.r.Spawn(a.parentJob().ID, sub)
	if err != nil {
		apiError(w, http.StatusForbidden, err)
		return
	}
	a.r.cfg.Log.Info("subagent started", "job", j.ID, "parent", j.Parent, "harness", j.Harness)
	writeJSON(w, http.StatusCreated, j)
}

func (a *agentAPI) child(w http.ResponseWriter, req *http.Request) (*job, bool) {
	a.r.mu.Lock()
	defer a.r.mu.Unlock()
	c, ok := subagentLocked(a.parent, req.PathValue("id"))
	if !ok {
		apiError(w, http.StatusNotFound, ErrNotSubagent)
	}
	return c, ok
}

func (a *agentAPI) get(w http.ResponseWriter, req *http.Request) {
	c, ok := a.child(w, req)
	if !ok {
		return
	}
	a.r.mu.Lock()
	v := SubagentView{Job: c.snapshot()}
	watch := c.watch
	a.r.mu.Unlock()
	if watch != nil {
		v.Output = watch.agentOutput()
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *agentAPI) stop(w http.ResponseWriter, req *http.Request) {
	c, ok := a.child(w, req)
	if !ok {
		return
	}
	if err := a.r.Cancel(c.ID); err != nil {
		apiError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"stopped": c.ID})
}

func (a *agentAPI) results(w http.ResponseWriter, req *http.Request) {
	c, ok := a.child(w, req)
	if !ok {
		return
	}
	a.r.mu.Lock()
	done, res := c.State.Terminal(), c.Results
	a.r.mu.Unlock()
	switch {
	case !done:
		apiError(w, http.StatusConflict, errors.New("the subagent has not finished"))
		return
	case res == nil || res.Files == 0 || a.r.cfg.ReadResults == nil:
		apiError(w, http.StatusNotFound, errors.New("the subagent has no results"))
		return
	}
	f, err := a.r.cfg.ReadResults(c.ID)
	if err != nil {
		apiError(w, http.StatusNotFound, errors.New("the subagent's results are gone"))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f) // the guest may hang up
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

func apiError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
