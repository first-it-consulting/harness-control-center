package runnerd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

// Client talks to a runner daemon.
type Client struct {
	http *http.Client
}

// baseURL is a placeholder host: every connection goes to the dialer.
const baseURL = "http://runner"

// NewUnixClient connects to a daemon's local socket.
func NewUnixClient(socket string) *Client {
	return newClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	})
}

var (
	// sshHostRe forbids a leading '-' so a host can never be parsed as an
	// ssh option (e.g. "-oProxyCommand=...").
	sshHostRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@-]{0,252}$`)
	// The remote socket path is passed through the remote shell, so only
	// shell-inert characters are allowed.
	remotePathRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,255}$`)
)

// NewSSHClient reaches a remote daemon through ssh, using the user's ssh
// configuration (host keys, keys, jump hosts): each connection runs
// "hcc runner stdio" on the remote host, which relays to its local
// socket. Nothing listens on the network.
func NewSSHClient(host, remoteSocket string) (*Client, error) {
	if !sshHostRe.MatchString(host) {
		return nil, fmt.Errorf("runnerd: invalid ssh host %q", host)
	}
	if !remotePathRe.MatchString(remoteSocket) {
		return nil, fmt.Errorf("runnerd: invalid remote socket path %q", remoteSocket)
	}
	return newClient(func(ctx context.Context) (net.Conn, error) {
		//nolint:gosec // host and socket are validated above; "--" ends ssh's options
		cmd := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "--", host,
			"hcc", "runner", "stdio", "--socket", remoteSocket)
		return commandConn(cmd)
	}), nil
}

func newClient(dial func(context.Context) (net.Conn, error)) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:    2,
		IdleConnTimeout: 30 * time.Second,
	}}}
}

// APIError is an error reported by the daemon.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("runner unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// Submit sends a harness document and returns the job ID.
func (c *Client) Submit(ctx context.Context, harnessYAML []byte) (string, error) {
	var out struct{ ID string }
	if err := c.do(ctx, http.MethodPost, "/v1/jobs", bytes.NewReader(harnessYAML), &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Info returns the runner's status.
func (c *Client) Info(ctx context.Context) (runner.Status, error) {
	var st runner.Status
	return st, c.do(ctx, http.MethodGet, "/v1/info", nil, &st)
}

// List returns all jobs.
func (c *Client) List(ctx context.Context) ([]runner.Job, error) {
	var jobs []runner.Job
	return jobs, c.do(ctx, http.MethodGet, "/v1/jobs", nil, &jobs)
}

// Get returns one job.
func (c *Client) Get(ctx context.Context, id string) (runner.Job, error) {
	var j runner.Job
	if err := sandbox.ValidateID(id); err != nil {
		return j, err
	}
	return j, c.do(ctx, http.MethodGet, "/v1/jobs/"+id, nil, &j)
}

// Cancel stops one job.
func (c *Client) Cancel(ctx context.Context, id string) error {
	if err := sandbox.ValidateID(id); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/v1/jobs/"+id+"/cancel", nil, nil)
}

// KillAll engages the kill switch.
func (c *Client) KillAll(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/kill", nil, nil)
}

// Resume releases the kill switch.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/resume", nil, nil)
}

// Console writes a job's console log to w.
func (c *Client) Console(ctx context.Context, id string, w io.Writer) error {
	if err := sandbox.ValidateID(id); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/jobs/"+id+"/console", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("runner unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Message: resp.Status}
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Results writes a job's results archive to w.
func (c *Client) Results(ctx context.Context, id string, w io.Writer) error {
	if err := sandbox.ValidateID(id); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/jobs/"+id+"/results", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("runner unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Follow calls fn for each event of job id, from the first, until the
// job is terminal (it then returns that final event), fn returns false,
// or ctx ends.
func (c *Client) Follow(ctx context.Context, id string, fn func(runner.Event) bool) (runner.Event, error) {
	var last runner.Event
	if err := sandbox.ValidateID(id); err != nil {
		return last, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/events?job="+id, nil)
	if err != nil {
		return last, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return last, fmt.Errorf("runner unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return last, &APIError{Status: resp.StatusCode, Message: resp.Status}
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var e runner.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return last, fmt.Errorf("runnerd: bad event: %w", err)
		}
		if e.To == "" {
			return last, errors.New("runnerd: " + string(sc.Bytes()))
		}
		last = e
		if !fn(e) || e.To.Terminal() {
			return last, nil
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return last, err
	}
	return last, ctx.Err()
}
