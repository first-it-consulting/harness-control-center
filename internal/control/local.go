package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/local"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/version"
)

// localRunner is the runner on this Mac (ADR-0016), which the Control
// Center can set up itself: the "Run agents on this Mac" button.
type localRunner struct {
	// up runs the setup (local.Up), status reads the state, stop
	// removes the service, restart restarts it; replaceable for tests.
	up      func(ctx context.Context, restart bool, progress func(local.Step)) error
	status  func(ctx context.Context) local.Status
	stop    func(ctx context.Context) error
	restart func(ctx context.Context) error
	// config is the local runner's runner.yaml: the only configuration
	// the Control Center writes itself (its settings, ADR-0018).
	config string
	// socket is the local runner's socket, and version this Control
	// Center's: the runner it starts runs the same hcc, so a runner on
	// that socket with another version is out of date.
	socket, version string

	mu       sync.Mutex
	started  time.Time
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
		l.up = func(context.Context, bool, func(local.Step)) error { return err }
		l.stop = func(context.Context) error { return err }
		l.restart = l.stop
		return l
	}
	l.status = func(ctx context.Context) local.Status { return local.GetStatus(ctx, layout, svc) }
	l.up = func(ctx context.Context, restart bool, progress func(local.Step)) error {
		return local.Up(ctx, local.Options{Layout: layout, Version: version.Version, Service: svc, Progress: progress, Restart: restart})
	}
	l.stop = svc.Stop
	l.restart = svc.Restart
	l.config = layout.Config()
	l.socket, l.version = layout.Socket(), version.Version
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
//
// If the runner answering there runs another version than this Control
// Center (an older bin/hcc, since replaced), the setup also restarts it:
// that is how the Mac's runner is updated. A restart would end its jobs,
// so it is refused while jobs run or wait.
func (s *Server) localSetup(w http.ResponseWriter, r *http.Request) {
	l := s.local
	restart, busy, err := s.localOutdated(r.Context())
	switch {
	case err != nil:
		writeError(w, http.StatusBadGateway, err)
		return
	case restart && busy > 0:
		writeError(w, http.StatusConflict, fmt.Errorf("the runner on this Mac has %d jobs running or queued: updating restarts it and would end them. Wait until they finish, or stop them first", busy))
		return
	}
	l.mu.Lock()
	if l.running {
		l.mu.Unlock()
		writeError(w, http.StatusConflict, errors.New("the setup is running already"))
		return
	}
	l.running, l.finished, l.steps, l.err, l.started = true, false, nil, "", time.Now()
	l.mu.Unlock()
	// The setup outlives the request: not cancelled with it.
	ctx := context.WithoutCancel(r.Context())
	go func() {
		err := l.up(ctx, restart, l.progress)
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

// localOutdated reports whether the runner answering on the local
// runner's socket runs another version than this Control Center, and how
// many jobs it has. A runner that does not answer is not outdated (the
// setup starts it with this version anyway).
func (s *Server) localOutdated(ctx context.Context) (outdated bool, jobs int, err error) {
	l := s.local
	if l.socket == "" || !versioned(l.version) {
		return false, 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, runnerTimeout)
	defer cancel()
	st, err := runnerd.NewUnixClient(l.socket).Info(ctx)
	if err != nil {
		return false, 0, nil //nolint:nilerr // not answering: nothing to restart
	}
	return st.Version != l.version, st.Running + st.Queued, nil
}

// versioned reports whether v is a release or git-described build, not a
// plain "go build" (whose version says nothing).
func versioned(v string) bool { return v != "" && v != "dev" }

// localProgress is the setup's progress for the local runner's card, or
// nil when no setup runs.
func (l *localRunner) localProgress() *installProgress {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return nil
	}
	p := &installProgress{Started: l.started}
	if n := len(l.steps); n > 0 {
		p.Step = l.steps[n-1].Title
	}
	return p
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
