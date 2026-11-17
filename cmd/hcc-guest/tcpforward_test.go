package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConnectProxy allows CONNECT to allowed only, and then echoes what
// it gets (as the far host would), so a tunnel can be checked end to end.
func fakeConnectProxy(t *testing.T, allowed string) (addr string, asked func() []string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var mu sync.Mutex
	var targets []string
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				mu.Lock()
				targets = append(targets, req.Method+" "+req.Host)
				mu.Unlock()
				if req.Method != http.MethodConnect || req.Host != allowed {
					_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
					return
				}
				// The first bytes may come with the request.
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				_, _ = io.Copy(c, br)
			}()
		}
	}()
	return l.Addr().String(), func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), targets...) }
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestTCPForward(t *testing.T) {
	proxy, asked := fakeConnectProxy(t, "db.example:5432")
	okRoute, refusedRoute := freeAddr(t), freeAddr(t)
	cfg := &forwardConfig{Proxy: proxy, Routes: []forwardRoute{
		{Listen: okRoute, Target: "db.example:5432"},
		{Listen: refusedRoute, Target: "other.example:22"},
	}}
	if err := cfg.check(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = serveForwards(ctx, cfg, io.Discard); close(done) }()
	defer func() { cancel(); <-done }()

	var c net.Conn
	var err error
	for i := 0; i < 50; i++ {
		if c, err = net.Dial("tcp", okRoute); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "SELECT 1;")
	_ = c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	_ = c.Close()
	if string(got) != "SELECT 1;" {
		t.Errorf("through the tunnel: %q", got)
	}

	// Refused by the proxy: the connection just ends.
	r, err := net.Dial("tcp", refusedRoute)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(r, "SSH-2.0-test\r\n")
	if b, _ := io.ReadAll(r); len(b) != 0 {
		t.Errorf("a refused tunnel carried %q", b)
	}
	_ = r.Close()
	if a := strings.Join(asked(), ", "); a != "CONNECT db.example:5432, CONNECT other.example:22" {
		t.Errorf("asked the proxy: %s", a)
	}
}

func TestForwardConfigCheck(t *testing.T) {
	good := forwardConfig{Proxy: "10.0.0.1:3128", Routes: []forwardRoute{{Listen: "127.77.0.1:5432", Target: "db.lan:5432"}}}
	if err := good.check(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*forwardConfig){
		"no routes":        func(c *forwardConfig) { c.Routes = nil },
		"bad proxy":        func(c *forwardConfig) { c.Proxy = "proxy" },
		"public listen":    func(c *forwardConfig) { c.Routes[0].Listen = "0.0.0.0:5432" },
		"no port":          func(c *forwardConfig) { c.Routes[0].Listen = "127.77.0.1:0" },
		"target no port":   func(c *forwardConfig) { c.Routes[0].Target = "db.lan" },
		"target injection": func(c *forwardConfig) { c.Routes[0].Target = "db.lan:5432\r\nX: y" },
		"target space":     func(c *forwardConfig) { c.Routes[0].Target = "db lan:5432" },
	} {
		c := good
		c.Routes = append([]forwardRoute(nil), good.Routes...)
		change(&c)
		if err := c.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
