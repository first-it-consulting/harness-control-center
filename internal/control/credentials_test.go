package control

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCredentialsThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	var logs bytes.Buffer
	ts.s.log = slog.New(slog.NewTextHandler(&logs, nil))
	sock := startRunner(t)
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "mac", Socket: sock}, nil); code != http.StatusCreated {
		t.Fatalf("add runner: %d", code)
	}
	const secret = "sk-test-0123456789abcdef"
	path := "/api/runners/mac/credentials/omlx"
	body := map[string]any{"hosts": []string{"mac.fritz.box"}, "scheme": "http", "port": 4000, "header": "Authorization", "prefix": "Bearer ", "secret": secret}
	rec := ts.rawJSON(http.MethodPut, path, body)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	rec = ts.rawJSON(http.MethodGet, "/api/runners/mac/credentials", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"omlx"`) {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	for _, leak := range []string{secret, "secrets/", "credentials.yaml"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the list shows %q: %s", leak, rec.Body.String())
		}
	}
	// Changing where it goes keeps the secret; a new one needs a secret.
	delete(body, "secret")
	body["hosts"] = []string{"other.fritz.box"}
	if rec := ts.rawJSON(http.MethodPut, path, body); rec.Code != http.StatusOK {
		t.Errorf("move: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ts.rawJSON(http.MethodPut, "/api/runners/mac/credentials/new", body); rec.Code != http.StatusBadRequest {
		t.Errorf("new without a secret: %d", rec.Code)
	}
	// Refused: bad names, unknown fields, unknown runners.
	if rec := ts.rawJSON(http.MethodPut, "/api/runners/mac/credentials/Bad_Name", body); rec.Code != http.StatusBadRequest {
		t.Errorf("bad name: %d", rec.Code)
	}
	if rec := ts.rawJSON(http.MethodPut, path, map[string]any{"secretFile": "/etc/shadow"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := ts.rawJSON(http.MethodPut, "/api/runners/nope/credentials/omlx", body); rec.Code != http.StatusNotFound {
		t.Errorf("unknown runner: %d", rec.Code)
	}
	if rec := ts.rawJSON(http.MethodDelete, path, map[string]any{}); rec.Code != http.StatusOK {
		t.Errorf("DELETE: %d %s", rec.Code, rec.Body.String())
	}
	rec = ts.rawJSON(http.MethodGet, "/api/runners/mac/credentials", nil)
	if strings.Contains(rec.Body.String(), "omlx") {
		t.Errorf("still listed: %s", rec.Body.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Errorf("the secret is in the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "credential saved") {
		t.Errorf("changes are not logged:\n%s", logs.String())
	}
}

// rawJSON sends a JSON API call the way the UI does and returns the
// recorded answer.
func (ts *testServer) rawJSON(method, path string, body any) *httptest.ResponseRecorder {
	ts.t.Helper()
	hdr := map[string]string{"Origin": "http://" + host}
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
		hdr["Content-Type"] = "application/json"
	}
	return ts.raw(method, path, string(b), hdr)
}
