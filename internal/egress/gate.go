package egress

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

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
	actions := policy.HTTPActions(req.Method, req.URL.Path)
	if !t.p.Policy.RequiresApprovalAny(actions) {
		return t.base.RoundTrip(req)
	}
	gr := GateRequest{Actions: actions, Method: req.Method, Host: t.host, Path: truncate(req.URL.RequestURI(), maxGatePath)}
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
		cmds = append(cmds, describeRefUpdate(line))
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

// describeRefUpdate turns "old new ref\0caps" into "ref: old → new".
func describeRefUpdate(line []byte) string {
	if i := bytes.IndexByte(line, 0); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(string(line))
	if len(f) != 3 {
		return truncate(string(line), 200)
	}
	old, nw, ref := f[0], f[1], truncate(f[2], 200)
	switch {
	case strings.Trim(nw, "0") == "":
		return ref + ": delete (was " + short(old) + ")"
	case strings.Trim(old, "0") == "":
		return ref + ": create at " + short(nw)
	default:
		return ref + ": " + short(old) + " → " + short(nw)
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
