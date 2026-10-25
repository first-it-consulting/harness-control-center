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
	"github.com/first-it-consulting/harness-control-center/internal/safetext"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
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

// flags repeats the connection flags for a suggested command line.
func (c *connFlags) flags() string {
	if c.ssh != "" {
		return " --ssh " + c.ssh
	}
	if c.socket != runnerd.DefaultSocket {
		return " --socket " + c.socket
	}
	return ""
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
	if h, err := spec.Parse(data); err == nil && (h.Spec.Trigger.Schedule != "" || h.Spec.Trigger.Loop != nil) {
		sc, err := c.AddSchedule(ctx, data)
		if err != nil {
			fmt.Fprintln(stderr, "hcc run: not scheduled:", err)
			return exitInvalid
		}
		fmt.Fprintf(stdout, "Added schedule %s on %s. %s\n", sc.ID, conn.where(), describeNext(sc))
		fmt.Fprintf(stdout, "See it with \"hcc schedules%s\"; stop it with \"hcc schedule pause %s%s\".\n", conn.flags(), sc.ID, conn.flags())
		return exitOK
	}
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
			line += ": " + safetext.Line(ev.Message)
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
	if j, err := c.Get(ctx, id); err == nil && j.Results != nil {
		fmt.Fprintln(stdout)
		printResults(stdout, j.Results)
		if j.Results.Files > 0 {
			fmt.Fprintf(stdout, "Save them with: hcc results %s -o %s.tar%s\n", id, id, conn.flags())
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

// describeNext says when a schedule runs next.
func describeNext(sc runner.Schedule) string {
	switch {
	case sc.State != runner.ScheduleActive:
		return "It is " + sc.State + "."
	case sc.NextRun.IsZero():
		return "A run is going."
	default:
		return "Next run: " + sc.NextRun.Local().Format("2006-01-02 15:04 MST") + "."
	}
}

func cmdSchedules(cmd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var conn connFlags
	conn.register(fs)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	c, err := conn.client()
	if err != nil {
		fmt.Fprintf(stderr, "hcc %s: %v\n", cmd, err)
		return exitUsage
	}
	ctx := context.Background()
	if cmd == "schedules" {
		if len(pos) != 0 {
			fmt.Fprintf(stderr, "hcc schedules: no arguments expected\n\n%s", usage)
			return exitUsage
		}
		list, err := c.Schedules(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "hcc schedules:", err)
			return exitInvalid
		}
		printSchedules(stdout, list)
		return exitOK
	}
	if len(pos) != 2 || (pos[0] != "pause" && pos[0] != "resume" && pos[0] != "delete") {
		fmt.Fprintf(stderr, "hcc schedule: expected pause, resume or delete and a schedule\n\n%s", usage)
		return exitUsage
	}
	if err := c.ScheduleAction(ctx, pos[1], pos[0]); err != nil {
		fmt.Fprintf(stderr, "hcc schedule %s: %v\n", pos[0], err)
		return exitInvalid
	}
	fmt.Fprintf(stdout, "Schedule %s: %s done.\n", pos[1], pos[0])
	return exitOK
}

func printSchedules(w io.Writer, list []runner.Schedule) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No schedules.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCHEDULE\tHARNESS\tWHEN\tSTATE\tRUNS\tNEXT RUN\tDETAIL")
	for _, s := range list {
		when := "cron " + s.Cron + " (UTC)"
		if s.Cron == "" {
			when = fmt.Sprintf("loop, at most %d runs", s.MaxRuns)
		}
		next := "-"
		if !s.NextRun.IsZero() {
			next = s.NextRun.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", s.ID, s.Harness, when, s.State, s.Runs, next, safetext.Line(s.Message))
	}
	_ = tw.Flush()
}

// cmdResults saves a job's results archive. It never writes to a
// terminal: the archive is the agent's content.
func cmdResults(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("results", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var conn connFlags
	conn.register(fs)
	out := fs.String("o", "", `file to create (it must not exist), or "-" for stdout (not a terminal)`)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) != 1 || *out == "" {
		fmt.Fprintf(stderr, "hcc results: expected a job and -o <file>\n\n%s", usage)
		return exitUsage
	}
	c, err := conn.client()
	if err != nil {
		fmt.Fprintln(stderr, "hcc results:", err)
		return exitUsage
	}
	ctx := context.Background()
	id := pos[0]
	if *out == "-" {
		if f, ok := stdout.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			fmt.Fprintln(stderr, "hcc results: refusing to write an archive to a terminal; use -o <file>")
			return exitUsage
		}
		if err := c.Results(ctx, id, stdout); err != nil {
			printErrSafe(stderr, "hcc results", err)
			return exitInvalid
		}
		return exitOK
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		fmt.Fprintln(stderr, "hcc results:", err)
		return exitInvalid
	}
	err = c.Results(ctx, id, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(*out)
		printErrSafe(stderr, "hcc results", err)
		return exitInvalid
	}
	fmt.Fprintf(stdout, "Saved the results of %s to %s.\n", id, *out)
	if j, err := c.Get(ctx, id); err == nil && j.Results != nil {
		printResults(stdout, j.Results)
	}
	return exitOK
}

// printErrSafe prints an error that may quote the agent (e.g. a results
// problem naming one of its files).
func printErrSafe(w io.Writer, prefix string, err error) {
	fmt.Fprint(w, prefix+": ")
	_ = safetext.Write(w, strings.NewReader(err.Error()+"\n"))
}

// printResults summarizes a job's results. Skipped names come from the
// agent, so they are printed safely.
func printResults(w io.Writer, r *runner.Results) {
	fmt.Fprintf(w, "Results: %d files, %d bytes.\n", r.Files, r.Bytes)
	if r.Error != "" {
		fmt.Fprintf(w, "Results problem: ")
		_ = safetext.Write(w, strings.NewReader(r.Error+"\n"))
	}
	if len(r.Skipped) > 0 {
		fmt.Fprintln(w, "Left out (only regular files and directories with safe names are kept):")
		for _, s := range r.Skipped {
			fmt.Fprint(w, "  ")
			_ = safetext.Write(w, strings.NewReader(s+"\n"))
		}
	}
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
	return safetext.Write(w, &buf)
}

func printJobs(w io.Writer, jobs []runner.Job) {
	if len(jobs) == 0 {
		fmt.Fprintln(w, "No jobs.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tHARNESS\tSTATE\tCREATED\tTOKENS\tDETAIL")
	for _, j := range jobs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", j.ID, j.Harness, j.State, j.Created.Local().Format("2006-01-02 15:04:05"), formatUsage(j), safetext.Line(j.Error))
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
