package pool

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func mustAcquire(t *testing.T, p *Pool, prio int) func() {
	t.Helper()
	rel, err := p.Acquire(context.Background(), prio)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return rel
}

// waitQueued blocks until the pool has n queued waiters.
func waitQueued(t *testing.T, p *Pool, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.Stats().QueueDepth != n {
		if time.Now().After(deadline) {
			t.Fatalf("queue depth = %d, want %d", p.Stats().QueueDepth, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// enqueue starts an acquirer and waits until it is queued. Acquired slots are
// reported on out (by label) and must be released via the returned channel.
type result struct {
	label   string
	release func()
}

func enqueue(t *testing.T, p *Pool, prio int, label string, out chan<- result, depth int) {
	t.Helper()
	go func() {
		rel, err := p.Acquire(context.Background(), prio)
		if err != nil {
			return
		}
		out <- result{label, rel}
	}()
	waitQueued(t, p, depth)
}

func next(t *testing.T, out <-chan result) result {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for grant")
		return result{}
	}
}

func TestFastPathAndStats(t *testing.T) {
	p := New(2, 0)
	r1 := mustAcquire(t, p, 0)
	r2 := mustAcquire(t, p, 0)
	s := p.Stats()
	if s.MaxConcurrency != 2 || s.Active != 2 || s.QueueDepth != 0 || s.TotalServed != 2 {
		t.Fatalf("unexpected stats %+v", s)
	}
	r1()
	r2()
	if s := p.Stats(); s.Active != 0 || s.TotalServed != 2 {
		t.Fatalf("unexpected stats %+v", s)
	}
}

func TestPriorityOrder(t *testing.T) {
	p := New(1, 0)
	hold := mustAcquire(t, p, 0)
	out := make(chan result, 10)
	enqueue(t, p, 10, "low", out, 1)
	enqueue(t, p, 100, "high", out, 2)
	enqueue(t, p, 50, "mid", out, 3)

	hold()
	for _, want := range []string{"high", "mid", "low"} {
		r := next(t, out)
		if r.label != want {
			t.Fatalf("got %s, want %s", r.label, want)
		}
		if a := p.Stats().Active; a != 1 {
			t.Fatalf("active = %d during handoff, want 1", a)
		}
		r.release()
	}
	if a := p.Stats().Active; a != 0 {
		t.Fatalf("active = %d, want 0", a)
	}
}

func TestFIFOForTies(t *testing.T) {
	p := New(1, 0)
	hold := mustAcquire(t, p, 0)
	out := make(chan result, 10)
	labels := []string{"a", "b", "c", "d"}
	for i, l := range labels {
		enqueue(t, p, 5, l, out, i+1)
	}
	hold()
	for _, want := range labels {
		r := next(t, out)
		if r.label != want {
			t.Fatalf("got %s, want %s", r.label, want)
		}
		r.release()
	}
}

func TestAging(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	p := New(1, 10*time.Second)
	p.now = clk.Now
	hold := mustAcquire(t, p, 0)
	out := make(chan result, 10)

	enqueue(t, p, 10, "old-low", out, 1)
	clk.Advance(35 * time.Second) // old-low effective: 10 + 3 = 13
	enqueue(t, p, 12, "new-high", out, 2)

	hold()
	if r := next(t, out); r.label != "old-low" {
		t.Fatalf("got %s, want old-low (aged)", r.label)
	}
}

func TestAgingDisabledKeepsBasePriority(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	p := New(1, 0)
	p.now = clk.Now
	hold := mustAcquire(t, p, 0)
	out := make(chan result, 10)
	enqueue(t, p, 10, "old-low", out, 1)
	clk.Advance(time.Hour)
	enqueue(t, p, 12, "new-high", out, 2)
	hold()
	if r := next(t, out); r.label != "new-high" {
		t.Fatalf("got %s, want new-high", r.label)
	}
}

func TestCancellationRemovesWaiter(t *testing.T) {
	p := New(1, 0)
	hold := mustAcquire(t, p, 0)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.Acquire(ctx, 1)
		errc <- err
	}()
	waitQueued(t, p, 1)
	cancel()
	if err := <-errc; err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d := p.Stats().QueueDepth; d != 0 {
		t.Fatalf("queue depth = %d, want 0", d)
	}
	hold()
	if a := p.Stats().Active; a != 0 {
		t.Fatalf("active = %d, want 0 (no leak)", a)
	}
}

func TestAcquireWithCancelledContext(t *testing.T) {
	p := New(1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Acquire(ctx, 0); err == nil {
		t.Fatal("expected error")
	}
	if a := p.Stats().Active; a != 0 {
		t.Fatalf("active = %d, want 0", a)
	}
}

func TestCancelledWaiterSkippedOnRelease(t *testing.T) {
	p := New(1, 0)
	hold := mustAcquire(t, p, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = p.Acquire(ctx, 100) }()
	waitQueued(t, p, 1)
	out := make(chan result, 1)
	enqueue(t, p, 1, "survivor", out, 2)
	cancel()
	waitQueued(t, p, 1)
	hold()
	next(t, out).release()
}

func TestNoBargingOnHandoff(t *testing.T) {
	p := New(1, 0)
	hold := mustAcquire(t, p, 0)
	out := make(chan result, 1)
	enqueue(t, p, 1, "waiter", out, 1)
	hold()
	// The slot was handed to the waiter; a new arrival must queue.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, 1000); err == nil {
		t.Fatal("new arrival barged past handed-off slot")
	}
	next(t, out).release()
}

func TestIdempotentRelease(t *testing.T) {
	p := New(1, 0)
	rel := mustAcquire(t, p, 0)
	out := make(chan result, 2)
	enqueue(t, p, 1, "w", out, 1)
	rel()
	rel()
	rel()
	r := next(t, out)
	if a := p.Stats().Active; a != 1 {
		t.Fatalf("active = %d, want 1 (double release must not free the waiter's slot)", a)
	}
	r.release()
	if a := p.Stats().Active; a != 0 {
		t.Fatalf("active = %d, want 0", a)
	}
}

func TestResizeUp(t *testing.T) {
	p := New(1, 0)
	hold := mustAcquire(t, p, 0)
	out := make(chan result, 3)
	enqueue(t, p, 1, "a", out, 1)
	enqueue(t, p, 1, "b", out, 2)
	p.Resize(3, 0)
	next(t, out)
	next(t, out)
	if s := p.Stats(); s.Active != 3 || s.MaxConcurrency != 3 {
		t.Fatalf("stats %+v", s)
	}
	hold()
}

func TestResizeDownDoesNotRevoke(t *testing.T) {
	p := New(3, 0)
	r := []func(){mustAcquire(t, p, 0), mustAcquire(t, p, 0), mustAcquire(t, p, 0)}
	p.Resize(1, 0)
	out := make(chan result, 1)
	enqueue(t, p, 1, "w", out, 1)
	r[0]()
	r[1]()
	select {
	case <-out:
		t.Fatal("waiter granted while active >= new max")
	case <-time.After(50 * time.Millisecond):
	}
	r[2]()
	next(t, out).release()
	if a := p.Stats().Active; a != 0 {
		t.Fatalf("active = %d", a)
	}
}

// TestStressNoLeak hammers the pool with random cancellations and checks the
// invariants hold. Run with -race.
func TestStressNoLeak(t *testing.T) {
	const max = 3
	p := New(max, time.Millisecond)
	var wg sync.WaitGroup
	var mu sync.Mutex
	cur, peak := 0, 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%7)*time.Millisecond+time.Millisecond)
			defer cancel()
			rel, err := p.Acquire(ctx, i%5)
			if err != nil {
				return
			}
			mu.Lock()
			cur++
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			rel()
		}(i)
	}
	wg.Wait()
	if peak > max {
		t.Fatalf("peak concurrency %d exceeded %d", peak, max)
	}
	if s := p.Stats(); s.Active != 0 || s.QueueDepth != 0 {
		t.Fatalf("leaked state: %+v", s)
	}
}

func TestQueueByPriority(t *testing.T) {
	p := New(1, 0)
	rel, _ := p.Acquire(context.Background(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, prio := range []int{10, 10, 1} {
		go func(prio int) { _, _ = p.Acquire(ctx, prio) }(prio)
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.Stats().QueueDepth < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := p.QueueByPriority()
	if got[10] != 2 || got[1] != 1 {
		t.Errorf("got %v", got)
	}
	rel()
}

func TestTryAcquire(t *testing.T) {
	p := New(1, 0)
	rel, ok := p.TryAcquire()
	if !ok {
		t.Fatal("expected slot")
	}
	if _, ok := p.TryAcquire(); ok {
		t.Fatal("pool is full")
	}
	rel()
	if _, ok := p.TryAcquire(); !ok {
		t.Fatal("slot should be free again")
	}
}
