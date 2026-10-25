// Package version holds build information injected via -ldflags.
package version

// Set at build time by GoReleaser, e.g.
// -X github.com/first-it-consulting/harness-control-center/internal/version.Version=v0.1.0
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable build description.
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
