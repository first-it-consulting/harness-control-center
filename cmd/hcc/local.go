package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/first-it-consulting/harness-control-center/internal/local"
)

// cmdLocal handles "hcc local": agents on this Mac (ADR-0016).
func cmdLocal(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "images" {
		fmt.Fprintf(stderr, "hcc local: expected \"images\"\n\n%s", usage)
		return exitUsage
	}
	files, err := local.ReleaseImages()
	if errors.Is(err, local.ErrNoImages) {
		fmt.Fprintln(stderr, "hcc local images:", err)
		fmt.Fprintln(stderr, "Build the images on this Mac with hack/vz/build-images.sh instead.")
		return exitInvalid
	}
	if err != nil {
		fmt.Fprintln(stderr, "hcc local images:", err)
		return exitInvalid
	}
	for _, f := range files {
		fmt.Fprintf(stdout, "%s  %s\n", f.SHA256, f.Name)
	}
	return exitOK
}
