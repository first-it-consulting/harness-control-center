// Package local runs agents on the user's own Mac (ADR-0016): it knows
// the VM images a release publishes and, from them, sets up the local
// runner.
package local

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// releaseImages lists this release's image files and their SHA-256
// checksums ("file=hex,..."). The release pipeline sets it when it builds
// hcc for Apple Silicon (.goreleaser.yaml), from the images it built in
// the same run, so a release verifies its downloads against what it
// carries itself. Development builds carry none.
var releaseImages string

// File is one published image file.
type File struct {
	Name   string
	SHA256 string
}

var (
	fileNameRe = regexp.MustCompile(`^vz-[a-z0-9][a-z0-9.-]{0,95}$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ErrNoImages means this build carries no image checksums: a development
// build, whose images are built locally (hack/vz/build-images.sh).
var ErrNoImages = errors.New("this build of hcc carries no VM image checksums (a development build)")

// ReleaseImages returns the image files this build of hcc may download.
func ReleaseImages() ([]File, error) { return parseImages(releaseImages) }

func parseImages(s string) ([]File, error) {
	if s == "" {
		return nil, ErrNoImages
	}
	var files []File
	seen := map[string]bool{}
	for e := range strings.SplitSeq(s, ",") {
		name, sum, ok := strings.Cut(e, "=")
		if !ok || !fileNameRe.MatchString(name) || !sha256Re.MatchString(sum) || seen[name] {
			return nil, fmt.Errorf("local: invalid image list entry %q", e)
		}
		seen[name] = true
		files = append(files, File{Name: name, SHA256: sum})
	}
	return files, nil
}
