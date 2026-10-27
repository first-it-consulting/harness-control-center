package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// credentialRequest is what "hcc credentials set" reads on stdin: the
// secret travels there, never on the command line (where ps shows it).
type credentialRequest struct {
	credentials.Entry
	// Secret, if set, replaces the credential's secret.
	Secret *string `json:"secret,omitempty"`
}

// cmdCredentials edits a runner's credential store where the runner runs
// (ADR-0017): on a server as root (the Control Center runs it with sudo
// over ssh), on a Mac as the user.
func cmdCredentials(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "hcc credentials: expected list, set or remove\n\n%s", usage)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("credentials "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	config := fs.String("config", "/etc/hcc/runner.yaml", "the runner's configuration, which names the credential store")
	asJSON := fs.Bool("json", false, "list as JSON")
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	nargs := map[string]int{"list": 0, "set": 0, "remove": 1}
	n, ok := nargs[sub]
	if !ok || fs.NArg() != n {
		fmt.Fprintf(stderr, "hcc credentials: expected list, set (the credential as JSON on stdin) or remove NAME\n\n%s", usage)
		return exitUsage
	}
	store, err := credentialStore(*config)
	if err != nil {
		fmt.Fprintln(stderr, "hcc credentials:", err)
		return exitInvalid
	}
	secrets := filepath.Join(filepath.Dir(store), "secrets")
	switch sub {
	case "list":
		list, err := credentials.List(store)
		if err != nil {
			fmt.Fprintln(stderr, "hcc credentials:", err)
			return exitInvalid
		}
		if *asJSON {
			_ = json.NewEncoder(stdout).Encode(list)
			return exitOK
		}
		for _, e := range list {
			fmt.Fprintf(stdout, "%s: %s for %s://%v:%d\n", e.Name, e.Header, e.Scheme, e.Hosts, e.Port)
		}
	case "set":
		var req credentialRequest
		dec := json.NewDecoder(io.LimitReader(stdin, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			fmt.Fprintln(stderr, "hcc credentials set: invalid request on stdin:", err)
			return exitInvalid
		}
		var secret []byte
		if req.Secret != nil {
			secret = []byte(*req.Secret)
		}
		if err := credentials.Set(store, secrets, req.Entry, secret); err != nil {
			fmt.Fprintln(stderr, "hcc credentials set:", err)
			return exitInvalid
		}
		fmt.Fprintf(stdout, "Saved credential %s. Jobs that start from now on use it.\n", req.Name)
	case "remove":
		if err := credentials.Remove(store, secrets, fs.Arg(0)); err != nil {
			fmt.Fprintln(stderr, "hcc credentials remove:", err)
			return exitInvalid
		}
		fmt.Fprintf(stdout, "Removed credential %s.\n", fs.Arg(0))
	}
	return exitOK
}

// credentialStore is the store the runner's configuration names.
func credentialStore(config string) (string, error) {
	cfg, err := runnerd.LoadConfig(config)
	if err != nil {
		return "", err
	}
	if cfg.Credentials == "" {
		return "", errors.New(config + " names no credential store (credentials:)")
	}
	return cfg.Credentials, nil
}
