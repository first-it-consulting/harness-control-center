package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/mcp"
	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// An mcp-server plugin (ADR-0014) runs for one job, in its own sandbox,
// as long as the job does. Inside, hcc-guest plugin-shim runs the
// plugin's stdio MCP server and carries its lines to and from the
// runner through the sandbox's proxy:
//
//	GET  /hcc/v1/plugin/in   lines for the server's stdin (long poll; 204: none yet)
//	POST /hcc/v1/plugin/out  lines from the server's stdout
//
// The runner matches responses to the requests the job's agent made.

var _ runner.MCPPlugins = (*Manager)(nil)

// Limits of an MCP session.
const (
	pollWait       = 25 * time.Second
	maxShimBody    = 8 << 20
	sendTimeout    = 30 * time.Second
	slotWait       = 30 * time.Second
	maxPendingMCPs = 64
)

// CheckMCP implements runner.MCPPlugins.
func (m *Manager) CheckMCP(plugin string) error {
	_, p, _, err := m.cfg.Store.Get(plugin)
	if err != nil {
		return err
	}
	if p.Spec.Type != spec.PluginMCPServer {
		return fmt.Errorf("plugin %s is a %s, not an mcp-server", plugin, p.Spec.Type)
	}
	return nil
}

// StartMCP implements runner.MCPPlugins. The plugin takes one of the
// plugin sandbox slots until ctx (its job) ends.
func (m *Manager) StartMCP(ctx context.Context, jobID, plugin string) (runner.MCPConn, error) {
	if err := m.CheckMCP(plugin); err != nil {
		return nil, err
	}
	_, p, bundle, err := m.cfg.Store.Get(plugin)
	if err != nil {
		return nil, err
	}
	wait, cancel := context.WithTimeout(ctx, slotWait)
	defer cancel()
	select {
	case m.vms <- struct{}{}:
	case <-wait.Done():
		return nil, errors.New("no plugin sandbox is free (maxPluginVMs)")
	}
	s := newSession()
	id := newRunID()
	m.wg.Go(func() {
		defer func() { <-m.vms }()
		err := m.runPlugin(ctx, id, p, bundle, runOpts{local: s})
		if err != nil && ctx.Err() == nil {
			m.cfg.Log.Warn("MCP plugin stopped", "plugin", plugin, "job", jobID, "run", id, "err", err)
		}
		s.end(err)
	})
	m.cfg.Log.Info("MCP plugin started", "plugin", plugin, "job", jobID, "run", id)
	return s, nil
}

// session is one running MCP server plugin.
type session struct {
	in   chan []byte
	done chan struct{}

	mu      sync.Mutex
	pending map[string]chan []byte
	err     error
}

func newSession() *session {
	return &session{in: make(chan []byte, 64), done: make(chan struct{}), pending: map[string]chan []byte{}}
}

func (s *session) end(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
		if s.err == nil {
			s.err = errors.New("the MCP server stopped")
		}
		close(s.done)
	}
}

func (s *session) stopped() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Errorf("the MCP server stopped: %w", s.err)
}

// Call implements runner.MCPConn.
func (s *session) Call(ctx context.Context, id string, msg []byte) ([]byte, error) {
	ch := make(chan []byte, 1)
	s.mu.Lock()
	if _, dup := s.pending[id]; dup || len(s.pending) >= maxPendingMCPs {
		s.mu.Unlock()
		return nil, errors.New("too many requests in flight, or a request id used twice")
	}
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	if err := s.push(ctx, msg); err != nil {
		return nil, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return nil, errors.New("the MCP server did not answer in time")
	case <-s.done:
		return nil, s.stopped()
	}
}

// Send implements runner.MCPConn.
func (s *session) Send(msg []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	return s.push(ctx, msg)
}

func (s *session) push(ctx context.Context, msg []byte) error {
	select {
	case s.in <- bytes.TrimSpace(msg):
		return nil
	case <-ctx.Done():
		return errors.New("the MCP server is not reading")
	case <-s.done:
		return s.stopped()
	}
}

// ServeHTTP is the shim's API, served by the plugin sandbox's proxy.
func (s *session) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.Method == http.MethodGet && req.URL.Path == "/hcc/v1/plugin/in":
		s.serveIn(w, req)
	case req.Method == http.MethodPost && req.URL.Path == "/hcc/v1/plugin/out":
		s.serveOut(w, req)
	default:
		http.NotFound(w, req)
	}
}

func (s *session) serveIn(w http.ResponseWriter, req *http.Request) {
	timer := time.NewTimer(pollWait)
	defer timer.Stop()
	var out bytes.Buffer
	select {
	case msg := <-s.in:
		out.Write(msg)
		out.WriteByte('\n')
	case <-timer.C:
		w.WriteHeader(http.StatusNoContent)
		return
	case <-req.Context().Done():
		return
	case <-s.done:
		w.WriteHeader(http.StatusGone)
		return
	}
drain: // whatever else is waiting
	for out.Len() < maxShimBody/2 {
		select {
		case msg := <-s.in:
			out.Write(msg)
			out.WriteByte('\n')
		default:
			break drain
		}
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	_, _ = w.Write(out.Bytes())
}

func (s *session) serveOut(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxShimBody))
	if err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		msg, err := mcp.Parse(line)
		if err != nil || !msg.IsResponse() {
			continue // server-to-client requests and notifications are not relayed
		}
		s.mu.Lock()
		ch := s.pending[msg.ID]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- bytes.Clone(line):
			default:
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
