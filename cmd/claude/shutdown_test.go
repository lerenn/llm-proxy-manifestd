package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/common"
)

func slowCLI(t *testing.T, sleep string) string {
	t.Helper()
	dir := t.TempDir()
	body := "#!/bin/sh\ncat >/dev/null\nsleep " + sleep + "\nprintf '%s\\n' '" + delta1 + "' '" + delta2 + "' '" + asst + "' '" + result + "'\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func startServe(t *testing.T, bin string, timeout time.Duration) (base string, stop context.CancelFunc, done chan error) {
	t.Helper()
	common.ResetDrain()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	s := &Server{Bin: bin, ConfigDir: loggedIn(t)}
	hs := &http.Server{Addr: addr, Handler: common.Gate(s.Handler())}
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- common.Serve(ctx, hs, timeout) }()
	base = "http://" + addr
	for i := 0; i < 100; i++ {
		if r, err := http.Get(base + "/ready"); err == nil {
			r.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return base, cancel, done
}

func TestGracefulShutdownFinishesInFlight(t *testing.T) {
	base, stop, done := startServe(t, slowCLI(t, "1"), 10*time.Second)
	type res struct {
		code int
		body string
		err  error
	}
	rc := make(chan res, 1)
	go func() {
		r, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			rc <- res{err: err}
			return
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		rc <- res{r.StatusCode, string(b), nil}
	}()
	time.Sleep(300 * time.Millisecond) // request is running the CLI
	stop()
	time.Sleep(100 * time.Millisecond)

	// Ready flips immediately; new connections are refused or answered 503.
	if r, err := http.Get(base + "/ready"); err == nil {
		r.Body.Close()
		if r.StatusCode != 503 {
			t.Fatalf("ready during drain = %d", r.StatusCode)
		}
	}
	if r, err := http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{}`)); err == nil {
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Fatal("new request accepted during drain")
		}
	}

	got := <-rc
	if got.err != nil || got.code != 200 || !strings.Contains(got.body, "Hello") {
		t.Fatalf("in-flight request cut: %+v", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("Serve = %v, want clean exit", err)
	}
}

func TestShutdownTimeoutKillsChild(t *testing.T) {
	base, stop, done := startServe(t, slowCLI(t, "30"), 300*time.Millisecond)
	go http.Post(base+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	stop()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "killed 1") {
			t.Fatalf("Serve = %v, want timeout error killing 1 child", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after timeout")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("took too long")
	}
}
