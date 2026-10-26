package control

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Suggestions for the "Where" field of a runner: the host aliases of the
// user's ssh configuration. They are only suggestions (any ssh host may be
// typed), so a configuration that cannot be read gives none.

// Limits on reading ssh configuration.
const (
	maxSSHConfigBytes = 1 << 20
	maxSSHIncludes    = 8 // depth of Include
	maxSSHHosts       = 256
)

// SSHHosts returns the concrete host aliases (no patterns) of the ssh
// configuration file at path and the files it includes, sorted.
func SSHHosts(path string) []string {
	seen := map[string]bool{}
	readSSHConfig(path, filepath.Dir(path), 0, seen)
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

func readSSHConfig(path, sshDir string, depth int, seen map[string]bool) {
	if depth > maxSSHIncludes {
		return
	}
	f, err := os.Open(path) //nolint:gosec // the user's own ssh configuration
	if err != nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSSHConfigBytes))
	_ = f.Close()
	if err != nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		key, args := sshConfigLine(sc.Text())
		switch key {
		case "host":
			for _, h := range args {
				if len(seen) < maxSSHHosts && sshHostAlias(h) {
					seen[h] = true
				}
			}
		case "include":
			for _, pat := range args {
				if strings.HasPrefix(pat, "~/") {
					pat = filepath.Join(filepath.Dir(sshDir), pat[2:])
				} else if !filepath.IsAbs(pat) {
					pat = filepath.Join(sshDir, pat) // relative to ~/.ssh, as ssh does
				}
				matches, _ := filepath.Glob(pat)
				for _, m := range matches {
					readSSHConfig(m, sshDir, depth+1, seen)
				}
			}
		}
	}
}

// sshConfigLine splits a line into its keyword (lower case) and
// arguments; "Keyword=value" is accepted as ssh does.
func sshConfigLine(line string) (string, []string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", nil
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), nil
	}
	rest := strings.TrimLeft(line[i:], " \t")
	rest = strings.TrimPrefix(rest, "=")
	return strings.ToLower(line[:i]), strings.Fields(rest)
}

// sshHostAlias reports whether h names one host (not a pattern or a
// negation) that is safe to offer as an ssh destination.
func sshHostAlias(h string) bool {
	return h != "" && !strings.ContainsAny(h, "*?!\"'") && !strings.HasPrefix(h, "-") && len(h) <= 253
}
