package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"golang.org/x/term"
)

// connFlags selects the runner to talk to.
type connFlags struct {
	socket, ssh, remoteSocket string
}

func (c *connFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.socket, "socket", runnerd.DefaultSocket, "runner socket on this machine")
	fs.StringVar(&c.ssh, "ssh", "", "reach the runner on this host through ssh (uses your ssh config)")
	fs.StringVar(&c.remoteSocket, "remote-socket", runnerd.DefaultSocket, "runner socket on the --ssh host")
}

func (c *connFlags) client() (*runnerd.Client, error) {
	if c.ssh != "" {
		return runnerd.NewSSHClient(c.ssh, c.remoteSocket)
	}
	return runnerd.NewUnixClient(c.socket), nil
}

func (c *connFlags) where() string {
	if c.ssh != "" {
		return c.ssh
	}
	return "this computer"
}

func cmdRun(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var conn connFlags
	conn.register(fs)
	yes := fs.Bool("yes", false, "start without asking for confirmation")
	console := fs.Bool("console", false, "print the guest console when the job ends")
	raw := fs.Bool("raw", false, "with --console: print the console byte for byte (control characters included)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "hcc run: expected exactly one harness file")
		return exitUsage
	}
	data, e, err := load(pos[0])
	if err != nil {
		printInvalid(stderr, pos[0], err)
		return exitInvalid
	}
	c, err := conn.client()
	if err != nil {
		fmt.Fprintln(stderr, "hcc run:", err)
		return exitUsage
	}

	explain(stdout, e)
	fmt.Fprintln(stdout)
	if !*yes {
		ok, err := confirm(stdin, stdout, fmt.Sprintf("Start this harness on %s?", conn.where()))
		if err != nil {
			fmt.Fprintln(stderr, "hcc run:", err)
			return exitUsage
		}
		if !ok {
			fmt.Fprintln(stdout, "Not started.")
			return exitOK
		}
	}

	ctx := context.Background()
	id, err := c.Submit(ctx, data)
	if err != nil {
		fmt.Fprintln(stderr, "hcc run: not started:", err)
		return exitInvalid
	}
	fmt.Fprintf(stdout, "Started job %s on %s. Press Ctrl-C to stop it.\n", id, conn.where())

	// Ctrl-C stops the job (not just this command), then we keep
	// following until the runner confirms it ended.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		if _, ok := <-sigs; ok {
			fmt.Fprintln(stdout, "\nStopping the job…")
			if err := c.Cancel(context.Background(), id); err != nil {
				fmt.Fprintln(stderr, "hcc run: could not stop the job:", err)
			}
		}
	}()

	var started time.Time
	last, err := c.Follow(ctx, id, func(ev runner.Event) bool {
		if ev.To == runner.StateRunning {
			started = ev.At
		}
		line := fmt.Sprintf("  %s  %s", ev.At.Local().Format("15:04:05"), ev.To)
		if ev.To.Terminal() && !started.IsZero() {
			line += fmt.Sprintf(" after %s", ev.At.Sub(started).Round(100*time.Millisecond))
		}
		if ev.Message != "" {
			line += ": " + ev.Message
		}
		fmt.Fprintln(stdout, line)
		return true
	})
	if err != nil {
		fmt.Fprintln(stderr, "hcc run: lost track of the job:", err)
		return exitInvalid
	}
	if *console {
		fmt.Fprintln(stdout, "\n--- console ---")
		if err := printConsole(ctx, c, id, *raw, stdout); err != nil {
			fmt.Fprintln(stderr, "hcc run: console:", err)
		}
	}
	if last.To != runner.StateSucceeded {
		return exitJob
	}
	return exitOK
}

// confirm asks a yes/no question. Without a terminal to ask on, it fails
// instead of guessing: starting an agent needs an explicit yes.
func confirm(stdin io.Reader, stdout io.Writer, question string) (bool, error) {
	// A real terminal, not just any character device (/dev/null is one).
	if f, ok := stdin.(*os.File); ok && !term.IsTerminal(int(f.Fd())) {
		return false, errors.New("no terminal to confirm on; review with \"hcc explain\" and pass --yes")
	}
	fmt.Fprintf(stdout, "%s [y/N] ", question)
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func cmdJobs(cmd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var conn connFlags
	conn.register(fs)
	var raw *bool
	if cmd == "logs" {
		raw = fs.Bool("raw", false, "print the console byte for byte (control characters included)")
	}
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	needsID := cmd == "logs" || cmd == "cancel"
	if (needsID && len(pos) != 1) || (!needsID && len(pos) != 0) {
		fmt.Fprintf(stderr, "hcc %s: wrong number of arguments\n\n%s", cmd, usage)
		return exitUsage
	}
	c, err := conn.client()
	if err != nil {
		fmt.Fprintf(stderr, "hcc %s: %v\n", cmd, err)
		return exitUsage
	}
	ctx := context.Background()
	switch cmd {
	case "jobs":
		var jobs []runner.Job
		if jobs, err = c.List(ctx); err == nil {
			printJobs(stdout, jobs)
		}
	case "logs":
		err = printConsole(ctx, c, pos[0], *raw, stdout)
	case "cancel":
		if err = c.Cancel(ctx, pos[0]); err == nil {
			fmt.Fprintf(stdout, "Stopping %s.\n", pos[0])
		}
	case "kill":
		if err = c.KillAll(ctx); err == nil {
			fmt.Fprintf(stdout, "Kill switch engaged on %s: all jobs are being stopped and new ones are refused until \"hcc resume\".\n", conn.where())
		}
	case "resume":
		if err = c.Resume(ctx); err == nil {
			fmt.Fprintf(stdout, "%s accepts jobs again.\n", conn.where())
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "hcc %s: %v\n", cmd, err)
		return exitInvalid
	}
	return exitOK
}

// printConsole prints a job's console. Unless raw, control characters are
// shown visibly: the console is written by the agent and must not be able
// to drive the user's terminal.
func printConsole(ctx context.Context, c *runnerd.Client, id string, raw bool, w io.Writer) error {
	var buf bytes.Buffer // at most runnerd.MaxConsoleBytes
	if err := c.Console(ctx, id, &buf); err != nil {
		return err
	}
	if raw {
		_, err := w.Write(buf.Bytes())
		return err
	}
	return writeSafe(w, &buf)
}

func printJobs(w io.Writer, jobs []runner.Job) {
	if len(jobs) == 0 {
		fmt.Fprintln(w, "No jobs.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tHARNESS\tSTATE\tCREATED\tTOKENS\tDETAIL")
	for _, j := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Harness, j.State, j.Created.Local().Format("2006-01-02 15:04:05"), formatUsage(j), j.Error)
	}
	_ = tw.Flush()
}

// formatUsage formats a job's metered model usage for the jobs table.
func formatUsage(j runner.Job) string {
	if j.Usage == nil {
		return "-"
	}
	out := fmt.Sprintf("%d in / %d out", j.Usage.InputTokens, j.Usage.OutputTokens)
	if j.Usage.CostKnown {
		out += fmt.Sprintf(" ($%.4f)", j.Usage.CostUSD)
	}
	return out
}
