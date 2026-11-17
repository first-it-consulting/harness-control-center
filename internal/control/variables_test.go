package control

import (
	"net/http"
	"strings"
	"testing"
)

func TestVariablesThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	var out struct{ Variables []Variable }
	if code := ts.do(http.MethodGet, "/api/variables", nil, &out); code != http.StatusOK || out.Variables == nil || len(out.Variables) != 0 {
		t.Fatalf("empty: %d %+v", code, out)
	}
	for _, v := range []struct{ name, value string }{{"REGION", "eu-central"}, {"DB_HOST", "db.lan"}, {"REGION", "eu-west"}} {
		if code := ts.do(http.MethodPut, "/api/variables/"+v.name, map[string]string{"value": v.value}, nil); code != http.StatusOK {
			t.Fatalf("PUT %s: %d", v.name, code)
		}
	}
	ts.do(http.MethodGet, "/api/variables", nil, &out)
	if len(out.Variables) != 2 || out.Variables[0].Name != "DB_HOST" || out.Variables[1].Value != "eu-west" || out.Variables[1].Updated.IsZero() {
		t.Errorf("list = %+v", out.Variables)
	}
	for name, body := range map[string]string{"PATH": "x", "1A": "x", "LD_PRELOAD": "x", "OK": strings.Repeat("x", 5000)} {
		if code := ts.do(http.MethodPut, "/api/variables/"+name, map[string]string{"value": body}, nil); code != http.StatusBadRequest {
			t.Errorf("%s accepted: %d", name, code)
		}
	}
	if code := ts.do(http.MethodDelete, "/api/variables/REGION", nil, nil); code != http.StatusOK {
		t.Errorf("DELETE: %d", code)
	}
	if code := ts.do(http.MethodDelete, "/api/variables/REGION", nil, nil); code != http.StatusNotFound {
		t.Errorf("DELETE again: %d", code)
	}
}
