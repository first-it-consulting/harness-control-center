//go:build !unix

// Command hcc-helper is only supported on Linux.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "hcc-helper: only supported on Linux")
	os.Exit(1)
}
