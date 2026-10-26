package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStrictJSON(t *testing.T) {
	for _, ok := range []string{
		`{}`, `[]`, `{"a":1,"b":[1,{"a":2}],"c":{"a":{"a":null}}}`, `"x"`, `[{"a":1},{"a":2}]`, ` {"a" : [ ] } `,
	} {
		if err := StrictJSON([]byte(ok)); err != nil {
			t.Errorf("StrictJSON(%s) = %v", ok, err)
		}
	}
	for _, bad := range []string{
		`{"name":"a","name":"b"}`,
		`{"name":"a","Name":"b"}`,
		`{"params":{"name":"list","NAME":"delete"}}`,
		`{"a":{"x":1,"x":2}}`,
		`[{"k":1,"K":2}]`,
		`{"name":"a","name":"b"}`,
		`{"params":{"name":"delete_note"},"paramſ":{"name":"list_notes"}}`, // ſ folds to s in Go's decoder
		`{"kind":1,"\u212aind":2}`, // the Kelvin sign folds to k
		"\xef\xbb\xbf{}",
		"{\"a\":\"\xff\"}",
		`{} {}`,
		`{"a":1`,
		strings.Repeat("[", 100) + strings.Repeat("]", 100),
	} {
		if err := StrictJSON([]byte(bad)); err == nil {
			t.Errorf("StrictJSON(%q) accepted", bad)
		}
	}
}

// The review's bypass: the gate must not see a tool other than the one a
// server would run.
func TestToolCallsRefuseAmbiguousNames(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_notes","Name":"delete_note"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","Method":"tools/call","params":{"name":"delete_note"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_note","name":"list_notes"}}`,
	} {
		if _, err := ToolCalls([]byte(body)); err == nil {
			t.Errorf("ToolCalls(%s) accepted", body)
		}
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("Parse(%s) accepted", body)
		}
	}
}

func FuzzStrictJSON(f *testing.F) {
	f.Add([]byte(`{"a":1,"b":[{"c":2}]}`))
	f.Add([]byte(`{"a":1,"A":2}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if StrictJSON(b) != nil {
			return
		}
		if !json.Valid(b) {
			t.Fatalf("accepted invalid JSON %q", b)
		}
	})
}
