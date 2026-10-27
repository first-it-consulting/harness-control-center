package local

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// ServiceLabel names the local runner's launchd agent.
const ServiceLabel = "io.github.first-it-consulting.hcc.runner"

// Service is the local runner as a launchd agent of the user's: it starts
// at login and again if it crashes, as the user, never as root.
type Service struct {
	// Plist is ~/Library/LaunchAgents/<ServiceLabel>.plist.
	Plist string
	// Launchctl runs launchctl (replaceable for tests).
	Launchctl func(ctx context.Context, args ...string) error
}

// DefaultService is the user's own agent.
func DefaultService() (Service, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Service{}, err
	}
	return Service{Plist: filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist"), Launchctl: launchctl}, nil
}

func launchctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput() //nolint:gosec // a fixed program; its arguments are ours, no shell
	if err != nil {
		return fmt.Errorf("launchctl %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return nil
}

func domain() string        { return "gui/" + strconv.Itoa(os.Getuid()) }
func serviceTarget() string { return domain() + "/" + ServiceLabel }

// plist is the agent's definition: hcc runner serve with the layout's
// config, restarted unless it exits cleanly, its output in the log.
func plist(exe string, l Layout) []byte {
	var b bytes.Buffer
	str := func(indent, s string) {
		b.WriteString(indent + "<string>")
		_ = xml.EscapeText(&b, []byte(s))
		b.WriteString("</string>\n")
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- The hcc runner for agents on this Mac. Written by "hcc local up"; "hcc local down" stops it. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + ServiceLabel + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range []string{exe, "runner", "serve", "--config", l.Config()} {
		str("\t\t", a)
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>StandardOutPath</key>
`)
	str("\t", l.Log())
	b.WriteString("\t<key>StandardErrorPath</key>\n")
	str("\t", l.Log())
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// Install writes the agent's definition and (re)starts it if the
// definition changed or restart is set. It reports whether it started
// the runner.
func (s Service) Install(ctx context.Context, exe string, l Layout, restart bool) (bool, error) {
	want := plist(exe, l)
	have, err := os.ReadFile(s.Plist)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if bytes.Equal(have, want) && !restart && s.Running(ctx) {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.Plist), 0o755); err != nil { //nolint:gosec // launchd's own directory layout
		return false, err
	}
	// launchd refuses definitions others can write.
	if err := writeFileAtomic(s.Plist, want, 0o644); err != nil {
		return false, err
	}
	_ = s.Launchctl(ctx, "bootout", serviceTarget()) // not loaded is fine
	if err := s.Launchctl(ctx, "bootstrap", domain(), s.Plist); err != nil {
		return false, err
	}
	return true, nil
}

// Running reports whether launchd has the agent loaded.
func (s Service) Running(ctx context.Context) bool {
	return s.Launchctl(ctx, "print", serviceTarget()) == nil
}

// Stop unloads the agent and removes its definition, so it does not come
// back at the next login; "hcc local up" starts it again.
func (s Service) Stop(ctx context.Context) error {
	err := s.Launchctl(ctx, "bootout", serviceTarget())
	if rerr := os.Remove(s.Plist); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return errors.Join(err, rerr)
	}
	if err != nil && s.Running(ctx) {
		return err
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Chmod(mode), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
