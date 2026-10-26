package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func mcpExchange(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := s.ServeMCP(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %s", l)
		}
		res = append(res, m)
	}
	return res
}

func TestMCPServerIsReadOnly(t *testing.T) {
	ts := newTestServer(t)
	if _, err := ts.s.lib.Put([]byte(okHarness)); err != nil {
		t.Fatal(err)
	}
	res := mcpExchange(t, ts.s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"explain_harness","arguments":{"name":"hello"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_harnesses"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"start_harness","arguments":{"name":"hello"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"list_jobs","arguments":{"runner":"nope"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_jobs","arguments":{"evil":1}}}`,
		`garbage`,
		`{"jsonrpc":"2.0","id":8,"method":"resources/list"}`,
	)
	if len(res) != 9 { // the notification gets no answer
		t.Fatalf("%d responses: %v", len(res), res)
	}
	init := res[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-03-26" || !strings.Contains(init["instructions"].(string), "cannot start, stop or approve") {
		t.Errorf("initialize = %v", init)
	}
	tools := res[1]["result"].(map[string]any)["tools"].([]any)
	for _, tl := range tools {
		m := tl.(map[string]any)
		name := m["name"].(string)
		if !strings.HasPrefix(name, "list_") && !strings.HasPrefix(name, "get_") && !strings.HasPrefix(name, "explain_") {
			t.Errorf("tool %q is not a read-only one", name)
		}
		if m["annotations"].(map[string]any)["readOnlyHint"] != true {
			t.Errorf("tool %q is not marked read-only", name)
		}
	}
	text := func(r map[string]any) (string, bool) {
		res := r["result"].(map[string]any)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
	}
	if s, isErr := text(res[2]); isErr || !strings.Contains(s, `"risk": "low"`) || !strings.Contains(s, "no network access") {
		t.Errorf("explain = %s", s)
	}
	if s, _ := text(res[3]); !strings.Contains(s, `"name": "hello"`) {
		t.Errorf("harnesses = %s", s)
	}
	if res[4]["error"] == nil {
		t.Error("an unknown (writing) tool was accepted")
	}
	if s, isErr := text(res[5]); !isErr || !strings.Contains(s, "no runner") {
		t.Errorf("unknown runner = %s", s)
	}
	if s, isErr := text(res[6]); !isErr || !strings.Contains(s, "unknown field") {
		t.Errorf("unknown argument = %s", s)
	}
	if res[7]["error"] == nil || res[8]["error"] == nil {
		t.Errorf("garbage / unknown method: %v %v", res[7], res[8])
	}
}
