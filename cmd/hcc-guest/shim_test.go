package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestPluginShimRelaysLines(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs cat")
	}
	var mu sync.Mutex
	polls := 0
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/hcc/v1/plugin/in":
			polls++
			switch {
			case polls == 1:
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n")
			case len(posted) == 0:
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusGone) // the session is over
			}
		case "/hcc/v1/plugin/out":
			b, _ := io.ReadAll(r.Body)
			posted = append(posted, string(b))
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	t.Setenv("HCC_PLUGIN_API", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1") // must be ignored
	var stderr strings.Builder
	// cat echoes each request back, as if it were the response.
	if code := pluginShim([]string{"--", "cat"}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 1 || posted[0] != `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n" {
		t.Errorf("posted %q", posted)
	}
}

func TestPluginShimNeedsItsAPI(t *testing.T) {
	t.Setenv("HCC_PLUGIN_API", "")
	var stderr strings.Builder
	if code := pluginShim([]string{"--", "cat"}, &stderr); code != 2 {
		t.Errorf("exit %d", code)
	}
}
