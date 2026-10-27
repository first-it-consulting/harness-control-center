package control

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"

	"github.com/first-it-consulting/harness-control-center/internal/local"
	"github.com/first-it-consulting/harness-control-center/internal/version"
)

// localRunner is the runner on this Mac (ADR-0016), which the Control
// Center can set up itself: the "Run agents on this Mac" button.
type localRunner struct {
	// up runs the setup (local.Up), status reads the state, stop
	// removes the service; replaceable for tests.
	up     func(ctx context.Context, progress func(local.Step)) error
	status func(ctx context.Context) local.Status
	stop   func(ctx context.Context) error

	mu       sync.Mutex
	running  bool
	finished bool
	steps    []local.Step
	err      string
}

func newLocalRunner() *localRunner {
	l := &localRunner{}
	layout, lerr := local.DefaultLayout()
	svc, serr := local.DefaultService()
	if err := errors.Join(lerr, serr); err != nil {
		l.status = func(context.Context) local.Status { return local.Status{Supported: err.Error()} }
		l.up = func(context.Context, func(local.Step)) error { return err }
		l.stop = func(context.Context) error { return err }
		return l
	}
	l.status = func(ctx context.Context) local.Status { return local.GetStatus(ctx, layout, svc) }
	l.up = func(ctx context.Context, progress func(local.Step)) error {
		return local.Up(ctx, local.Options{Layout: layout, Version: version.Version, Service: svc, Progress: progress})
	}
	l.stop = svc.Stop
	return l
}

// progress records a step's latest state, in the order steps began.
func (l *localRunner) progress(s local.Step) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if i := slices.IndexFunc(l.steps, func(o local.Step) bool { return o.ID == s.ID }); i >= 0 {
		l.steps[i] = s
	} else {
		l.steps = append(l.steps, s)
	}
}

type localSetupState struct {
	Running  bool         `json:"running"`
	Finished bool         `json:"finished"`
	Steps    []local.Step `json:"steps"`
	Error    string       `json:"error,omitempty"`
}

type localView struct {
	local.Status
	Ready bool `json:"ready"`
	// Registered: the Control Center lists a runner on its socket.
	Registered bool            `json:"registered"`
	Setup      localSetupState `json:"setup"`
}

func (s *Server) localState(w http.ResponseWriter, r *http.Request) {
	st := s.local.status(r.Context())
	v := localView{Status: st, Ready: st.Ready()}
	s.mu.Lock()
	v.Registered = slices.ContainsFunc(s.cfg.Runners, func(r RunnerRef) bool { return r.Socket == st.Socket })
	s.mu.Unlock()
	l := s.local
	l.mu.Lock()
	v.Setup = localSetupState{Running: l.running, Finished: l.finished, Steps: append([]local.Step{}, l.steps...), Error: l.err}
	l.mu.Unlock()
	writeJSON(w, http.StatusOK, v)
}

// localSetup starts the setup in the background; GET /api/local follows
// it. When it succeeds, the runner is added to this Control Center.
func (s *Server) localSetup(w http.ResponseWriter, _ *http.Request) {
	l := s.local
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		writeError(w, http.StatusConflict, errors.New("the setup is running already"))
		return
	}
	l.running, l.finished, l.steps, l.err = true, false, nil, ""
	l.mu.Unlock()
	go func() {
		// Not the request's context: the setup outlives the request.
		ctx := context.Background()
		err := l.up(ctx, l.progress)
		if err == nil {
			err = s.registerLocal(l.status(ctx).Socket)
		}
		l.mu.Lock()
		l.running, l.finished = false, true
		if err != nil {
			l.err = err.Error()
		}
		l.mu.Unlock()
		if err != nil {
			s.log.Warn("local runner setup failed", "err", err)
		} else {
			s.log.Info("local runner set up")
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *Server) registerLocal(socket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, added, err := s.cfg.WithLocalRunner(socket)
	if err != nil || !added {
		return err
	}
	if err := SaveConfig(s.cfgPath, next); err != nil {
		return err
	}
	s.cfg = next
	return nil
}

// localStop stops the local runner's service (its files stay; the setup
// starts it again).
func (s *Server) localStop(w http.ResponseWriter, r *http.Request) {
	if err := s.local.stop(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"stopped": true})
}
