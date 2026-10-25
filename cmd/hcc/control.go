package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/first-it-consulting/harness-control-center/internal/control"
)

func cmdControl(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintf(stderr, "hcc control: expected serve\n\n%s", usage)
		return exitUsage
	}
	fs := flag.NewFlagSet("control serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:7878", "loopback address and port to serve the web UI on")
	dir := fs.String("dir", "", "directory for control.yaml and saved harnesses (default: <user config dir>/hcc)")
	if _, err := parseFlags(fs, args[1:]); err != nil {
		return exitUsage
	}
	if *dir == "" {
		d, err := control.DefaultDir()
		if err != nil {
			fmt.Fprintln(stderr, "hcc control:", err)
			return exitInvalid
		}
		*dir = d
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintln(stderr, "hcc control:", err)
		return exitInvalid
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	cfgPath, libDir := control.ConfigPaths(*dir)
	srv, err := control.NewServer(cfgPath, libDir, log)
	if err != nil {
		fmt.Fprintln(stderr, "hcc control:", err)
		return exitInvalid
	}
	l, err := control.ListenLoopback(*listen)
	if err != nil {
		fmt.Fprintln(stderr, "hcc control:", err)
		return exitInvalid
	}
	// The link carries the secret that signs a browser in: it is printed
	// here only, never passed on a command line other users could see.
	fmt.Fprintf(stdout, "Harness Control Center is running. Open this link in your browser:\n\n  %s\n\n"+
		"Keep it to yourself: it signs you in. Settings and harnesses are in %s.\nPress Ctrl-C to stop.\n", srv.LoginURL(l), *dir)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := srv.Serve(ctx, l); err != nil {
		fmt.Fprintln(stderr, "hcc control:", err)
		return exitInvalid
	}
	return exitOK
}
