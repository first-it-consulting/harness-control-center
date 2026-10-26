package control

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/safetext"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

//go:embed web
var webFS embed.FS

// API routes (JSON unless noted; all need a session, see auth):
//
//	GET    /api/session                          whether the session is valid
//	POST   /api/logout
//	GET    /api/runners                          configured runners and their status
//	POST   /api/runners                          add a runner
//	DELETE /api/runners/{runner}                 remove a runner (from this list only)
//	POST   /api/runners/{runner}/kill            kill switch on one runner
//	POST   /api/runners/{runner}/resume
//	POST   /api/kill                             kill switch on every runner
//	GET    /api/jobs                             jobs of every runner
//	GET    /api/runners/{runner}/jobs/{id}
//	POST   /api/runners/{runner}/jobs/{id}/cancel
//	GET    /api/runners/{runner}/jobs/{id}/console   console, control characters made visible
//	POST   /api/runners/{runner}/jobs/{id}/download  a ticket for GET /download/{ticket}: the results archive
//	GET    /api/schedules                        schedules of every runner
//	POST   /api/runners/{runner}/schedules/{id}/{action}   pause or resume
//	DELETE /api/runners/{runner}/schedules/{id}
//	GET    /api/approvals                        requests waiting for a decision, on every runner
//	POST   /api/runners/{runner}/approvals/{id}  decide: {"approve": true|false}
//	POST   /api/check                            validate and explain a harness
//	POST   /api/runs                             start a harness, or add it as a schedule if it has a trigger
//	GET    /api/harnesses                        the library
//	GET    /api/harnesses/{name}
//	PUT    /api/harnesses                        save (named by its metadata.name)
//	DELETE /api/harnesses/{name}

// Timeouts for calls to runners.
const (
	runnerTimeout   = 15 * time.Second
	transferTimeout = 10 * time.Minute
	maxJSONBody     = 2 << 20
)

// Server is the Control Center.
type Server struct {
	cfgPath string
	lib     *Library
	auth    *auth
	log     *slog.Logger
	handler http.Handler

	mu      sync.Mutex
	cfg     *Config
	clients map[RunnerRef]*runnerd.Client
}

// NewServer returns a Control Center using the config file at cfgPath and
// the harness library in libDir.
func NewServer(cfgPath, libDir string, log *slog.Logger) (*Server, error) {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	s := &Server{cfgPath: cfgPath, lib: NewLibrary(libDir), auth: newAuth(), log: log, cfg: cfg, clients: map[RunnerRef]*runnerd.Client{}}
	mux := http.NewServeMux()
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/runners", s.runners)
	mux.HandleFunc("POST /api/runners", s.addRunner)
	mux.HandleFunc("DELETE /api/runners/{runner}", s.removeRunner)
	mux.HandleFunc("POST /api/runners/{runner}/kill", s.killRunner)
	mux.HandleFunc("POST /api/runners/{runner}/resume", s.resumeRunner)
	mux.HandleFunc("POST /api/kill", s.killAll)
	mux.HandleFunc("GET /api/jobs", s.jobs)
	mux.HandleFunc("GET /api/runners/{runner}/jobs/{id}", s.job)
	mux.HandleFunc("POST /api/runners/{runner}/jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("GET /api/runners/{runner}/jobs/{id}/console", s.console)
	mux.HandleFunc("POST /api/runners/{runner}/jobs/{id}/download", s.download)
	mux.HandleFunc("GET /download/{ticket}", s.results)
	mux.HandleFunc("GET /api/schedules", s.schedules)
	mux.HandleFunc("POST /api/runners/{runner}/schedules/{id}/{action}", s.scheduleAction)
	mux.HandleFunc("DELETE /api/runners/{runner}/schedules/{id}", s.scheduleAction)
	mux.HandleFunc("GET /api/approvals", s.approvals)
	mux.HandleFunc("POST /api/runners/{runner}/approvals/{id}", s.decide)
	mux.HandleFunc("POST /api/check", s.check)
	mux.HandleFunc("POST /api/runs", s.run)
	mux.HandleFunc("GET /api/harnesses", s.harnesses)
	mux.HandleFunc("GET /api/harnesses/{name}", s.harness)
	mux.HandleFunc("PUT /api/harnesses", s.saveHarness)
	mux.HandleFunc("DELETE /api/harnesses/{name}", s.deleteHarness)
	s.handler = s.auth.guard(mux)
	return s, nil
}

// ListenLoopback listens on addr, which must be a loopback IP and port:
// the Control Center is never reachable from the network (use ssh -L to
// reach it from elsewhere).
func ListenLoopback(addr string) (net.Listener, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, fmt.Errorf("listen address %q must be an IP and port, e.g. 127.0.0.1:7878", addr)
	}
	if !ap.Addr().IsLoopback() {
		return nil, fmt.Errorf("listen address %s is not a loopback address; the Control Center only listens locally", addr)
	}
	return net.Listen("tcp", addr)
}

