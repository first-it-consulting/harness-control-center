// Command hcc is the Harness Control Center CLI.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
	"github.com/first-it-consulting/harness-control-center/internal/version"
)

// Exit codes.
const (
	exitOK      = 0
	exitInvalid = 1 // invalid input or a failed operation
	exitUsage   = 2 // the command line is wrong
	exitJob     = 3 // the job ran but did not succeed
)

const usage = `Usage: hcc <command> [arguments]

Harness files:
  validate <file>          check a harness file and report every problem
  explain <file>           describe in plain language what a harness may do

Jobs (on a local runner, or a remote one with --ssh HOST):
  run <file>               show what the harness may do, confirm, run it and follow it
  jobs                     list jobs
  logs <job>               print a job's console output
  cancel <job>             stop one job
  kill                     kill switch: stop all jobs and refuse new ones
  resume                   accept jobs again after kill

Runner:
  runner serve --config <file>   run the runner daemon
  runner stdio [--socket path]   relay stdin/stdout to the daemon (used over ssh)

  version                  print version information

Run "hcc <command> -h" for a command's flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "validate", "explain":
		return cmdCheck(cmd, rest, stdout, stderr)
	case "run":
		return cmdRun(rest, stdin, stdout, stderr)
	case "jobs", "logs", "cancel", "kill", "resume":
		return cmdJobs(cmd, rest, stdout, stderr)
	case "runner":
		return cmdRunner(rest, stdin, stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "hcc", version.String())
		return exitOK
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "hcc: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
}

func cmdCheck(cmd string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "hcc %s: expected exactly one file\n\n%s", cmd, usage)
		return exitUsage
	}
	_, e, err := load(args[0])
	if err != nil {
		printInvalid(stderr, args[0], err)
		return exitInvalid
	}
	if cmd == "validate" {
		fmt.Fprintf(stdout, "%s is valid.\n", args[0])
		return exitOK
	}
	explain(stdout, e)
	return exitOK
}

// load reads and parses a harness file, returning its raw bytes too
// (they are what gets submitted, so the runner parses the same document).
func load(path string) ([]byte, *policy.Engine, error) {
	f, err := os.Open(path) //nolint:gosec // the user chooses which file to read
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	// Read one byte past the limit so spec.Parse can report oversize input.
	data, err := io.ReadAll(io.LimitReader(f, spec.MaxDocumentBytes+1))
	if err != nil {
		return nil, nil, err
	}
	h, err := spec.Parse(data)
	if err != nil {
		return nil, nil, err
	}
	e, err := policy.New(h)
	return data, e, err
}

func printInvalid(w io.Writer, path string, err error) {
	fmt.Fprintf(w, "%s is not valid:\n", path)
	for _, line := range splitErrors(err) {
		fmt.Fprintf(w, "  - %s\n", line)
	}
}

func explain(w io.Writer, e *policy.Engine) {
	risk, why := e.Risk()
	fmt.Fprintf(w, "Risk: %s\n", risk)
	for _, r := range why {
		fmt.Fprintf(w, "  because it %s\n", r)
	}
	fmt.Fprintln(w)
	for _, line := range e.Explain() {
		fmt.Fprintf(w, "• %s\n", line)
	}
}

// splitErrors flattens errors.Join trees into one message per line.
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

// parseFlags parses flags anywhere among args (Go's flag package stops at
// the first positional argument) and returns the positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
