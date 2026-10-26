package control

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runner"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
	"github.com/first-it-consulting/harness-control-center/internal/sandbox"
)

const host = "127.0.0.1:7878"

const okHarness = `apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: hello, description: says hello}
spec:
  runtime: claude-code
  prompt: hi
`

// testServer is a Control Center with a signed-in client.
type testServer struct {
	t       *testing.T
	s       *Server
	dir     string
	session string
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	dir := t.TempDir()
	cfg, lib := ConfigPaths(dir)
	s, err := NewServer(cfg, lib, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s.auth.setHosts([]string{host, "localhost:7878"})
	ts := &testServer{t: t, s: s, dir: dir}
	rec := ts.raw(http.MethodGet, "/login?token="+s.auth.loginToken, "", nil)
	loc := rec.Header().Get("Location")
	session, ok := strings.CutPrefix(loc, "/#session=")
	if rec.Code != http.StatusSeeOther || !ok || len(session) != 64 {
		t.Fatalf("login: %d %q", rec.Code, loc)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("login set a cookie: cookies reach every port of 127.0.0.1")
	}
	ts.session = session
	return ts
}

func (ts *testServer) raw(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if _, set := hdr[sessionHeader]; !set && ts.session != "" {
		req.Header.Set(sessionHeader, ts.session)
	}
	rec := httptest.NewRecorder()
	ts.s.ServeHTTP(rec, req)
	return rec
}

// do sends an API call the way the UI does and decodes the answer.
func (ts *testServer) do(method, path string, body, out any) int {
	ts.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	hdr := map[string]string{"Origin": "http://" + host}
	if body != nil {
		hdr["Content-Type"] = "application/json"
	}
	rec := ts.raw(method, path, string(b), hdr)
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			ts.t.Fatalf("%s %s: %d %q: %v", method, path, rec.Code, rec.Body.String(), err)
		}
	}
	return rec.Code
}

func TestAuth(t *testing.T) {
	ts := newTestServer(t)
	anon := &testServer{t: t, s: ts.s}

	if rec := anon.raw(http.MethodGet, "/api/jobs", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d", rec.Code)
	}
	if rec := anon.raw(http.MethodGet, "/login?token=wrong", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("wrong login token: %d", rec.Code)
	}
	// DNS rebinding: a page on evil.example resolving to 127.0.0.1.
	req := httptest.NewRequest(http.MethodGet, "http://evil.example:7878/api/jobs", nil)
	req.Header.Set(sessionHeader, ts.session)
	rec := httptest.NewRecorder()
	ts.s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("foreign Host: %d", rec.Code)
	}
	body := `{"yaml":"x"}`
	for name, tc := range map[string]struct {
		hdr  map[string]string
		code int
	}{
		"no session":    {map[string]string{"Content-Type": "application/json", "Origin": "http://" + host, sessionHeader: ""}, http.StatusUnauthorized},
		"wrong session": {map[string]string{"Content-Type": "application/json", "Origin": "http://" + host, sessionHeader: "x"}, http.StatusUnauthorized},
		"no origin":     {map[string]string{"Content-Type": "application/json"}, http.StatusForbidden},
		"cross origin":  {map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}, http.StatusForbidden},
	} {
		if rec := ts.raw(http.MethodPost, "/api/check", body, tc.hdr); rec.Code != tc.code {
			t.Errorf("%s: %d, want %d", name, rec.Code, tc.code)
		}
	}
	// A form post (what another site could make the browser send) is
	// not JSON.
	if rec := ts.raw(http.MethodPost, "/api/check", "yaml=x", map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://" + host,
	}); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form post: %d", rec.Code)
	}
	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/logout", map[string]string{}, &out); code != http.StatusOK {
		t.Fatalf("logout: %d", code)
	}
	if rec := ts.raw(http.MethodGet, "/api/jobs", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", rec.Code)
	}
}

func TestDownloadTickets(t *testing.T) {
	ts := newTestServer(t)
	anon := &testServer{t: t, s: ts.s}
	if rec := anon.raw(http.MethodGet, "/download/"+strings.Repeat("a", 64), "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown ticket: %d", rec.Code)
	}
	id := ts.s.auth.newTicket("r/job-1")
	ts.s.auth.now = func() time.Time { return time.Now().Add(2 * ticketTTL) }
	if _, ok := ts.s.auth.redeem(id); ok {
		t.Error("an expired ticket was redeemed")
	}
}

