package local

import (
	"errors"
	"strings"
	"testing"
)

func TestParseImages(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	files, err := parseImages("vz-kernel-arm64.gz=" + sum + ",vz-base-arm64.ext4.gz=" + sum)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "vz-kernel-arm64.gz" || files[1].SHA256 != sum {
		t.Fatalf("got %+v", files)
	}
}

func TestParseImagesNone(t *testing.T) {
	if _, err := parseImages(""); !errors.Is(err, ErrNoImages) {
		t.Fatalf("got %v, want ErrNoImages", err)
	}
}

func TestParseImagesRejects(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for _, s := range []string{
		"vz-a.gz",                            // no checksum
		"vz-a.gz=" + sum[:63],                // short checksum
		"vz-a.gz=" + strings.ToUpper(sum),    // not lowercase hex
		"../vz-a.gz=" + sum,                  // a path
		"vz-a/b.gz=" + sum,                   // a path
		"kernel.gz=" + sum,                   // not a vz file
		"vz-a.gz=" + sum + ",vz-a.gz=" + sum, // twice
		"vz-a.gz=" + sum + ",",               // empty entry
	} {
		if _, err := parseImages(s); err == nil {
			t.Errorf("%q: accepted", s)
		}
	}
}