// LoginURL is the URL that opens a session; it carries the secret login
// token, so it is shown only to the user who started the server.
func (s *Server) LoginURL(l net.Listener) string {
	return "http://" + l.Addr().String() + "/login?token=" + s.auth.loginToken
}

// Serve serves on l (from ListenLoopback) until ctx ends.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	ap, err := netip.ParseAddrPort(l.Addr().String())
	if err != nil {
		return err
	}
	port := strconv.Itoa(int(ap.Port()))
	hosts := []string{l.Addr().String(), "localhost:" + port}
	s.auth.setHosts(hosts)
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		// No WriteTimeout: results downloads can be large.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// ServeHTTP implements http.Handler (for tests).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	id, ok := s.auth.login(r.URL.Query().Get("token"))
	if !ok {
		http.Error(w, "invalid login link: use the one \"hcc control serve\" printed", http.StatusForbidden)
		return
	}
	// Away from the URL with the login token in it. The session goes in
	// the fragment: the page takes it from there, and fragments are never
	// sent to a server.
	http.Redirect(w, r, "/#session="+id, http.StatusSeeOther)
}

func (s *Server) session(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true}) // the guard checked it
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.logout(r)
	writeJSON(w, http.StatusOK, map[string]string{})
}

// --- runners ---

func (s *Server) config() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *s.cfg
	c.Runners = append([]RunnerRef(nil), s.cfg.Runners...)
	return &c
}

// client returns the (cached) client for a configured runner.
func (s *Server) client(name string) (*runnerd.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.cfg.Runner(name)
	if !ok {
		return nil, errNotFound("no runner called " + name)
	}
	if c, ok := s.clients[ref]; ok {
		return c, nil
	}
	c, err := ref.client()
	if err != nil {
		return nil, err
	}
	s.clients[ref] = c
	return c, nil
}

type notFound string

func (e notFound) Error() string { return string(e) }

func errNotFound(msg string) error { return notFound(msg) }

// RunnerView is a runner as the UI shows it.
type RunnerView struct {
	RunnerRef
	Status *runner.Status `json:"status,omitempty"`
	Error  string         `json:"error,omitempty"`
}

// eachRunner calls fn for every configured runner concurrently.
func (s *Server) eachRunner(ctx context.Context, fn func(ctx context.Context, c *runnerd.Client, ref RunnerRef) error) []RunnerView {
	cfg := s.config()
	out := make([]RunnerView, len(cfg.Runners))
	var wg sync.WaitGroup
	for i, ref := range cfg.Runners {
		out[i].RunnerRef = ref
		wg.Go(func() {
			c, err := s.client(ref.Name)
			if err == nil {
				rctx, cancel := context.WithTimeout(ctx, runnerTimeout)
				err = fn(rctx, c, ref)
				cancel()
			}
			if err != nil {
				out[i].Error = err.Error()
			}
		})
	}
	wg.Wait()
	return out
}

