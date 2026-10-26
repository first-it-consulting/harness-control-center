package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func a2aServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/agent-card.json":
			_, _ = io.WriteString(w, `{"name":"reviewer","skills":[{"id":"review"}]}`)
		case r.URL.Path == "/a2a" && r.Method == http.MethodPost:
			var req struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
				Params struct {
					Message struct {
						Parts []struct {
							Text string `json:"text"`
						} `json:"parts"`
						ContextID string `json:"contextId"`
					} `json:"message"`
					ID string `json:"id"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			switch req.Method {
			case "message/send":
				text := req.Params.Message.Parts[0].Text
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":{"kind":"task","id":"t1","contextId":"c1",`+
					`"status":{"state":"completed"},"artifacts":[{"name":"review","parts":[{"kind":"text","text":"looks fine: `+strings.TrimSpace(text)+`"}]}]}}`)
			case "tasks/get":
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(req.ID)+`,"error":{"code":-32001,"message":"task not found"}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := a2aClient
	a2aClient = srv.Client()
	t.Cleanup(func() { a2aClient = old })
	cfg := filepath.Join(t.TempDir(), "a2a.json")
	if err := os.WriteFile(cfg, []byte(`{"agents":[{"name":"reviewer","url":"`+srv.URL+`/a2a"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HCC_A2A_CONFIG", cfg)
	return srv
}

func TestA2AClient(t *testing.T) {
	a2aServer(t)
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errOut strings.Builder
		code := cmdA2A(args, strings.NewReader(stdin), &out, &errOut)
		return code, out.String(), errOut.String()
	}
	if code, out, _ := run("", "list"); code != 0 || !strings.HasPrefix(out, "reviewer\thttps://") {
		t.Errorf("list = %d %q", code, out)
	}
	if code, out, _ := run("", "card", "reviewer"); code != 0 || !strings.Contains(out, `"skills"`) {
		t.Errorf("card = %d %q", code, out)
	}
	code, out, errOut := run("my diff\n", "send", "reviewer", "--context", "c0")
	if code != 0 || !strings.Contains(out, "task t1 (completed), context c1") || !strings.Contains(out, "looks fine: my diff") {
		t.Errorf("send = %d %q %q", code, out, errOut)
	}
	if code, _, errOut := run("", "get", "reviewer", "t9"); code != 1 || !strings.Contains(errOut, "task not found") {
		t.Errorf("get = %d %q", code, errOut)
	}
	if code, _, errOut := run("x", "send", "stranger"); code != 1 || !strings.Contains(errOut, `no A2A agent "stranger"`) {
		t.Errorf("unknown agent = %d %q", code, errOut)
	}
	if code, _, _ := run("  \n", "send", "reviewer"); code != 1 {
		t.Error("an empty message was sent")
	}
	t.Setenv("HCC_A2A_CONFIG", "")
	if code, _, errOut := run("", "list"); code != 1 || !strings.Contains(errOut, "may not talk") {
		t.Errorf("without config = %d %q", code, errOut)
	}
}
