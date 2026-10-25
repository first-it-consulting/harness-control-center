// Command hcc is the Harness Control Center CLI.
package main

import (
	"errors"
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
	exitInvalid = 1 // the harness is invalid
	exitUsage   = 2 // the command line is wrong
)

const usage = `Usage: hcc <command> [arguments]

Commands:
  validate <file>   check a harness file and report every problem
  explain <file>    describe in plain language what a harness may do
  version           print version information
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "validate", "explain":
		if len(rest) != 1 {
			fmt.Fprintf(stderr, "hcc %s: expected exactly one file\n\n%s", cmd, usage)
			return exitUsage
		}
		e, err := load(rest[0])
		if err != nil {
			fmt.Fprintf(stderr, "%s is not valid:\n", rest[0])
			for _, line := range splitErrors(err) {
				fmt.Fprintf(stderr, "  - %s\n", line)
			}
			return exitInvalid
		}
		if cmd == "validate" {
			fmt.Fprintf(stdout, "%s is valid.\n", rest[0])
			return exitOK
		}
		explain(stdout, e)
		return exitOK
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

func load(path string) (*policy.Engine, error) {
	f, err := os.Open(path) //nolint:gosec // the user chooses which file to read
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Read one byte past the limit so spec.Parse can report oversize input.
	data, err := io.ReadAll(io.LimitReader(f, spec.MaxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	h, err := spec.Parse(data)
	if err != nil {
		return nil, err
	}
	return policy.New(h)
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