func (s *Server) runners(w http.ResponseWriter, r *http.Request) {
	var mu sync.Mutex
	statuses := map[string]runner.Status{}
	views := s.eachRunner(r.Context(), func(ctx context.Context, c *runnerd.Client, ref RunnerRef) error {
		st, err := c.Info(ctx)
		if err == nil {
			mu.Lock()
			statuses[ref.Name] = st
			mu.Unlock()
		}
		return err
	})
	for i := range views {
		if st, ok := statuses[views[i].Name]; ok {
			views[i].Status = &st
		}
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) addRunner(w http.ResponseWriter, r *http.Request) {
	var ref RunnerRef
	if !readJSON(w, r, &ref) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.cfg.Runner(ref.Name); exists {
		writeError(w, http.StatusConflict, fmt.Errorf("a runner called %s exists already", ref.Name))
		return
	}
	next := &Config{Runners: append(append([]RunnerRef(nil), s.cfg.Runners...), ref)}
	if err := SaveConfig(s.cfgPath, next); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.cfg = next
	s.log.Info("runner added", "runner", ref.Name, "ssh", ref.SSH, "socket", ref.Socket)
	writeJSON(w, http.StatusCreated, ref)
}

func (s *Server) removeRunner(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("runner")
	s.mu.Lock()
	defer s.mu.Unlock()
	next := &Config{}
	for _, ref := range s.cfg.Runners {
		if ref.Name != name {
			next.Runners = append(next.Runners, ref)
		}
	}
	if len(next.Runners) == len(s.cfg.Runners) {
		writeError(w, http.StatusNotFound, errNotFound("no runner called "+name))
		return
	}
	if err := SaveConfig(s.cfgPath, next); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.cfg = next
	s.log.Info("runner removed", "runner", name)
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (s *Server) killRunner(w http.ResponseWriter, r *http.Request) {
	s.withRunner(w, r, func(ctx context.Context, c *runnerd.Client) (any, error) {
		s.log.Warn("kill switch", "runner", r.PathValue("runner"))
		return map[string]string{}, c.KillAll(ctx)
	})
}

func (s *Server) resumeRunner(w http.ResponseWriter, r *http.Request) {
	s.withRunner(w, r, func(ctx context.Context, c *runnerd.Client) (any, error) {
		s.log.Info("resume", "runner", r.PathValue("runner"))
		return map[string]string{}, c.Resume(ctx)
	})
}

// killAll engages every runner's kill switch at once and reports which
// could not be reached: those are still running.
func (s *Server) killAll(w http.ResponseWriter, r *http.Request) {
	s.log.Warn("kill switch on every runner")
	views := s.eachRunner(r.Context(), func(ctx context.Context, c *runnerd.Client, _ RunnerRef) error {
		return c.KillAll(ctx)
	})
	writeJSON(w, http.StatusOK, views)
}

// withRunner runs fn against the request's runner and writes its result.
func (s *Server) withRunner(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, c *runnerd.Client) (any, error)) {
	c, err := s.client(r.PathValue("runner"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runnerTimeout)
	defer cancel()
	v, err := fn(ctx, c)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// --- jobs ---

// JobView is a job and the runner it belongs to.
type JobView struct {
	Runner string `json:"runner"`
	runner.Job
}

func (s *Server) jobs(w http.ResponseWriter, r *http.Request) {
	var mu sync.Mutex
	var jobs []JobView
	views := s.eachRunner(r.Context(), func(ctx context.Context, c *runnerd.Client, ref RunnerRef) error {
		list, err := c.List(ctx)
		mu.Lock()
		for _, j := range list {
			jobs = append(jobs, JobView{Runner: ref.Name, Job: j})
		}
		mu.Unlock()
		return err
	})
	errs := map[string]string{}
	for _, v := range views {
		if v.Error != "" {
			errs[v.Name] = v.Error
		}
	}
	// Newest first.
	sortJobs(jobs)
	if jobs == nil {
		jobs = []JobView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs, "errors": errs})
}

func jobID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	return id, sandbox.ValidateID(id)
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	s.withRunner(w, r, func(ctx context.Context, c *runnerd.Client) (any, error) {
		id, err := jobID(r)
		if err != nil {
			return nil, err
		}
		j, err := c.Get(ctx, id)
		return JobView{Runner: r.PathValue("runner"), Job: j}, err
	})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	s.withRunner(w, r, func(ctx context.Context, c *runnerd.Client) (any, error) {
		id, err := jobID(r)
		if err != nil {
			return nil, err
		}
		s.log.Info("cancel", "runner", r.PathValue("runner"), "job", id)
		return map[string]string{}, c.Cancel(ctx, id)
	})
}

// console returns the console log with control characters made visible:
// it is the agent's output.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	s.withRunner(w, r, func(ctx context.Context, c *runnerd.Client) (any, error) {
		id, err := jobID(r)
		if err != nil {
			return nil, err
		}
		var b strings.Builder
		if err := c.Console(ctx, id, &b); err != nil {
			return nil, err
		}
		return map[string]string{"text": safetext.String(b.String())}, nil
	})
}

// download hands out a ticket for a job's results: a link cannot carry
// the session header.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if _, err := s.client(r.PathValue("runner")); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if _, err := jobID(r); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.auth.newTicket(r.PathValue("runner") + "/" + r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]string{"url": "/download/" + t})
}

