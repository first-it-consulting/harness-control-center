package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The A2A client (ADR-0015): talks to the agents the job's harness lists
// in spec.a2a, through the sandbox's proxy (its allowlist, credential
// broker and approval gates apply). What agents answer is untrusted.

// a2aClient is replaceable in tests. It honours HTTPS_PROXY, and on Linux
// SSL_CERT_FILE (the bundle with the job's CA that init sets).
var a2aClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}, Timeout: 5 * time.Minute}

const maxA2AResponse = 4 << 20

type a2aAgent struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

func a2aAgents() ([]a2aAgent, error) {
	p := os.Getenv("HCC_A2A_CONFIG")
	if p == "" {
		return nil, errors.New("this job may not talk to A2A agents")
	}
	b, err := os.ReadFile(p) //nolint:gosec // written by init
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Agents []a2aAgent `json:"agents"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return cfg.Agents, nil
}

func findAgent(name string) (a2aAgent, error) {
	agents, err := a2aAgents()
	if err != nil {
		return a2aAgent{}, err
	}
	for _, a := range agents {
		if a.Name == name {
			return a, nil
		}
	}
	return a2aAgent{}, fmt.Errorf("no A2A agent %q for this job (see \"hcc-guest a2a list\")", name)
}

const a2aUsage = `usage:
  hcc-guest a2a list                       the A2A agents this job may talk to
  hcc-guest a2a card AGENT                 what the agent says it can do (its agent card)
  hcc-guest a2a send AGENT [--context ID] [--task ID] [--json] < message.txt
                                           send a message, print the answer
  hcc-guest a2a get AGENT TASK [--json]    a task's state and results
`

func cmdA2A(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, a2aUsage)
		return 2
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("a2a "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the raw JSON result")
	contextID := fs.String("context", "", "continue this conversation (contextId)")
	taskID := fs.String("task", "", "continue this task (taskId)")
	// Flags may follow the positional arguments.
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return 2
		}
		args = fs.Args()
		if len(args) > 0 {
			pos, args = append(pos, args[0]), args[1:]
		}
	}
	err := func() error {
		switch sub {
		case "list":
			agents, err := a2aAgents()
			if err != nil {
				return err
			}
			for _, a := range agents {
				fmt.Fprintf(stdout, "%s\t%s\n", a.Name, a.URL)
			}
			return nil
		case "card":
			if len(pos) != 1 {
				return errors.New("expected an agent name")
			}
			return a2aCard(pos[0], stdout)
		case "send":
			if len(pos) != 1 {
				return errors.New("expected an agent name")
			}
			text, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
			if err != nil {
				return err
			}
			if strings.TrimSpace(string(text)) == "" {
				return errors.New("the message (on stdin) is empty")
			}
			msg := map[string]any{"role": "user", "kind": "message", "messageId": randomID(),
				"parts": []map[string]any{{"kind": "text", "text": string(text)}}}
			if *contextID != "" {
				msg["contextId"] = *contextID
			}
			if *taskID != "" {
				msg["taskId"] = *taskID
			}
			return a2aCall(pos[0], "message/send", map[string]any{"message": msg}, *asJSON, stdout)
		case "get":
			if len(pos) != 2 {
				return errors.New("expected an agent name and a task id")
			}
			return a2aCall(pos[0], "tasks/get", map[string]any{"id": pos[1]}, *asJSON, stdout)
		default:
			fmt.Fprint(stderr, a2aUsage)
			return fmt.Errorf("unknown command %q", sub)
		}
	}()
	if err != nil {
		fmt.Fprintln(stderr, "hcc-guest a2a:", err)
		return 1
	}
	return 0
}

// a2aCard prints the agent's card: at /.well-known/agent-card.json, or
// the older /.well-known/agent.json.
func a2aCard(name string, stdout io.Writer) error {
	a, err := findAgent(name)
	if err != nil {
		return err
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return err
	}
	var last error
	for _, p := range []string{"/.well-known/agent-card.json", "/.well-known/agent.json"} {
		card := url.URL{Scheme: u.Scheme, Host: u.Host, Path: p}
		resp, err := a2aClient.Get(card.String())
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxA2AResponse))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusOK {
			var out bytes.Buffer
			if json.Indent(&out, b, "", "  ") != nil {
				return errors.New("the agent card is not JSON")
			}
			_, err = fmt.Fprintln(stdout, out.String())
			return err
		}
		last = fmt.Errorf("%s: %s", card.String(), resp.Status)
	}
	return last
}

// a2aCall makes one JSON-RPC call and prints its result.
func a2aCall(name, method string, params any, asJSON bool, stdout io.Writer) error {
	a, err := findAgent(name)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": randomID(), "method": method, "params": params})
	if err != nil {
		return err
	}
	resp, err := a2aClient.Post(a.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxA2AResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("not a JSON-RPC response: %w", err)
	}
	if r.Error != nil {
		return fmt.Errorf("the agent answered with error %d: %s", r.Error.Code, r.Error.Message)
	}
	if asJSON {
		var out bytes.Buffer
		if err := json.Indent(&out, r.Result, "", "  "); err != nil {
			return err
		}
		_, err := fmt.Fprintln(stdout, out.String())
		return err
	}
	return printResult(r.Result, stdout)
}

type part struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// printResult prints a Task or a Message: its ids, state and text parts.
func printResult(raw json.RawMessage, w io.Writer) error {
	var res struct {
		Kind      string `json:"kind"`
		ID        string `json:"id"`
		ContextID string `json:"contextId"`
		Status    struct {
			State   string `json:"state"`
			Message *struct {
				Parts []part `json:"parts"`
			} `json:"message"`
		} `json:"status"`
		Parts     []part `json:"parts"`
		Artifacts []struct {
			Name  string `json:"name"`
			Parts []part `json:"parts"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	texts := func(ps []part) {
		for _, p := range ps {
			if p.Kind == "text" || p.Text != "" {
				fmt.Fprintln(w, p.Text)
			} else {
				fmt.Fprintf(w, "[a %s part; see --json]\n", p.Kind)
			}
		}
	}
	switch res.Kind {
	case "task":
		fmt.Fprintf(w, "task %s (%s), context %s\n", res.ID, res.Status.State, res.ContextID)
		if res.Status.Message != nil {
			texts(res.Status.Message.Parts)
		}
		for _, a := range res.Artifacts {
			if a.Name != "" {
				fmt.Fprintf(w, "--- %s\n", a.Name)
			}
			texts(a.Parts)
		}
	default:
		if res.ContextID != "" {
			fmt.Fprintf(w, "context %s\n", res.ContextID)
		}
		texts(res.Parts)
	}
	return nil
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
