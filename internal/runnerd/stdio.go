package runnerd

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

// Relay copies between a local stream (stdin/stdout of "hcc runner
// stdio", i.e. an ssh session) and the daemon socket until either side
// closes. Access control is the socket's file mode plus ssh itself.
func Relay(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, in)
		// Client finished sending: half-close, but keep relaying the
		// daemon's response.
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, conn)
		done <- err
	}()
	// The relay ends when the daemon closes its side.
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cmdConn is a net.Conn over a command's stdin and stdout (the ssh
// transport).
type cmdConn struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	once sync.Once
}

func commandConn(cmd *exec.Cmd) (net.Conn, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &cmdConn{cmd: cmd, in: in, out: out}, nil
}

func (c *cmdConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *cmdConn) Write(p []byte) (int, error) { return c.in.Write(p) }

func (c *cmdConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.in.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		werr := c.cmd.Wait()
		var exit *exec.ExitError
		if werr != nil && !errors.As(werr, &exit) {
			err = errors.Join(err, werr)
		}
	})
	return err
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "ssh" }
func (pipeAddr) String() string  { return "ssh" }

func (c *cmdConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (c *cmdConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (c *cmdConn) SetDeadline(time.Time) error      { return nil }
func (c *cmdConn) SetReadDeadline(time.Time) error  { return nil }
func (c *cmdConn) SetWriteDeadline(time.Time) error { return nil }