func TestSessionsExpire(t *testing.T) {
	ts := newTestServer(t)
	ts.s.auth.now = func() time.Time { return time.Now().Add(SessionTTL + time.Minute) }
	if rec := ts.raw(http.MethodGet, "/api/jobs", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired session: %d", rec.Code)
	}
}

func TestStaticFilesAndHeaders(t *testing.T) {
	ts := newTestServer(t)
	for path, ctype := range map[string]string{"/": "text/html", "/app.js": "javascript", "/dashboard.js": "javascript", "/style.css": "text/css", "/icon.svg": "image/svg+xml"} {
		rec := ts.raw(http.MethodGet, path, "", nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: CSP %q", path, csp)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
}

// The UI shows agent-written text; it must never be parsed as HTML.
func TestWebNeverParsesHTML(t *testing.T) {
	forbidden := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|DOMParser|createContextualFragment|srcdoc`)
	entries, err := webFS.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, _ := webFS.ReadFile("web/" + e.Name())
		if strings.HasSuffix(e.Name(), ".js") && forbidden.Match(data) {
			t.Errorf("%s uses %q", e.Name(), forbidden.Find(data))
		}
		if strings.HasSuffix(e.Name(), ".html") {
			if regexp.MustCompile(`<script(?:\s[^>]*)?>\s*[^<\s]`).Match(data) || regexp.MustCompile(`\son[a-z]+=|\sstyle=`).Match(data) {
				t.Errorf("%s has inline script, handlers or styles (the CSP forbids them)", e.Name())
			}
		}
	}
}

func TestLibrary(t *testing.T) {
	ts := newTestServer(t)
	var saved map[string]any
	if code := ts.do(http.MethodPut, "/api/harnesses", map[string]string{"yaml": okHarness}, &saved); code != http.StatusOK || saved["name"] != "hello" {
		t.Fatalf("save: %d %v", code, saved)
	}
	if fi, err := os.Stat(filepath.Join(ts.dir, "harnesses", "hello.yaml")); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("saved file: %v %v", fi, err)
	}
	var bad map[string]any
	if code := ts.do(http.MethodPut, "/api/harnesses", map[string]string{"yaml": "kind: Nope"}, &bad); code != http.StatusBadRequest || bad["check"] == nil {
		t.Errorf("invalid harness saved: %d %v", code, bad)
	}
	// A broken file put there by hand is listed with its problem.
	_ = os.WriteFile(filepath.Join(ts.dir, "harnesses", "broken.yaml"), []byte("nope: ["), 0o600)
	var list []Entry
	ts.do(http.MethodGet, "/api/harnesses", nil, &list)
	if len(list) != 2 || list[0].Name != "broken" || list[0].Error == "" || list[1].Risk != "low" || list[1].Description != "says hello" {
		t.Errorf("list = %+v", list)
	}
	var got struct {
		YAML  string `json:"yaml"`
		Check Check  `json:"check"`
	}
	ts.do(http.MethodGet, "/api/harnesses/hello", nil, &got)
	if got.YAML != okHarness || !got.Check.Valid || len(got.Check.Explain) == 0 {
		t.Errorf("get = %+v", got)
	}
	for _, name := range []string{"..%2Fcontrol", "Hello", "a_b"} {
		if code := ts.do(http.MethodGet, "/api/harnesses/"+name, nil, &map[string]any{}); code == http.StatusOK {
			t.Errorf("name %q accepted", name)
		}
	}
	if code := ts.do(http.MethodDelete, "/api/harnesses/hello", nil, &map[string]any{}); code != http.StatusOK {
		t.Errorf("delete: %d", code)
	}
	if code := ts.do(http.MethodDelete, "/api/harnesses/hello", nil, &map[string]any{}); code != http.StatusNotFound {
		t.Errorf("delete again: %d", code)
	}
}

func TestCheck(t *testing.T) {
	ts := newTestServer(t)
	var c Check
	ts.do(http.MethodPost, "/api/check", map[string]string{"yaml": okHarness}, &c)
	if !c.Valid || c.Name != "hello" || c.Risk != "low" || c.Target != "local" {
		t.Errorf("check = %+v", c)
	}
	ts.do(http.MethodPost, "/api/check", map[string]string{"yaml": strings.Replace(okHarness, "prompt: hi", "prompt: hi\n  bogus: 1", 1)}, &c)
	if c.Valid || len(c.Errors) == 0 {
		t.Errorf("unknown field accepted: %+v", c)
	}
	var e map[string]any
	if code := ts.do(http.MethodPost, "/api/check", map[string]any{"yaml": okHarness, "extra": 1}, &e); code != http.StatusBadRequest {
		t.Errorf("unknown request field: %d", code)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "control.yaml")
	c, err := LoadConfig(p)
	if err != nil || len(c.Runners) != 0 {
		t.Fatalf("missing file: %+v, %v", c, err)
	}
	bad := []Config{
		{Runners: []RunnerRef{{Name: "a"}}},
		{Runners: []RunnerRef{{Name: "a", SSH: "h", Socket: "/s"}}},
		{Runners: []RunnerRef{{Name: "a", SSH: "-oProxyCommand=x"}}},
		{Runners: []RunnerRef{{Name: "a", Socket: "rel/sock"}}},
		{Runners: []RunnerRef{{Name: "a", SSH: "h"}, {Name: "a", SSH: "g"}}},
		{Runners: []RunnerRef{{Name: "A", SSH: "h"}}},
		{Runners: []RunnerRef{{Name: "a", SSH: "h", RemoteSocket: "/x;rm"}}},
	}
	for _, b := range bad {
		if err := SaveConfig(p, &b); err == nil {
			t.Errorf("saved %+v", b)
		}
	}
	good := &Config{Runners: []RunnerRef{{Name: "dev", SSH: "hcc-dev"}}}
	if runtime.GOOS != "windows" { // runners are Unix; from Windows they are reached over ssh
		good.Runners = append(good.Runners, RunnerRef{Name: "local", Socket: "/run/hcc/runner.sock"})
	}
	if err := SaveConfig(p, good); err != nil {
		t.Fatal(err)
	}
	c, err = LoadConfig(p)
	if err != nil || len(c.Runners) != len(good.Runners) || c.Runners[0].SSH != "hcc-dev" {
		t.Errorf("reloaded %+v, %v", c, err)
	}
	_ = os.WriteFile(p, []byte("runners: []\nextra: 1\n"), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestListenLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.168.1.5:0", "localhost:0", ":0"} {
		if l, err := ListenLoopback(addr); err == nil {
			_ = l.Close()
			t.Errorf("%s accepted", addr)
		}
	}
	l, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
}

// --- against a real runner daemon ---

type uiProvider struct{}

func (uiProvider) Name() string { return "test" }

func (uiProvider) Run(ctx context.Context, s sandbox.Spec, console io.Writer) (sandbox.Result, error) {
	_, _ = io.WriteString(console, "hello \x1b]0;pwned\x07\u202e\n")
	if s.MemoryMiB == 256 {
		<-ctx.Done()
		return sandbox.Result{Outcome: sandbox.OutcomeKilled}, nil
	}
	if s.Collect != nil {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: "out.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644})
		_, _ = tw.Write([]byte("ok"))
		_ = tw.Close()
		if err := s.Collect(&buf); err != nil {
			return sandbox.Result{}, err
		}
	}
	return sandbox.Result{Outcome: sandbox.OutcomeExited}, nil
}

func startRunner(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets")
	}
	dir, err := os.MkdirTemp("/tmp", "hcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	log := slog.New(slog.DiscardHandler)
	state := filepath.Join(dir, "state")
	r, err := runner.New(runner.Config{Provider: uiProvider{}, Images: []string{"base"}, MaxConcurrent: 2,
		Console: runnerd.ConsoleFunc(state, log), Results: runnerd.ResultsFunc(state)})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "r.sock")
	l, err := runnerd.Listen(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = runnerd.NewServer(r, state, log).Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return sock
}

func TestRunThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	sock := startRunner(t)
	var ref map[string]any
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "local", Socket: sock}, &ref); code != http.StatusCreated {
		t.Fatalf("add runner: %d %v", code, ref)
	}
	if code := ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "local", Socket: sock}, &ref); code != http.StatusConflict {
		t.Errorf("duplicate runner: %d", code)
	}
	var runners []RunnerView
	ts.do(http.MethodGet, "/api/runners", nil, &runners)
	if len(runners) != 1 || runners[0].Status == nil || runners[0].Status.Target != "local" {
		t.Fatalf("runners = %+v", runners)
	}

	var out map[string]any
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "local", YAML: okHarness}, &out); code != http.StatusBadRequest {
		t.Errorf("unconfirmed run: %d %v", code, out)
	}
	other := strings.Replace(okHarness, "prompt: hi", "prompt: hi\n  target: server:elsewhere", 1)
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "local", YAML: other, Confirmed: true}, &out); code != http.StatusBadRequest ||
		!strings.Contains(out["error"].(string), "meant for") {
		t.Errorf("target mismatch: %d %v", code, out)
	}
	var started map[string]string
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "local", YAML: okHarness, Confirmed: true}, &started); code != http.StatusCreated {
		t.Fatalf("run: %d %v", code, started)
	}
	id := started["id"]
	jobPath := "/api/runners/local/jobs/" + id
	var j JobView
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ts.do(http.MethodGet, jobPath, nil, &j)
		if j.State.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if j.State != runner.StateSucceeded || j.Results == nil || j.Results.Files != 1 {
		t.Fatalf("job = %+v", j)
	}
	var list struct {
		Jobs   []JobView         `json:"jobs"`
		Errors map[string]string `json:"errors"`
	}
	ts.do(http.MethodGet, "/api/jobs", nil, &list)
	if len(list.Jobs) != 1 || list.Jobs[0].Runner != "local" || len(list.Errors) != 0 {
		t.Errorf("jobs = %+v", list)
	}
	var console map[string]string
	ts.do(http.MethodGet, jobPath+"/console", nil, &console)
	if strings.ContainsAny(console["text"], "\x1b\x07\u202e") || !strings.Contains(console["text"], `^[]0;pwned^G\u202e`) {
		t.Errorf("console not neutralized: %q", console["text"])
	}
	var ticket map[string]string
	ts.do(http.MethodPost, jobPath+"/download", map[string]string{}, &ticket)
	if !strings.HasPrefix(ticket["url"], "/download/") {
		t.Fatalf("ticket = %v", ticket)
	}
	anon := &testServer{t: t, s: ts.s}
	rec := anon.raw(http.MethodGet, ticket["url"], "", nil) // a link carries no session
	if again := anon.raw(http.MethodGet, ticket["url"], "", nil); again.Code != http.StatusNotFound {
		t.Errorf("a ticket worked twice: %d", again.Code)
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("results download: %d %v", rec.Code, rec.Header())
	}
	if h, err := tar.NewReader(rec.Body).Next(); err != nil || h.Name != "out.txt" {
		t.Errorf("archive: %+v %v", h, err)
	}

	// Kill switch on every runner, then resume.
	var killed []RunnerView
	ts.do(http.MethodPost, "/api/kill", map[string]string{}, &killed)
	if len(killed) != 1 || killed[0].Error != "" {
		t.Errorf("kill all = %+v", killed)
	}
	ts.do(http.MethodGet, "/api/runners", nil, &runners)
	if !runners[0].Status.Killed {
		t.Error("runner not stopped")
	}
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "local", YAML: okHarness, Confirmed: true}, &out); code == http.StatusCreated {
		t.Error("started a job while the kill switch was engaged")
	}
	ts.do(http.MethodPost, "/api/runners/local/resume", map[string]string{}, &out)
	ts.do(http.MethodGet, "/api/runners", nil, &runners)
	if runners[0].Status.Killed {
		t.Error("not resumed")
	}
	if code := ts.do(http.MethodDelete, "/api/runners/local", nil, &out); code != http.StatusOK {
		t.Errorf("remove runner: %d", code)
	}
}

func TestRunnerOneByOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets")
	}
	ts := newTestServer(t)
	sock := startRunner(t)
	var out map[string]any
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "up", Socket: sock}, &out)
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "down", Socket: "/nonexistent/hcc.sock"}, &out)

	var list []RunnerView
	if code := ts.do(http.MethodGet, "/api/runners?status=false", nil, &list); code != http.StatusOK ||
		len(list) != 2 || list[0].Name != "up" || list[0].Status != nil || list[1].Error != "" {
		t.Errorf("list only: %d %+v", code, list)
	}
	var up, down RunnerView
	if code := ts.do(http.MethodGet, "/api/runners/up", nil, &up); code != http.StatusOK || up.Status == nil || up.Status.Target != "local" || up.Error != "" {
		t.Errorf("up: %d %+v", code, up)
	}
	if code := ts.do(http.MethodGet, "/api/runners/down", nil, &down); code != http.StatusOK || down.Status != nil || down.Error == "" {
		t.Errorf("down: %d %+v", code, down)
	}
	if code := ts.do(http.MethodGet, "/api/runners/nope", nil, &out); code != http.StatusNotFound {
		t.Errorf("unknown runner: %d", code)
	}
	if code := ts.do(http.MethodGet, "/api/runners", nil, &list); code != http.StatusOK || len(list) != 2 || list[0].Status == nil || list[1].Error == "" {
		t.Errorf("all with status: %d %+v", code, list)
	}
}

func TestEditRunner(t *testing.T) {
	ts := newTestServer(t)
	dir := t.TempDir()
	sock := func(name string) string { return filepath.Join(dir, name) } // absolute on every OS
	var out map[string]any
	for _, ref := range []RunnerRef{{Name: "a", SSH: "host-a"}, {Name: "b", SSH: "host-b", RemoteSocket: "/run/hcc/other.sock"}, {Name: "c", Socket: sock("c.sock")}} {
		if code := ts.do(http.MethodPost, "/api/runners", ref, &out); code != http.StatusCreated {
			t.Fatalf("add %s: %d %v", ref.Name, code, out)
		}
	}
	// Rename and change how it is reached; its place in the list stays.
	if code := ts.do(http.MethodPut, "/api/runners/b", RunnerRef{Name: "b2", Socket: sock("b.sock")}, &out); code != http.StatusOK {
		t.Fatalf("edit: %d %v", code, out)
	}
	cfg, err := LoadConfig(ts.s.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []RunnerRef{{Name: "a", SSH: "host-a"}, {Name: "b2", Socket: sock("b.sock")}, {Name: "c", Socket: sock("c.sock")}}
	if !slices.Equal(cfg.Runners, want) {
		t.Errorf("saved runners = %+v, want %+v", cfg.Runners, want)
	}
	for _, tc := range []struct {
		path string
		ref  RunnerRef
		code int
	}{
		{"/api/runners/a", RunnerRef{Name: "c", SSH: "host-a"}, http.StatusConflict},                 // takes another's name
		{"/api/runners/nope", RunnerRef{Name: "nope", SSH: "x"}, http.StatusNotFound},                // no such runner
		{"/api/runners/a", RunnerRef{Name: "a", SSH: "x", Socket: sock("y")}, http.StatusBadRequest}, // both ways
		{"/api/runners/a", RunnerRef{Name: "A!", SSH: "x"}, http.StatusBadRequest},                   // bad name
		{"/api/runners/a", RunnerRef{Name: "a", SSH: "-oProxyCommand=x"}, http.StatusBadRequest},
	} {
		if code := ts.do(http.MethodPut, tc.path, tc.ref, &out); code != tc.code {
			t.Errorf("PUT %s %+v: %d, want %d (%v)", tc.path, tc.ref, code, tc.code, out)
		}
	}
	if cfg2, _ := LoadConfig(ts.s.cfgPath); !slices.Equal(cfg2.Runners, want) {
		t.Errorf("a refused edit changed the config: %+v", cfg2.Runners)
	}
}

func TestUnreachableRunnerIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets")
	}
	ts := newTestServer(t)
	var ref map[string]any
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "gone", Socket: "/nonexistent/hcc.sock"}, &ref)
	var list struct {
		Errors map[string]string `json:"errors"`
	}
	ts.do(http.MethodGet, "/api/jobs", nil, &list)
	if list.Errors["gone"] == "" {
		t.Errorf("errors = %v", list.Errors)
	}
	var killed []RunnerView
	ts.do(http.MethodPost, "/api/kill", map[string]string{}, &killed)
	if len(killed) != 1 || killed[0].Error == "" {
		t.Errorf("an unreachable runner must be reported by the kill switch: %+v", killed)
	}
}

// The whole server over a real loopback listener.
func TestServeOverLoopback(t *testing.T) {
	dir := t.TempDir()
	cfg, lib := ConfigPaths(dir)
	s, err := NewServer(cfg, lib, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	l, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(s.LoginURL(l))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.Request.URL.Path != "/" || resp.StatusCode != http.StatusOK || resp.Request.URL.RawQuery != "" {
		t.Errorf("after login: %d %s", resp.StatusCode, resp.Request.URL)
	}
	session := strings.TrimPrefix(resp.Request.Response.Header.Get("Location"), "/#session=")
	req, _ := http.NewRequest(http.MethodGet, "http://"+l.Addr().String()+"/api/jobs", nil)
	req.Header.Set(sessionHeader, session)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("api after login: %d", resp.StatusCode)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	resp, err = client.Get("http://localhost:" + port + "/")
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("localhost Host: %d", resp.StatusCode)
		}
	}
}

func TestSchedulesThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	sock := startRunner(t)
	var out map[string]any
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "local", Socket: sock}, &out)
	doc := okHarness + "  trigger: {schedule: '0 4 * * *'}\n"
	var c Check
	ts.do(http.MethodPost, "/api/check", map[string]string{"yaml": doc}, &c)
	if c.Trigger != "schedule" || c.Risk != "medium" {
		t.Errorf("check = %+v", c)
	}
	var added map[string]string
	if code := ts.do(http.MethodPost, "/api/runs", runRequest{Runner: "local", YAML: doc, Confirmed: true}, &added); code != http.StatusCreated || added["schedule"] == "" {
		t.Fatalf("schedule: %d %v", code, added)
	}
	var list struct {
		Schedules []ScheduleView    `json:"schedules"`
		Errors    map[string]string `json:"errors"`
	}
	ts.do(http.MethodGet, "/api/schedules", nil, &list)
	if len(list.Schedules) != 1 || list.Schedules[0].Runner != "local" || list.Schedules[0].Cron != "0 4 * * *" || list.Schedules[0].Document != nil {
		t.Fatalf("schedules = %+v", list)
	}
	path := "/api/runners/local/schedules/" + added["schedule"]
	if code := ts.do(http.MethodPost, path+"/pause", map[string]string{}, &out); code != http.StatusOK {
		t.Errorf("pause: %d %v", code, out)
	}
	if code := ts.do(http.MethodPost, path+"/explode", map[string]string{}, &out); code != http.StatusNotFound {
		t.Errorf("unknown action: %d", code)
	}
	ts.do(http.MethodGet, "/api/schedules", nil, &list)
	if list.Schedules[0].State != runner.SchedulePaused {
		t.Errorf("after pause: %+v", list.Schedules[0])
	}
	if code := ts.do(http.MethodDelete, path, nil, &out); code != http.StatusOK {
		t.Errorf("delete: %d %v", code, out)
	}
	ts.do(http.MethodGet, "/api/schedules", nil, &list)
	if len(list.Schedules) != 0 {
		t.Errorf("after delete: %+v", list.Schedules)
	}
}

func TestApprovalsThroughTheUI(t *testing.T) {
	ts := newTestServer(t)
	sock := startRunner(t)
	var out map[string]any
	ts.do(http.MethodPost, "/api/runners", RunnerRef{Name: "local", Socket: sock}, &out)
	var list struct {
		Approvals []ApprovalView    `json:"approvals"`
		Errors    map[string]string `json:"errors"`
	}
	ts.do(http.MethodGet, "/api/approvals", nil, &list)
	if len(list.Approvals) != 0 || len(list.Errors) != 0 {
		t.Errorf("approvals = %+v", list)
	}
	if code := ts.do(http.MethodPost, "/api/runners/local/approvals/apr-1", map[string]any{}, &out); code != http.StatusBadRequest {
		t.Errorf("decision without approve: %d", code)
	}
	if code := ts.do(http.MethodPost, "/api/runners/local/approvals/apr-1", map[string]any{"approve": true}, &out); code != http.StatusNotFound {
		t.Errorf("unknown approval: %d %v", code, out)
	}
}
