package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox/firecracker"
	"github.com/first-it-consulting/harness-control-center/internal/version"
)

func cmdRunner(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "hcc runner: expected serve or stdio\n\n%s", usage)
		return exitUsage
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:], stderr)
	case "stdio":
		return cmdStdio(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hcc runner: unknown command %q\n", args[0])
		return exitUsage
	}
}

func cmdServe(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("runner serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "/etc/hcc/runner.yaml", "runner configuration file")
	if _, err := parseFlags(fs, args); err != nil {
		return exitUsage
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := serve(*path, log); err != nil {
		log.Error("runner stopped", "err", err)
		return exitInvalid
	}
	return exitOK
}

func serve(configPath string, log *slog.Logger) error {
	// The runner itself never needs root; only the jailer does (via the
	// sandbox Host). Refusing root keeps that boundary real.
	if os.Geteuid() == 0 {
		return errors.New("refusing to run as root: run the runner as an unprivileged user")
	}
	cfg, err := runnerd.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if err := cfg.CheckFiles(); err != nil {
		return err
	}
	for _, d := range []string{cfg.StateDir, cfg.Firecracker.ChrootBase} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	fc := cfg.Firecracker
	provider, err := firecracker.New(firecracker.Config{
		JailerPath: fc.Jailer, FirecrackerPath: fc.Firecracker,
		ChrootBase: fc.ChrootBase, ImageRoot: fc.ImageRoot,
		UID: fc.UID, GID: fc.GID,
		Host: firecracker.NewSudoHost(fc.ChrootBase),
	})
	if err != nil {
		return err
	}
	images := make(map[string]runner.Image, len(cfg.Images))
	for name, img := range cfg.Images {
		images[name] = runner.Image{Kernel: img.Kernel, RootFS: img.RootFS}
	}
	r, err := runner.New(runner.Config{
		Provider:      provider,
		Target:        cfg.Target,
		Images:        images,
		MaxConcurrent: cfg.MaxConcurrent,
		Console:       runnerd.ConsoleFunc(cfg.StateDir, log),
	})
	if err != nil {
		return err
	}

	// Every state change goes to the log, for journald and auditing.
	events, stop := r.Subscribe(1024)
	defer stop()
	go func() {
		for e := range events {
			log.Info("job", "job", e.JobID, "harness", e.Harness, "from", e.From, "to", e.To, "message", e.Message)
		}
	}()

	l, err := runnerd.Listen(cfg.Socket)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	log.Info("runner started", "version", version.Version, "target", cfg.Target, "socket", cfg.Socket, "maxConcurrent", cfg.MaxConcurrent)
	err = runnerd.NewServer(r, cfg.StateDir, log).Serve(ctx, l)
	log.Info("runner stopped; all jobs were stopped")
	return err
}

func cmdStdio(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("runner stdio", flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("socket", runnerd.DefaultSocket, "runner socket")
	if _, err := parseFlags(fs, args); err != nil {
		return exitUsage
	}
	if err := runnerd.Relay(context.Background(), *socket, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "hcc runner stdio:", err)
		return exitInvalid
	}
	return exitOK
}
