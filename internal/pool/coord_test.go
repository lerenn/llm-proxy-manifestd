package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// eachServer runs f against an in-process fake and, when LPM_TEST_VALKEY is
// set (host:port), against that real server too.
func eachServer(t *testing.T, f func(t *testing.T, addr string)) {
	t.Run("fake", func(t *testing.T) {
		m := miniredis.RunT(t)
		f(t, m.Addr())
	})
	if addr := os.Getenv("LPM_TEST_VALKEY"); addr != "" {
		t.Run("valkey", func(t *testing.T) { f(t, addr) })
	}
}

func newCoord(t *testing.T, addr string, prefix string, ttl time.Duration, closed bool) *Coordinator {
	t.Helper()
	c := NewCoordinator(CoordConfig{Address: addr, KeyPrefix: prefix, LeaseTTL: ttl, FailClosed: closed})
	t.Cleanup(func() { c.Close() })
	return c
}

func prefix(t *testing.T) string { return fmt.Sprintf("t%d", time.Now().UnixNano()) }

func waitQueue(t *testing.T, p *Pool, n int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if p.Stats().QueueDepth == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queue depth never reached %d (now %d)", n, p.Stats().QueueDepth)
}

func TestSharedSingleSlotAcrossInstances(t *testing.T) {
	eachServer(t, func(t *testing.T, addr string) {
		pre := prefix(t)
		a := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		b := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		relA := mustAcquire(t, a, 0)
		if _, ok := b.TryAcquire(); ok {
			t.Fatal("second instance must not get the slot")
		}
		got := make(chan func(), 1)
		go func() { got <- mustAcquire(t, b, 0) }()
		waitQueue(t, a, 1)
		if s := b.Stats(); !s.Coordinated || s.Active != 1 || s.QueueDepth != 1 {
			t.Fatalf("stats %+v", s)
		}
		select {
		case <-got:
			t.Fatal("granted while slot held")
		case <-time.After(100 * time.Millisecond):
		}
		relA()
		select {
		case rel := <-got:
			rel()
		case <-time.After(3 * time.Second):
			t.Fatal("waiter not granted after release")
		}
		if s := a.Stats(); s.Active != 0 || s.QueueDepth != 0 {
			t.Fatalf("stats %+v", s)
		}
	})
}

func TestSharedPriorityOrderAcrossInstances(t *testing.T) {
	eachServer(t, func(t *testing.T, addr string) {
		pre := prefix(t)
		a := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		b := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		hold := mustAcquire(t, a, 0)
		var mu sync.Mutex
		var order []int
		var wg sync.WaitGroup
		for i, w := range []struct {
			p    *Pool
			prio int
		}{{a, 1}, {b, 5}, {a, 3}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rel := mustAcquire(t, w.p, w.prio)
				mu.Lock()
				order = append(order, w.prio)
				mu.Unlock()
				rel()
			}()
			waitQueue(t, a, i+1)
			time.Sleep(5 * time.Millisecond)
		}
		hold()
		wg.Wait()
		if fmt.Sprint(order) != "[5 3 1]" {
			t.Fatalf("order %v", order)
		}
	})
}

func TestSharedAging(t *testing.T) {
	eachServer(t, func(t *testing.T, addr string) {
		pre := prefix(t)
		aging := 100 * time.Millisecond
		a := NewShared(1, aging, newCoord(t, addr, pre, 30*time.Second, false), "p")
		b := NewShared(1, aging, newCoord(t, addr, pre, 30*time.Second, false), "p")
		hold := mustAcquire(t, a, 0)
		var mu sync.Mutex
		var order []int
		var wg sync.WaitGroup
		start := func(p *Pool, prio int) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rel := mustAcquire(t, p, prio)
				mu.Lock()
				order = append(order, prio)
				mu.Unlock()
				rel()
			}()
		}
		start(a, 0) // enqueued first
		waitQueue(t, a, 1)
		time.Sleep(450 * time.Millisecond) // ages by ~4 priority levels
		start(b, 2)
		waitQueue(t, a, 2)
		hold()
		wg.Wait()
		if fmt.Sprint(order) != "[0 2]" {
			t.Fatalf("aged waiter should win: %v", order)
		}
	})
}

func TestSharedLeaseExpiresAfterCrash(t *testing.T) {
	eachServer(t, func(t *testing.T, addr string) {
		pre := prefix(t)
		ttl := 600 * time.Millisecond
		crashed := NewShared(1, 0, newCoord(t, addr, pre, ttl, false), "p")
		live := NewShared(1, 0, newCoord(t, addr, pre, ttl, false), "p")
		rel := mustAcquire(t, crashed, 0)
		// Simulate a crash: stop the heartbeat without releasing the lease.
		// (The release closure stops the heartbeat; we only close its connection.)
		crashed.coord.Close()
		_ = rel
		start := time.Now()
		r := mustAcquire(t, live, 0)
		defer r()
		if d := time.Since(start); d < 200*time.Millisecond {
			t.Fatalf("slot granted too early (%v): lease should still be live", d)
		}
	})
}

