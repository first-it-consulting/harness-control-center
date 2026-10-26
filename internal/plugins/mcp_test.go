package plugins

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// shim plays hcc-guest plugin-shim with an echo server: it answers every
// request with a result naming the method.
func shim(t *testing.T, s *session, stop <-chan struct{}) {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			resp, err := http.Get(srv.URL + "/hcc/v1/plugin/in")
			if err != nil {
				return
			}
			if resp.StatusCode == http.StatusGone {
				_ = resp.Body.Close()
				return
			}
			sc := bufio.NewScanner(resp.Body)
			var out strings.Builder
			for sc.Scan() {
				line := sc.Text()
				if !strings.Contains(line, `"id"`) {
					continue // a notification
				}
				id := line[strings.Index(line, `"id":`)+5 : strings.Index(line, `,"method"`)]
				out.WriteString(`{"jsonrpc":"2.0","id":` + id + `,"result":{"echo":true}}` + "\n")
			}
			_ = resp.Body.Close()
			if out.Len() > 0 {
				r, err := http.Post(srv.URL+"/hcc/v1/plugin/out", "application/x-ndjson", strings.NewReader("garbage\n"+out.String()))
				if err == nil {
					_ = r.Body.Close()
				}
			}
		}
	}()
}

func TestSessionRelaysRequestsAndResponses(t *testing.T) {
	s := newSession()
	stop := make(chan struct{})
	defer close(stop)
	shim(t, s, stop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Send([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"1", `"two"`} {
		resp, err := s.Call(ctx, id, []byte(`{"jsonrpc":"2.0","id":`+id+`,"method":"tools/list"}`))
		if err != nil || !strings.Contains(string(resp), `"id":`+id+`,"result"`) {
			t.Fatalf("Call(%s) = %s, %v", id, resp, err)
		}
	}
	// When the plugin stops, callers learn it.
	s.end(io.EOF)
	if _, err := s.Call(ctx, "3", []byte(`{"jsonrpc":"2.0","id":3,"method":"x"}`)); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("Call after the end = %v", err)
	}
}

func TestSessionPollTimesOutEmpty(t *testing.T) {
	s := newSession()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hcc/v1/plugin/in", nil).WithContext(ctx))
	if w.Body.Len() != 0 {
		t.Errorf("poll with nothing waiting returned %q", w.Body)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hcc/v1/agents", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("other path = %d", w.Code)
	}
}
