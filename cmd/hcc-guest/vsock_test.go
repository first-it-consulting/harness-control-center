package main

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
)

func TestVsockArgs(t *testing.T) {
	if ap, port, err := vsockArgs("127.0.0.1:3128", "1024"); err != nil || ap.String() != "127.0.0.1:3128" || port != 1024 {
		t.Errorf("valid: %v %d %v", ap, port, err)
	}
	for _, bad := range [][2]string{
		{"0.0.0.0:3128", "1024"},  // not loopback
		{"10.0.0.1:3128", "1024"}, // not loopback
		{"127.0.0.1:0", "1024"},   // no port
		{"localhost:3128", "1"},   // a name, not an address
		{"127.0.0.1:3128", "0"},
		{"127.0.0.1:3128", "-1"},
		{"127.0.0.1:3128", "x"},
	} {
		if _, _, err := vsockArgs(bad[0], bad[1]); err == nil {
			t.Errorf("vsockArgs(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

// forward joins each accepted connection to a dialed one, both ways.
func TestForward(t *testing.T) {
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Close() }()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				line, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = c.Write([]byte(strings.ToUpper(line)))
			}()
		}
	}()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- forward(ctx, l, func() (net.Conn, error) { return net.Dial("tcp", up.Addr().String()) })
	}()
	for range 3 {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte("hello\n"))
		got, _ := bufio.NewReader(c).ReadString('\n')
		_ = c.Close()
		if got != "HELLO\n" {
			t.Errorf("got %q", got)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("forward: %v", err)
	}
}
