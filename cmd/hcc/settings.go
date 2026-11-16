package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// restartRunner restarts the runner's service after its settings changed
// (replaceable for tests). It runs as root, from the Control Center's
// sudo over ssh.
var restartRunner = func(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "systemctl", "restart", "hcc-runner").CombinedOutput()
	if err != nil {
		return fmt.Errorf("restarting the runner: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// cmdRunnerSettings shows or changes the settings in runner.yaml that the
// Control Center may change (on a server as root, through sudo over ssh).
func cmdRunnerSettings(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		fmt.Fprintf(stderr, "hcc runner settings: expected get or set (the settings as JSON on stdin)\n\n%s", usage)
		return exitUsage
	}
	sub := args[0]
	fs := flag.NewFlagSet("runner settings "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	config := fs.String("config", "/etc/hcc/runner.yaml", "runner configuration file")
	restart := fs.Bool("restart", false, "restart the runner's service (hcc-runner) to apply the settings")
	restore := fs.Bool("restore", false, "for the installer: put back settings saved before an update, checked only against what runner.yaml allows")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	switch sub {
	case "get":
		cfg, err := runnerd.LoadConfig(*config)
		if err != nil {
			fmt.Fprintln(stderr, "hcc runner settings:", err)
			return exitInvalid
		}
		// The same form "set" reads.
		_ = json.NewEncoder(stdout).Encode(cfg.Settings())
	case "set":
		var s runnerd.Settings
		dec := json.NewDecoder(io.LimitReader(stdin, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			fmt.Fprintln(stderr, "hcc runner settings set: invalid settings on stdin:", err)
			return exitInvalid
		}
		bounds := runnerd.HostBounds()
		if *restore {
			bounds = runnerd.FileBounds()
		}
		if err := runnerd.ApplySettings(*config, s, bounds); err != nil {
			// One line: the Control Center shows it as it comes over ssh.
			fmt.Fprintln(stderr, "hcc runner settings set:", strings.Join(splitErrors(err), "; "))
			return exitInvalid
		}
		if !*restart {
			fmt.Fprintln(stdout, "Saved the runner's settings. They apply when the runner restarts.")
			return exitOK
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := restartRunner(ctx); err != nil {
			fmt.Fprintln(stderr, "hcc runner settings set: saved, but", err)
			return exitInvalid
		}
		fmt.Fprintln(stdout, "Saved the runner's settings and restarted it.")
	}
	return exitOK
}
