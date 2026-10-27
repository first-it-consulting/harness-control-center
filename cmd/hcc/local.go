package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/first-it-consulting/harness-control-center/internal/control"
	"github.com/first-it-consulting/harness-control-center/internal/local"
	"github.com/first-it-consulting/harness-control-center/internal/version"
)

// cmdLocal handles "hcc local": agents on this Mac (ADR-0016).
func cmdLocal(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "hcc local: expected up, status, down or images\n\n%s", usage)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("local "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "take the release's image files from this directory instead of downloading them")
	dir := fs.String("dir", "", "the Control Center's directory (default: the user's)")
	switch sub {
	case "up", "status", "down", "images":
	default:
		fmt.Fprintf(stderr, "hcc local: unknown command %q\n\n%s", sub, usage)
		return exitUsage
	}
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || (*from != "" && sub != "up") {
		return exitUsage
	}
	if sub == "images" {
		return localImages(stdout, stderr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	l, err := local.DefaultLayout()
	if err == nil {
		var svc local.Service
		if svc, err = local.DefaultService(); err == nil {
			switch sub {
			case "up":
				err = localUp(ctx, l, svc, *from, *dir, stdout)
			case "status":
				localStatus(local.GetStatus(ctx, l, svc), stdout)
			case "down":
				if err = svc.Stop(ctx); err == nil {
					fmt.Fprintln(stdout, "The local runner is stopped and does not start at login. \"hcc local up\" starts it again.")
				}
			}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "hcc local %s: %v\n", sub, err)
		return exitInvalid
	}
	return exitOK
}

func localUp(ctx context.Context, l local.Layout, svc local.Service, from, dir string, stdout io.Writer) error {
	opts := local.Options{Layout: l, Version: version.Version, Service: svc}
	if from != "" {
		opts.Source = &local.Source{Dir: from}
	}
	var shown int64
	opts.Progress = func(s local.Step) {
		switch {
		case s.State == "running" && s.Bytes == 0 && s.Detail == "":
			fmt.Fprintf(stdout, "%s ...\n", s.Title)
			shown = 0
		case s.State == "running" && s.Bytes == 0:
			fmt.Fprintf(stdout, "  %s\n", s.Detail)
			shown = 0
		case s.State == "running":
			if s.Bytes-shown >= 64<<20 {
				fmt.Fprintf(stdout, "  %d MiB\n", s.Bytes>>20)
				shown = s.Bytes
			}
		case s.Detail != "":
			fmt.Fprintf(stdout, "  %s: %s\n", s.State, s.Detail)
		default:
			fmt.Fprintf(stdout, "  %s\n", s.State)
		}
	}
	if err := local.Up(ctx, opts); err != nil {
		return err
	}
	if dir == "" {
		d, err := control.DefaultDir()
		if err != nil {
			return err
		}
		dir = d
	}
	cfgPath, _ := control.ConfigPaths(dir)
	cfg, err := control.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	next, added, err := cfg.WithLocalRunner(l.Socket())
	if err != nil {
		return err
	}
	if added {
		if err := control.SaveConfig(cfgPath, next); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Added the runner %q to the Control Center (restart \"hcc control serve\" if it is running).\n", control.LocalRunnerName)
	}
	fmt.Fprintln(stdout, "Agents can run on this Mac now: harnesses with \"target: local\".")
	return nil
}

func localStatus(st local.Status, stdout io.Writer) {
	if st.Supported != "" {
		fmt.Fprintln(stdout, st.Supported)
		return
	}
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(stdout, "VM images:      %s\n", yes(st.Images))
	if st.ClaudeCode != "" {
		fmt.Fprintf(stdout, "Claude Code:    %s\n", st.ClaudeCode)
	}
	fmt.Fprintf(stdout, "Settings:       %s\n", yes(st.Config))
	fmt.Fprintf(stdout, "Service loaded: %s\n", yes(st.Service))
	fmt.Fprintf(stdout, "Answering:      %s (%s)\n", yes(st.Answering), st.Socket)
	if !st.Ready() {
		fmt.Fprintln(stdout, "Run \"hcc local up\" to set it up.")
	}
}

func localImages(stdout, stderr io.Writer) int {
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