func TestSharedHeartbeatKeepsLease(t *testing.T) {
	eachServer(t, func(t *testing.T, addr string) {
		pre := prefix(t)
		ttl := 600 * time.Millisecond
		a := NewShared(1, 0, newCoord(t, addr, pre, ttl, false), "p")
		b := NewShared(1, 0, newCoord(t, addr, pre, ttl, false), "p")
		rel := mustAcquire(t, a, 0)
		defer rel()
		time.Sleep(2 * ttl)
		if _, ok := b.TryAcquire(); ok {
			t.Fatal("lease lost despite heartbeat")
		}
	})
}

func TestSharedQueueTimeoutAndCancel(t *testing.T) {
	eachServer(t, func(t *testing.T, addr string) {
		pre := prefix(t)
		a := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		b := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		hold := mustAcquire(t, a, 0)
		defer hold()
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if _, err := b.Acquire(ctx, 0); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("want deadline exceeded, got %v", err)
		}
		if d := a.Stats().QueueDepth; d != 0 {
			t.Fatalf("timed-out waiter left in queue: %d", d)
		}
	})
}

func TestSharedFailOpenFallsBackToLocal(t *testing.T) {
	m := miniredis.RunT(t)
	c := newCoord(t, m.Addr(), "x", 30*time.Second, false)
	p := NewShared(1, 0, c, "p")
	m.Close() // backend gone
	rel := mustAcquire(t, p, 0)
	if _, ok := p.TryAcquire(); ok {
		t.Fatal("local fallback must still enforce the limit")
	}
	rel()
	if _, ok := p.TryAcquire(); !ok {
		t.Fatal("slot should be free after release")
	}
}

func TestSharedFailClosedRejects(t *testing.T) {
	m := miniredis.RunT(t)
	c := newCoord(t, m.Addr(), "x", 30*time.Second, true)
	p := NewShared(1, 0, c, "p")
	m.Close()
	if _, err := p.Acquire(context.Background(), 0); !errors.Is(err, ErrCoordinationUnavailable) {
		t.Fatalf("want ErrCoordinationUnavailable, got %v", err)
	}
	if _, ok := p.TryAcquire(); ok {
		t.Fatal("TryAcquire must not succeed")
	}
}

func TestSharedUnlimitedNeverTouchesBackend(t *testing.T) {
	m := miniredis.RunT(t)
	c := newCoord(t, m.Addr(), "x", 30*time.Second, true)
	p := NewShared(Unlimited, 0, c, "p")
	m.Close()
	for i := 0; i < 3; i++ {
		rel, err := p.Acquire(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer rel()
	}
	if p.Stats().Coordinated {
		t.Fatal("unlimited pool must not report coordinated")
	}
}

func TestNoCoordinationUnchanged(t *testing.T) {
	p := New(1, 0)
	rel := mustAcquire(t, p, 0)
	if s := p.Stats(); s.Coordinated || s.Active != 1 {
		t.Fatalf("stats %+v", s)
	}
	rel()
}

func TestSharedResizeAppliesToSharedLimit(t *testing.T) {
	eachServer(t, func(t *testing.T, addr string) {
		pre := prefix(t)
		a := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		r1 := mustAcquire(t, a, 0)
		defer r1()
		got := make(chan func(), 1)
		go func() { got <- mustAcquire(t, a, 0) }()
		waitQueue(t, a, 1)
		a.Resize(2, 0)
		select {
		case r := <-got:
			r()
		case <-time.After(3 * time.Second):
			t.Fatal("resize did not release the waiter")
		}
	})
}

func TestSharedTryAcquireNeverQueues(t *testing.T) {
	eachServer(t, func(t *testing.T, addr string) {
		pre := prefix(t)
		a := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		b := NewShared(1, 0, newCoord(t, addr, pre, 30*time.Second, false), "p")
		rel, ok := a.TryAcquire()
		if !ok {
			t.Fatal("free slot must be taken")
		}
		if _, ok := b.TryAcquire(); ok {
			t.Fatal("slot held on another pod: try must fail")
		}
		if s := b.Stats(); s.QueueDepth != 0 || s.Active != 1 {
			t.Fatalf("failed try left state behind: %+v", s)
		}
		// A waiter ahead in the queue blocks try-acquire even once the slot frees.
		done := make(chan func(), 1)
		go func() { r, _ := b.Acquire(context.Background(), 0); done <- r }()
		waitQueue(t, b, 1)
		rel()
		if r, ok := a.TryAcquire(); ok {
			r()
			t.Fatal("try-acquire jumped the queue")
		}
		(<-done)()
		if s := a.Stats(); s.QueueDepth != 0 || s.Active != 0 {
			t.Fatalf("leftover state %+v", s)
		}
	})
}
