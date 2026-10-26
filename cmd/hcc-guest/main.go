// Command hcc-guest runs inside sandboxes (the claude-code image). It
// holds no secrets and has no more rights than the agent or plugin that
// runs it; the runner enforces everything.
//
//	hcc-guest plugin-shim -- COMMAND [ARGS]
//	    runs a stdio MCP server plugin and carries its lines to and from
//	    the runner through the sandbox's proxy (ADR-0014)
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
`

func run(args []string, _ io.Reader, _, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "plugin-shim":
		return pluginShim(args[1:], stderr)
	default:
		fmt.Fprintf(stderr, "hcc-guest: unknown command %q\n%s", args[0], usage)
		return 2
	}
}
