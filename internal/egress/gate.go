package egress

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/first-it-consulting/harness-control-center/internal/mcp"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
)

// GateRequest describes a request that needs a human's approval. Path
// and Details come from the agent: whoever shows them must neutralize
// control characters.
type GateRequest struct {
	Actions []string
	Method  string
	Host    string
	Path    string
	// Details: for a git push, the ref updates.
	Details []string
}

// Gate decides on a request that needs approval; nil lets it through.
// It blocks until a decision (or ctx ends).
type Gate func(ctx context.Context, r GateRequest) error

const (
	maxGatePath     = 512
	maxPushCommands = 100
	maxPushPreamble = 64 << 10
)

// gatedTransport wraps every brokered request. It refuses all of them
// once the job's budget is exhausted, and holds those whose actions need
// approval until the Gate decides. The model endpoint is never gated (the budget
// covers it). Without a Gate, such requests are refused: fail closed.
type gatedTransport struct {
	base http.RoundTripper
	p    *Proxy
	host string
	port int
}

func (t *gatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Once the budget is exhausted the job is being stopped: nothing
	// brokered goes out any more, whatever the host.
	if t.p.Meter != nil {
		if why := t.p.Meter.Exceeded(); why != "" {
			return plainResponse(req, http.StatusTooManyRequests, "budget exhausted: "+why), nil
		}
	}
	if t.host == t.p.ModelHost && t.port == t.p.ModelPort {
		return t.base.RoundTrip(req)
	}
	// An upgraded connection (e.g. WebSocket) would carry requests no
	// gate sees: none while gates are on.
	if t.p.Inspect && req.Header.Get("Upgrade") != "" {
		return plainResponse(req, http.StatusForbidden, "connection upgrades are not allowed for a job with approval gates"), nil
	}
	actions := policy.HTTPActions(req.Method, req.URL.Path)
	var details []string
	// On a host with a gated MCP server or A2A agent, every request with a
	// body is read, whatever its path or method: a server might answer
	// on paths or in forms the endpoint's URL does not name.
	if eps := t.p.gatedEndpoints(t.host, t.port); len(eps) > 0 && hasBody(req) {
		ep, err := pickEndpoint(eps, req.URL.Path)
		ce := req.Header.Get("Content-Encoding")
		switch {
		case err != nil:
		case ce != "" && !strings.EqualFold(ce, "identity"):
			err = fmt.Errorf("its body is encoded (%s)", ce)
		case ep.kind == kindMCP:
			req, actions, details, err = t.inspectMCP(req, ep.Endpoint, actions)
		default:
			req, actions, details, err = t.inspectA2A(req, ep.Endpoint, actions, details)
		}
		if err != nil {
			t.p.Log.Warn("egress: request to a gated MCP or A2A host refused", "job", t.p.JobID, "host", t.host, "path", req.URL.Path, "err", err)
			return plainResponse(req, http.StatusForbidden, "this request cannot be approved: "+err.Error()), nil
		}
	}
	if !t.p.Policy.RequiresApprovalAny(actions) {
		return t.base.RoundTrip(req)
	}
	uri := req.URL.RequestURI()
	if len(uri) > maxGatePath { // never ask about what cannot be shown in full
		return plainResponse(req, http.StatusForbidden, fmt.Sprintf("this request cannot be approved: its path is longer than %d bytes", maxGatePath)), nil
	}
	gr := GateRequest{Actions: actions, Method: req.Method, Host: t.host, Path: uri, Details: details}
	if req.Body != nil && req.Body != http.NoBody && strings.HasSuffix(req.URL.Path, "/git-receive-pack") {
		cmds, rest, err := pushCommands(req.Body)
		if err != nil {
			// Never ask about a push that cannot be shown in full.
			_ = req.Body.Close()
			t.p.Log.Warn("egress: push refused", "job", t.p.JobID, "host", t.host, "path", gr.Path, "err", err)
			return plainResponse(req, http.StatusForbidden, "this push cannot be approved: "+err.Error()), nil
		}
		req = req.Clone(req.Context())
		req.Body = rest
		gr.Details = cmds
	}
	if t.p.Gate == nil {
		return plainResponse(req, http.StatusForbidden, "this action needs approval, and no one can approve it on this runner"), nil
	}
	t.p.Log.Info("egress: waiting for approval", "job", t.p.JobID, "host", t.host, "method", req.Method, "path", gr.Path, "actions", actions)
	if err := t.p.Gate(req.Context(), gr); err != nil {
		t.p.Log.Warn("egress: not approved", "job", t.p.JobID, "host", t.host, "method", req.Method, "path", gr.Path, "err", err)
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return plainResponse(req, http.StatusForbidden, "not approved: "+err.Error()), nil
	}
	t.p.Log.Info("egress: approved", "job", t.p.JobID, "host", t.host, "method", req.Method, "path", gr.Path)
	return t.base.RoundTrip(req)
}

