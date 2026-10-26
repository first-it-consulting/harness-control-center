// Command hcc-guest runs inside sandboxes (the claude-code image). It
// holds no secrets and has no more rights than the agent or plugin that
// runs it; the runner enforces everything.
//
//	hcc-guest plugin-shim -- COMMAND [ARGS]
//	    runs a stdio MCP server plugin and carries its lines to and from
//	    the runner through the sandbox's proxy (ADR-0014)
//	hcc-guest a2a list|card|send|get
//	    talks to the A2A agents of the job's harness (ADR-0015)
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = `usage:
  hcc-guest plugin-shim -- COMMAND [ARGS]
  hcc-guest a2a list|card|send|get ...   (hcc-guest a2a for help)
`

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "plugin-shim":
		return pluginShim(args[1:], stderr)
	case "a2a":
		return cmdA2A(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hcc-guest: unknown command %q\n%s", args[0], usage)
		return 2
	}
}
