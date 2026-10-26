package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/first-it-consulting/harness-control-center/internal/plugins"
	"github.com/first-it-consulting/harness-control-center/internal/safetext"
)

// cmdPlugins: "plugins" lists them; "plugin install|remove|logs" manage
// one (ADR-0013).
func cmdPlugins(cmd string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	sub := "list"
	if cmd == "plugin" {
		if len(args) == 0 {
			fmt.Fprintf(stderr, "hcc plugin: expected install, remove or logs\n\n%s", usage)
			return exitUsage
		}
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("plugin "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var conn connFlags
	conn.register(fs)
	yes := fs.Bool("yes", false, "install without asking for confirmation")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage
	}
	want := 1
	if sub == "list" {
		want = 0
	}
	if len(pos) != want {
		fmt.Fprintf(stderr, "hcc %s %s: wrong number of arguments\n\n%s", cmd, sub, usage)
		return exitUsage
	}
	c, err := conn.client()
	if err != nil {
		fmt.Fprintf(stderr, "hcc %s: %v\n", cmd, err)
		return exitUsage
	}
	ctx := context.Background()
	switch sub {
	case "list":
		list, err := c.Plugins(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "hcc plugins:", err)
			return exitInvalid
		}
		if len(list) == 0 {
			fmt.Fprintln(stdout, "No plugins.")
			return exitOK
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PLUGIN\tVERSION\tTYPE\tRISK\tLAST RUN\tRESULT")
		for _, p := range list {
			last := "-"
			if !p.Status.LastRun.IsZero() {
				last = p.Status.LastRun.Local().Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, p.Version, p.Type, p.Risk, last, safetext.Line(p.Status.LastResult))
		}
		_ = tw.Flush()
	case "install":
		bundle, err := plugins.Pack(pos[0])
		if err != nil {
			fmt.Fprintln(stderr, "hcc plugin install:", err)
			return exitInvalid
		}
		pv, err := c.PreviewPlugin(ctx, bundle)
		if err != nil {
			fmt.Fprintln(stderr, "hcc plugin install:", err)
			return exitInvalid
		}
		printPreview(stdout, pv)
		if !*yes {
			ok, err := confirm(stdin, stdout, fmt.Sprintf("Install this plugin on %s?", conn.where()))
			if err != nil {
				fmt.Fprintln(stderr, "hcc plugin install:", err)
				return exitUsage
			}
			if !ok {
				fmt.Fprintln(stdout, "Not installed.")
				return exitOK
			}
		}
		in, err := c.InstallPlugin(ctx, bundle, pv.SHA256)
		if err != nil {
			fmt.Fprintln(stderr, "hcc plugin install:", err)
			return exitInvalid
		}
		fmt.Fprintf(stdout, "Installed %s %s.\n", in.Name, in.Version)
	case "remove":
		if err := c.RemovePlugin(ctx, pos[0]); err != nil {
			fmt.Fprintln(stderr, "hcc plugin remove:", err)
			return exitInvalid
		}
		fmt.Fprintf(stdout, "Removed %s.\n", pos[0])
	case "logs":
		var log bytes.Buffer
		if err := c.PluginLog(ctx, pos[0], &log); err != nil {
			fmt.Fprintln(stderr, "hcc plugin logs:", err)
			return exitInvalid
		}
		// Written by the plugin: shown inert.
		if err := safetext.Write(stdout, &log); err != nil {
			return exitInvalid
		}
	default:
		fmt.Fprintf(stderr, "hcc plugin: unknown command %q\n", sub)
		return exitUsage
	}
	return exitOK
}

// printPreview shows what installing a plugin means. Its text comes from
// the plugin's manifest, so it is shown inert.
func printPreview(w io.Writer, pv plugins.Preview) {
	fmt.Fprintf(w, "Plugin %s %s (%s), %d files, %d bytes\n", safetext.Line(pv.Name), safetext.Line(pv.Version), pv.Type, pv.Files, pv.Bytes)
	if pv.Description != "" {
		fmt.Fprintf(w, "  %s\n", safetext.Line(pv.Description))
	}
	fmt.Fprintf(w, "Checksum (SHA-256): %s\n", pv.SHA256)
	fmt.Fprintln(w, "Not signed: install it only if you trust its author and have read its files.")
	fmt.Fprintf(w, "\nRisk: %s\n", pv.Risk)
	for _, why := range pv.Why {
		fmt.Fprintf(w, "  because it %s\n", why)
	}
	fmt.Fprintln(w)
	for _, l := range pv.Explanation {
		fmt.Fprintf(w, "• %s\n", safetext.Line(l))
	}
	fmt.Fprintln(w)
}