// pushCommands reads the ref updates at the start of a git-receive-pack
// request (pkt-lines up to the first flush) and returns them, with a body
// that still yields every byte. It fails unless it read them all.
func pushCommands(body io.ReadCloser) ([]string, io.ReadCloser, error) {
	var consumed bytes.Buffer
	br := bufio.NewReader(io.LimitReader(body, maxPushPreamble))
	r := io.TeeReader(br, &consumed)
	var cmds []string
	var err error
	for {
		if len(cmds) == maxPushCommands {
			err = fmt.Errorf("it updates more than %d refs", maxPushCommands)
			break
		}
		var hdr [4]byte
		if _, err = io.ReadFull(r, hdr[:]); err != nil {
			err = fmt.Errorf("its ref updates end early or exceed %d bytes", maxPushPreamble)
			break
		}
		n, perr := strconv.ParseUint(string(hdr[:]), 16, 16)
		if perr != nil {
			err = fmt.Errorf("bad pkt-line length %q", hdr[:])
			break
		}
		if n == 0 { // flush: the commands are over
			break
		}
		if n < 4 {
			err = fmt.Errorf("bad pkt-line length %d", n)
			break
		}
		line := make([]byte, n-4)
		if _, err = io.ReadFull(r, line); err != nil {
			err = fmt.Errorf("its ref updates end early or exceed %d bytes", maxPushPreamble)
			break
		}
		cmd, derr := describeRefUpdate(line)
		if derr != nil {
			err = derr
			break
		}
		cmds = append(cmds, cmd)
	}
	// What bufio read ahead (but we did not consume) goes back too.
	ahead, _ := br.Peek(br.Buffered())
	consumed.Write(ahead)
	rest := struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(consumed.Bytes()), body), body}
	return cmds, rest, err
}

// maxRefName bounds a ref name shown for approval.
const maxRefName = 256

// describeRefUpdate turns "old new ref\0caps" into "ref: old → new". It
// fails for anything it cannot show in full.
func describeRefUpdate(line []byte) (string, error) {
	if i := bytes.IndexByte(line, 0); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(string(line))
	if len(f) != 3 || len(f[0]) != 40 && len(f[0]) != 64 || len(f[1]) != len(f[0]) {
		return "", errors.New("it contains a ref update that is not in the expected form")
	}
	old, nw, ref := f[0], f[1], f[2]
	if len(ref) > maxRefName {
		return "", fmt.Errorf("it updates a ref whose name is longer than %d bytes", maxRefName)
	}
	switch {
	case strings.Trim(nw, "0") == "":
		return ref + ": delete (was " + short(old) + ")", nil
	case strings.Trim(old, "0") == "":
		return ref + ": create at " + short(nw), nil
	default:
		return ref + ": " + short(old) + " → " + short(nw), nil
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// inspectMCP reads a request to an MCP server and adds the actions of its
// tool calls (mcp.<server>.<tool>), each described in full.
func (t *gatedTransport) inspectMCP(req *http.Request, ep Endpoint, actions []string) (*http.Request, []string, []string, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, actions, nil, nil
	}
	body, err := readMCPBody(req.Body)
	if err != nil {
		return req, nil, nil, err
	}
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	calls, err := mcp.ToolCalls(body)
	if err != nil {
		return req, nil, nil, err
	}
	var details []string
	for _, c := range calls {
		d, err := mcp.DescribeToolCall(c)
		if err != nil {
			return req, nil, nil, err
		}
		actions = append(actions, policy.MCPAction(ep.Name, c.Tool))
		details = append(details, ep.Name+": "+d)
	}
	return req, actions, details, nil
}
