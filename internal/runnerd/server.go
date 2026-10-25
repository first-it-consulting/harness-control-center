package runnerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// API routes (all under the Unix socket; there is no TCP listener).
//
//	POST /v1/jobs               submit a harness (YAML body)
//	GET  /v1/jobs               list jobs
//	GET  /v1/jobs/{id}          one job
//	POST /v1/jobs/{id}/cancel   cancel one job
//	GET  /v1/jobs/{id}/console  console log
//	GET  /v1/events[?job=ID]    NDJSON event stream
//	POST /v1/kill               kill switch
//	POST /v1/resume             accept jobs again

// Server serves a runner over a Unix socket.
type Server struct {
	r        *runner.Runner
	stateDir string
	log      *slog.Logger
	http     *http.Server
}

// NewServer returns a server for r. Console logs are read from stateDir
// (see ConsoleFunc).
func NewServer(r *runner.Runner, stateDir string, log *slog.Logger) *Server {
	s := &Server{r: r, stateDir: stateDir, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs", s.submit)
	mux.HandleFunc("GET /v1/jobs", s.list)
	mux.HandleFunc("GET /v1/jobs/{id}", s.get)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("GET /v1/jobs/{id}/console", s.console)
	mux.HandleFunc("GET /v1/events", s.events)
	mux.HandleFunc("POST /v1/kill", s.kill)
	mux.HandleFunc("POST /v1/resume", s.resume)
	s.http = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		// No WriteTimeout: /v1/events streams for as long as a job runs.
	}
	return s
}

// ConsoleFunc returns a runner.Config.Console that writes size-capped
// logs below stateDir.
func ConsoleFunc(stateDir string, log *slog.Logger) func(string) io.Writer {
	return func(id string) io.Writer {
		c, err := openConsoleLog(stateDir, id)
		if err != nil {
			log.Error("console log unavailable", "job", id, "err", err)
			return io.Discard
		}
		return c
	}
}

// Listen creates the socket so that only this user can connect: its
// directory is 0700 and the socket itself 0600. A stale socket is
// replaced; any other file at that path is an error.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("runnerd: %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// Serve serves on l until ctx ends, then stops accepting requests and
// shuts the runner down (stopping every job).
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Stop the runner first so streaming requests see their jobs end.
	rerr := s.r.Shutdown(sctx)
	herr := s.http.Shutdown(sctx)
	return errors.Join(rerr, herr)
}

func (s *Server) submit(w http.ResponseWriter, req *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, spec.MaxDocumentBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	h, err := spec.Parse(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id, err := s.r.Submit(h)
	switch {
	case errors.Is(err, runner.ErrKilled), errors.Is(err, runner.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		s.log.Info("job submitted", "job", id, "harness", h.Metadata.Name)
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	}
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.r.List())
}

func (s *Server) job(w http.ResponseWriter, req *http.Request) (runner.Job, bool) {
	id := req.PathValue("id")
	if err := sandbox.ValidateID(id); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return runner.Job{}, false
	}
	j, ok := s.r.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, runner.ErrNotFound)
	}
	return j, ok
}

func (s *Server) get(w http.ResponseWriter, req *http.Request) {
	if j, ok := s.job(w, req); ok {
		writeJSON(w, http.StatusOK, j)
	}
}

func (s *Server) cancel(w http.ResponseWriter, req *http.Request) {
	j, ok := s.job(w, req)
	if !ok {
		return
	}
	if err := s.r.Cancel(j.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("job cancel requested", "job", j.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": j.ID})
}

func (s *Server) console(w http.ResponseWriter, req *http.Request) {
	j, ok := s.job(w, req)
	if !ok {
		return
	}
	p, err := consolePath(s.stateDir, j.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f, err := os.Open(p) //nolint:gosec // path built from a validated id
	if errors.Is(err, os.ErrNotExist) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		return // not started yet: empty log
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, io.LimitReader(f, MaxConsoleBytes+int64(len(truncatedMarker))))
}

// events streams events as NDJSON. With ?job=ID it first replays that
// job's events so far, then streams the rest, and ends once the job is
// terminal: a client that connects late still sees every event once.
func (s *Server) events(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	jobID := req.URL.Query().Get("job")
	var (
		history []runner.Event
		ch      <-chan runner.Event
		stop    func()
	)
	if jobID != "" {
		if err := sandbox.ValidateID(jobID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var err error
		if history, ch, stop, err = s.r.Watch(jobID, 256); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
	} else {
		ch, stop = s.r.Subscribe(256)
	}
	defer stop()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	// send writes one event; it returns false when the stream should end.
	send := func(e runner.Event) bool {
		if err := enc.Encode(e); err != nil {
			return false
		}
		flusher.Flush()
		return jobID == "" || !e.To.Terminal()
	}
	for _, e := range history {
		if !send(e) {
			return
		}
	}
	for {
		select {
		case <-req.Context().Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			if jobID != "" && e.JobID != jobID {
				continue
			}
			if !send(e) {
				return
			}
		}
	}
}

func (s *Server) kill(w http.ResponseWriter, _ *http.Request) {
	s.r.KillAll()
	s.log.Warn("kill switch engaged")
	writeJSON(w, http.StatusOK, map[string]string{"status": "killed"})
}

func (s *Server) resume(w http.ResponseWriter, _ *http.Request) {
	s.r.Resume()
	s.log.Info("runner resumed")
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepting"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
