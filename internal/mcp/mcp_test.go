package mcp

import (
	"strings"
	"testing"
)

func TestToolCalls(t *testing.T) {
	calls, err := ToolCalls([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_issue","arguments":{"title": "x",  "n": 1}}}`))
	if err != nil || len(calls) != 1 || calls[0].Tool != "create_issue" || calls[0].Arguments != `{"title":"x","n":1}` {
		t.Fatalf("calls = %+v, %v", calls, err)
	}
	calls, err = ToolCalls([]byte(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"a"}},{"jsonrpc":"2.0","method":"notifications/initialized"}]`))
	if err != nil || len(calls) != 1 || calls[0].Tool != "a" || calls[0].Arguments != "{}" {
		t.Fatalf("batch: %+v, %v", calls, err)
	}
	for _, bad := range []string{
		`not json`,
		`{"method":"tools/call","params":{}}`,
		`{"method":"tools/call","params":{"name":""}}`,
		`{"method":"tools/call","params":{"name":"` + strings.Repeat("x", 200) + `"}}`,
		`[{"method":"tools/call","params":{"name":"a"}}, 5]`,
		`{"method":"tools/call","params":{"name":"a","arguments":{"x":}}}`,
	} {
		if _, err := ToolCalls([]byte(bad)); err == nil {
			t.Errorf("ToolCalls(%.40q) accepted", bad)
		}
	}
	if _, err := DescribeToolCall(ToolCall{Tool: "a", Arguments: strings.Repeat("x", MaxDetails)}); err == nil {
		t.Error("arguments too long to show were accepted")
	}
}

func FuzzToolCalls(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"x","arguments":{}}}`))
	f.Add([]byte(`[{"method":"tools/call","params":{"name":"y"}}]`))
	f.Fuzz(func(t *testing.T, b []byte) {
		calls, err := ToolCalls(b)
		if err != nil {
			return
		}
		for _, c := range calls {
			if c.Tool == "" || len(c.Tool) > MaxToolName {
				t.Fatalf("accepted tool name %q", c.Tool)
			}
		}
	})
}

func TestParse(t *testing.T) {
	for in, want := range map[string]Message{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`:         {ID: "1", Method: "tools/list"},
		`{"jsonrpc":"2.0","id":"a b","method":"x"}`:              {ID: `"a b"`, Method: "x"},
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`: {Method: "notifications/initialized"},
		`{"jsonrpc":"2.0","id":7,"result":{}}`:                   {ID: "7"},
	} {
		got, err := Parse([]byte(in))
		if err != nil || got != want {
			t.Errorf("Parse(%s) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{`[]`, `{"id":1,"method":"x"}`, `{"jsonrpc":"2.0"}`, `{"jsonrpc":"2.0","id":null}`, `x`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("Parse(%s) accepted", bad)
		}
	}
	r := ErrorResponse(`"q"`, CodeNotApproved, `not "approved"`)
	if m, err := Parse(r); err != nil || !m.IsResponse() || m.ID != `"q"` {
		t.Errorf("ErrorResponse = %s (%+v, %v)", r, m, err)
	}
}
