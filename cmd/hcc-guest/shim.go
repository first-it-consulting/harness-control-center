package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// maxLine bounds one JSON-RPC message from the server.
const maxLine = 4 << 20

// pluginShim runs the plugin's MCP server with its stdin and stdout
// connected to the runner: it polls the sandbox's proxy for lines to
// write and posts every line the server prints. It exits with the
// server's status.
func pluginShim(args []string, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	api := os.Getenv("HCC_PLUGIN_API")
	if len(args) == 0 || api == "" {
		fmt.Fprintln(stderr, "hcc-guest plugin-shim: HCC_PLUGIN_API and a command are required")
		return 2
	}
	base := "http://" + api + "/hcc/v1/plugin"
	// Straight to the proxy's own address: never through a proxy.
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 60 * time.Second}

	cmd := exec.Command(args[0], args[1:]...) //nolint:gosec // the plugin's own command, from its manifest
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintln(stderr, "hcc-guest plugin-shim:", err)
		return 1
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(stderr, "hcc-guest plugin-shim:", err)
		return 1
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "hcc-guest plugin-shim:", err)
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Go(func() { relayIn(ctx, client, base, in, stderr) })
	wg.Go(func() { relayOut(client, base, out, stderr) })
	err = cmd.Wait()
	cancel()
	_ = in.Close()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return ee.ExitCode()
	case err != nil:
		fmt.Fprintln(stderr, "hcc-guest plugin-shim:", err)
		return 1
	}
	return 0
}

// relayIn writes what the runner sends to the server's stdin.
func relayIn(ctx context.Context, client *http.Client, base string, in io.WriteCloser, stderr io.Writer) {
	defer in.Close()
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/in", nil) //nolint:gosec // the sandbox's own proxy, set by init
		if err != nil {
			return
		}
		resp, err := client.Do(req) //nolint:gosec // as above
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintln(stderr, "hcc-guest plugin-shim: poll:", err)
				time.Sleep(time.Second)
			}
			continue
		}
		switch resp.StatusCode {
		case http.StatusOK:
			_, err = io.Copy(in, resp.Body)
		case http.StatusNoContent:
		case http.StatusGone:
			_ = resp.Body.Close()
			return
		default:
			time.Sleep(time.Second)
		}
		_ = resp.Body.Close()
		if err != nil {
			return // the server closed its stdin
		}
	}
}

// relayOut posts every line the server prints.
func relayOut(client *http.Client, base string, out io.Reader, stderr io.Writer) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := append(bytes.Clone(sc.Bytes()), '\n')
		for attempt := 0; attempt < 3; attempt++ {
			resp, err := client.Post(base+"/out", "application/x-ndjson", bytes.NewReader(line)) //nolint:gosec // the sandbox's own proxy, set by init
			if err == nil {
				_ = resp.Body.Close()
				break
			}
			fmt.Fprintln(stderr, "hcc-guest plugin-shim: post:", err)
			time.Sleep(time.Second)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(stderr, "hcc-guest plugin-shim:", err)
	}
}