// results streams a job's archive for a download ticket. It is the
// agent's content: served as an attachment of an opaque type, never
// rendered.
func (s *Server) results(w http.ResponseWriter, r *http.Request) {
	target, ok := s.auth.redeem(r.PathValue("ticket"))
	if !ok {
		http.Error(w, "this download link has expired; download again from the job page", http.StatusNotFound)
		return
	}
	runnerName, id, _ := strings.Cut(target, "/")
	c, err := s.client(runnerName)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), transferTimeout)
	defer cancel()
	// Ask first, so an error can still be reported as JSON.
	j, err := c.Get(ctx, id)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if j.Results == nil || j.Results.Files == 0 {
		writeError(w, http.StatusNotFound, errNotFound("this job has no results"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`-results.tar"`)
	if err := c.Results(ctx, id, w); err != nil {
		s.log.Error("results download failed", "job", id, "err", err)
	}
}

// --- schedules ---

// ScheduleView is a schedule and the runner it belongs to. The harness
// document is left out of the list.
type ScheduleView struct {
	Runner string `json:"runner"`
	runner.Schedule
}

func (s *Server) schedules(w http.ResponseWriter, r *http.Request) {
	var mu sync.Mutex
	list := []ScheduleView{}
	views := s.eachRunner(r.Context(), func(ctx context.Context, c *runnerd.Client, ref RunnerRef) error {
		scheds, err := c.Schedules(ctx)
		mu.Lock()
		for _, sc := range scheds {
			sc.Document = nil
			list = append(list, ScheduleView{Runner: ref.Name, Schedule: sc})
		}
		mu.Unlock()
		return err
	})
	errs := map[string]string{}
	for _, v := range views {
		if v.Error != "" {
			errs[v.Name] = v.Error
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": list, "errors": errs})
}

func (s *Server) scheduleAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if r.Method == http.MethodDelete {
		action = "delete"
	} else if action != "pause" && action != "resume" {
		writeError(w, http.StatusNotFound, errNotFound("unknown action "+action))
		return
	}
	s.withRunner(w, r, func(ctx context.Context, c *runnerd.Client) (any, error) {
		id, err := jobID(r)
		if err != nil {
			return nil, err
		}
		s.log.Info("schedule "+action, "runner", r.PathValue("runner"), "schedule", id)
		return map[string]string{}, c.ScheduleAction(ctx, id, action)
	})
}

// --- approvals ---

// ApprovalView is a pending approval and its runner. Path and details
// come from the agent; the UI shows them as text.
type ApprovalView struct {
	Runner string `json:"runner"`
	runner.Approval
}

func (s *Server) approvals(w http.ResponseWriter, r *http.Request) {
	var mu sync.Mutex
	list := []ApprovalView{}
	views := s.eachRunner(r.Context(), func(ctx context.Context, c *runnerd.Client, ref RunnerRef) error {
		pending, err := c.Approvals(ctx)
		mu.Lock()
		for _, a := range pending {
			list = append(list, ApprovalView{Runner: ref.Name, Approval: a})
		}
		mu.Unlock()
		return err
	})
	errs := map[string]string{}
	for _, v := range views {
		if v.Error != "" {
			errs[v.Name] = v.Error
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": list, "errors": errs})
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Approve *bool `json:"approve"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if body.Approve == nil {
		writeError(w, http.StatusBadRequest, errString(`expected {"approve": true} or {"approve": false}`))
		return
	}
	s.withRunner(w, r, func(ctx context.Context, c *runnerd.Client) (any, error) {
		id, err := jobID(r)
		if err != nil {
			return nil, err
		}
		s.log.Warn("approval decided", "runner", r.PathValue("runner"), "approval", id, "approved", *body.Approve)
		return map[string]bool{"approved": *body.Approve}, c.Decide(ctx, id, *body.Approve)
	})
}

// --- harnesses ---

// Check is what the UI shows before a harness may start.
type Check struct {
	Valid   bool     `json:"valid"`
	Errors  []string `json:"errors,omitempty"`
	Name    string   `json:"name,omitempty"`
	Target  string   `json:"target,omitempty"`
	Risk    string   `json:"risk,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
	Explain []string `json:"explain,omitempty"`
	// Trigger is "schedule" or "loop" for harnesses that run
	// unattended: starting them adds a schedule.
	Trigger string `json:"trigger,omitempty"`
}

func checkHarness(yamlDoc string) (Check, *spec.Harness) {
	h, err := spec.Parse([]byte(yamlDoc))
	if err != nil {
		return Check{Errors: splitErrors(err)}, nil
	}
	e, err := policy.New(h)
	if err != nil {
		return Check{Errors: splitErrors(err)}, nil
	}
	risk, why := e.Risk()
	c := Check{Valid: true, Name: h.Metadata.Name, Target: h.Spec.Target, Risk: risk.String(), Reasons: why, Explain: e.Explain()}
	switch {
	case h.Spec.Trigger.Schedule != "":
		c.Trigger = "schedule"
	case h.Spec.Trigger.Loop != nil:
		c.Trigger = "loop"
	}
	return c, h
}

type yamlBody struct {
	YAML string `json:"yaml"`
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	var b yamlBody
	if !readJSON(w, r, &b) {
		return
	}
	c, _ := checkHarness(b.YAML)
	writeJSON(w, http.StatusOK, c)
}

type runRequest struct {
	Runner string `json:"runner"`
	YAML   string `json:"yaml"`
	// Confirmed must be true: the user saw the explanation of exactly
	// this document and chose to start it.
	Confirmed bool `json:"confirmed"`
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !req.Confirmed {
		writeError(w, http.StatusBadRequest, errString("confirm that you reviewed what this harness may do"))
		return
	}
	chk, h := checkHarness(req.YAML)
	if !chk.Valid {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the harness is not valid", "check": chk})
		return
	}
	c, err := s.client(req.Runner)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runnerTimeout)
	defer cancel()
	st, err := c.Info(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if st.Target != h.Spec.Target {
		writeError(w, http.StatusBadRequest, fmt.Errorf("this harness is meant for %q, but runner %s is %q: change spec.target or pick another runner",
			h.Spec.Target, req.Runner, st.Target))
		return
	}
	if chk.Trigger != "" {
		sc, err := c.AddSchedule(ctx, []byte(req.YAML))
		if err != nil {
			writeError(w, statusFor(err), err)
			return
		}
		s.log.Info("schedule added", "runner", req.Runner, "schedule", sc.ID, "harness", h.Metadata.Name, "risk", chk.Risk)
		writeJSON(w, http.StatusCreated, map[string]string{"runner": req.Runner, "schedule": sc.ID})
		return
	}
	id, err := c.Submit(ctx, []byte(req.YAML))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	s.log.Info("job started", "runner", req.Runner, "job", id, "harness", h.Metadata.Name, "risk", chk.Risk)
	writeJSON(w, http.StatusCreated, map[string]string{"runner": req.Runner, "id": id})
}

func (s *Server) harnesses(w http.ResponseWriter, _ *http.Request) {
	list, err := s.lib.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if list == nil {
		list = []Entry{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) harness(w http.ResponseWriter, r *http.Request) {
	data, err := s.lib.Get(r.PathValue("name"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	c, _ := checkHarness(string(data))
	writeJSON(w, http.StatusOK, map[string]any{"yaml": string(data), "check": c})
}

func (s *Server) saveHarness(w http.ResponseWriter, r *http.Request) {
	var b yamlBody
	if !readJSON(w, r, &b) {
		return
	}
	name, err := s.lib.Put([]byte(b.YAML))
	if err != nil {
		chk, _ := checkHarness(b.YAML)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "check": chk})
		return
	}
	s.log.Info("harness saved", "harness", name)
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) deleteHarness(w http.ResponseWriter, r *http.Request) {
	if err := s.lib.Delete(r.PathValue("name")); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	s.log.Info("harness deleted", "harness", r.PathValue("name"))
	writeJSON(w, http.StatusOK, map[string]string{})
}

// --- helpers ---

func statusFor(err error) int {
	var nf notFound
	var api *runnerd.APIError
	switch {
	case errors.As(err, &nf), errors.Is(err, ErrNoHarness):
		return http.StatusNotFound
	case errors.As(err, &api):
		if api.Status >= 400 && api.Status < 500 {
			return api.Status
		}
		return http.StatusBadGateway
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "runner unreachable"):
		return http.StatusBadGateway
	default:
		return http.StatusBadRequest
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, errString("expected application/json"))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request: %w", err))
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, errString("invalid request: trailing data"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// splitErrors flattens errors.Join trees into one message each.
func splitErrors(err error) []string {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		var out []string
		for _, e := range joined.Unwrap() {
			out = append(out, splitErrors(e)...)
		}
		return out
	}
	return []string{err.Error()}
}

// ConfigPaths returns the config file and harness library in dir.
func ConfigPaths(dir string) (cfgPath, libDir string) {
	return filepath.Join(dir, "control.yaml"), filepath.Join(dir, "harnesses")
}
