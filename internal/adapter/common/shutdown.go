package common

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	draining atomic.Bool
	mu       sync.Mutex
	children = map[*exec.Cmd]struct{}{}
)

// Isolate puts the CLI child in its own process group, so a SIGINT/SIGTERM
// aimed at the adapter's group (Ctrl-C, `docker stop` on a shell, ...) does
// not kill a turn that must be allowed to finish, and so KillGroup can take
// down the CLI together with anything it spawned.
func Isolate(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillGroup SIGKILLs the child's whole process group (the child must have
// been Isolate'd and started).
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// Track registers a started child so a shutdown timeout can kill it. Call the
// returned function once the child has been waited for.
func Track(cmd *exec.Cmd) (untrack func()) {
	mu.Lock()
	children[cmd] = struct{}{}
	mu.Unlock()
	return func() {
		mu.Lock()
		delete(children, cmd)
		mu.Unlock()
	}
}

// KillChildren kills every tracked CLI child (process-group kill).
func KillChildren() int {
	mu.Lock()
	defer mu.Unlock()
	for c := range children {
		_ = KillGroup(c)
	}
	return len(children)
}

// Gate makes GET /ready answer 503 "shutting down" as soon as the shutdown
// starts, and refuses any other request that still reaches the server (a
// kept-alive connection) while draining.
func Gate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if draining.Load() {
			w.Header().Set("Connection", "close")
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Serve runs hs until ctx is cancelled (SIGTERM/SIGINT), then stops accepting
// connections, flips /ready to 503 and waits up to timeout for in-flight
// requests. It returns nil on a clean drain; after the timeout it kills the
// remaining CLI children and returns an error.
func Serve(ctx context.Context, hs *http.Server, timeout time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	draining.Store(true)
	log.Printf("shutting down: waiting up to %s for in-flight requests", timeout)
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := hs.Shutdown(sctx)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	n := KillChildren()
	_ = hs.Close()
	return fmt.Errorf("shutdown timeout after %s: killed %d CLI process(es): %w", timeout, n, err)
}

// ResetDrain clears the draining flag (tests only).
func ResetDrain() { draining.Store(false) }
