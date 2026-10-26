package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/mcp"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// MCP servers run as plugins (ADR-0014): a job's agent reaches each of
// its plugin servers at http://<its proxy>/hcc/v1/mcp/<server> (MCP's
// streamable HTTP transport, JSON responses only). The runner starts the
// plugin, in a sandbox of its own, on first use, holds tool calls its
// approval gates name, and relays everything else as is. The plugin
// stops with the job.

// MCPPlugins runs mcp-server plugins for jobs.
type MCPPlugins interface {
	// CheckMCP reports whether plugin is an installed mcp-server plugin.
	CheckMCP(plugin string) error
	// StartMCP starts plugin for a job; it runs until ctx ends.
	StartMCP(ctx context.Context, jobID, plugin string) (MCPConn, error)
}

// MCPConn is a connection to a running MCP server plugin.
type MCPConn interface {
	// Call sends a request and returns the server's response to it.
	Call(ctx context.Context, id string, msg []byte) ([]byte, error)
	// Send passes a notification (or a response) on.
	Send(msg []byte) error
}

// Limits of the relay.
const (
	maxMCPMessage  = 4 << 20
	mcpCallTimeout = 10 * time.Minute
)

// mcpAPI relays one job's plugin MCP servers.
type mcpAPI struct {
	r       *Runner
	job     *job              // set under r.mu before the guest exists
	servers map[string]string // server name → plugin

	mu    sync.Mutex
	conns map[string]MCPConn
}

// pluginServers returns h's MCP servers that are plugins.
func pluginServers(h *spec.Harness) map[string]string {
	out := map[string]string{}
	for _, s := range h.Spec.MCP.Servers {
		if s.Plugin != "" {
			out[s.Name] = s.Plugin
		}
	}
	return out
}

func (a *mcpAPI) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	name := strings.TrimPrefix(req.URL.Path, "/hcc/v1/mcp/")
	plugin, ok := a.servers[name]
	if !ok {
		http.Error(w, "no such MCP server", http.StatusNotFound)
		return
	}
	if req.Method != http.MethodPost {
		// No server-to-client stream: responses come with each request.
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "only POST", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxMCPMessage))
	if err != nil {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}
	msg, err := mcp.Parse(body)
	if err != nil {
		writeRPC(w, mcp.ErrorResponse("", -32700, err.Error()))
		return
	}
	a.r.mu.Lock()
	j := a.job
	a.r.mu.Unlock()
	if msg.Method == "tools/call" {
		if resp := a.gate(req.Context(), j, name, msg, body); resp != nil {
			writeRPC(w, resp)
			return
		}
	}
	conn, err := a.conn(j, name, plugin)
	if err != nil {
		writeRPC(w, mcp.ErrorResponse(msg.ID, mcp.CodeUnavailable, "the MCP server could not start: "+err.Error()))
		return
	}
	if !msg.IsRequest() {
		if err := conn.Send(body); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), mcpCallTimeout)
	defer cancel()
	resp, err := conn.Call(ctx, msg.ID, body)
	if err != nil {
		writeRPC(w, mcp.ErrorResponse(msg.ID, mcp.CodeUnavailable, err.Error()))
		return
	}
	writeRPC(w, resp)
}

// gate holds a tool call its job's approval gates name until a person
// decides. It returns the response for a refused call, nil to go on.
func (a *mcpAPI) gate(ctx context.Context, j *job, server string, msg mcp.Message, body []byte) []byte {
	calls, err := mcp.ToolCalls(body)
	if err != nil || len(calls) != 1 {
		return mcp.ErrorResponse(msg.ID, -32602, "a tool call the runner cannot read")
	}
	c := calls[0]
	action := policy.MCPAction(server, c.Tool)
	eng, err := policy.New(j.harness)
	if err != nil {
		return mcp.ErrorResponse(msg.ID, mcp.CodeNotApproved, err.Error())
	}
	if !eng.RequiresApproval(action) {
		return nil
	}
	d, err := mcp.DescribeToolCall(c)
	if err != nil {
		return mcp.ErrorResponse(msg.ID, mcp.CodeNotApproved, "this tool call cannot be approved: "+err.Error())
	}
	err = a.r.awaitApproval(ctx, j, ApprovalRequest{
		Actions: []string{action}, Method: "MCP", Host: server, Path: "/" + c.Tool, Details: []string{server + ": " + d},
	})
	if err != nil {
		return mcp.ErrorResponse(msg.ID, mcp.CodeNotApproved, "not approved: "+err.Error())
	}
	return nil
}

// conn returns the connection to a server, starting its plugin on first
// use. A failed start is retried on the next message.
func (a *mcpAPI) conn(j *job, name, plugin string) (MCPConn, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.conns[name]; ok {
		return c, nil
	}
	if a.r.cfg.MCP == nil {
		return nil, errors.New("this runner cannot run MCP plugins")
	}
	c, err := a.r.cfg.MCP.StartMCP(j.ctx, j.ID, plugin)
	if err != nil {
		return nil, err
	}
	if a.conns == nil {
		a.conns = map[string]MCPConn{}
	}
	a.conns[name] = c
	a.r.cfg.Log.Info("MCP server started", "job", j.ID, "server", name, "plugin", plugin)
	return c, nil
}

func writeRPC(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b) //nolint:gosec // JSON-RPC for the agent's MCP client, not a page
}

// checkMCP rejects a harness whose plugin MCP servers this runner cannot
// run.
func (r *Runner) checkMCP(h *spec.Harness) error {
	var errs []error
	for name, plugin := range pluginServers(h) {
		if r.cfg.MCP == nil {
			errs = append(errs, fmt.Errorf("MCP server %s: this runner cannot run MCP plugins", name))
			continue
		}
		if err := r.cfg.MCP.CheckMCP(plugin); err != nil {
			errs = append(errs, fmt.Errorf("MCP server %s: %w", name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("runner: %w", err)
	}
	return nil
}
