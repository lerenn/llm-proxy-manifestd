// Package pool implements a concurrency-limited slot pool with a priority
// wait queue and direct handoff of released slots.
package pool

import (
	"container/heap"
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Stats is a point-in-time snapshot of a pool.
type Stats struct {
	MaxConcurrency int   `json:"max_concurrency"`
	Active         int   `json:"active_slots"`
	QueueDepth     int   `json:"queue_depth"`
	TotalServed    int64 `json:"total_served"`
}

// Pool limits the number of concurrently held slots. Callers that cannot get
// a slot immediately wait in a priority queue.
type Pool struct {
	mu     sync.Mutex
	max    int
	aging  time.Duration
	active int
	queue  waiterHeap
	seq    uint64
	served atomic.Int64

	now func() time.Time // replaceable in tests
}

type waiter struct {
	priority int
	enqueued time.Time
	seq      uint64
	ready    chan struct{}
	index    int
	granted  bool
}

// New creates a pool with the given maximum concurrency (min 1) and aging
// interval (0 disables aging).
func New(maxConcurrency int, aging time.Duration) *Pool {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	p := &Pool{max: maxConcurrency, aging: aging, now: time.Now}
	p.queue.pool = p
	return p
}

// Resize updates the concurrency limit and aging interval on a live pool.
// Raising the limit immediately hands slots to queued waiters; lowering it
// never revokes slots already held, it only stops new grants until the
// active count drops below the new limit.
func (p *Pool) Resize(maxConcurrency int, aging time.Duration) {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.max = maxConcurrency
	p.aging = aging
	for p.active < p.max && len(p.queue.items) > 0 {
		p.active++
		p.grantNextLocked()
	}
}

// Acquire obtains a slot, waiting in priority order if none is free. The
// returned release function is idempotent and must be called once the slot is
// no longer needed. Higher priority values are served first.
func (p *Pool) Acquire(ctx context.Context, priority int) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.active < p.max && len(p.queue.items) == 0 {
		p.active++
		p.mu.Unlock()
		p.served.Add(1)
		return p.releaser(), nil
	}
	w := &waiter{
		priority: priority,
		enqueued: p.now(),
		ready:    make(chan struct{}),
	}
	p.seq++
	w.seq = p.seq
	p.queue.at = w.enqueued
	heap.Push(&p.queue, w)
	p.mu.Unlock()

	select {
	case <-w.ready:
		p.served.Add(1)
		return p.releaser(), nil
	case <-ctx.Done():
		p.mu.Lock()
		if w.granted {
			// Slot was handed to us concurrently: give it back.
			p.mu.Unlock()
			p.release()
			return nil, ctx.Err()
		}
		heap.Remove(&p.queue, w.index)
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Stats returns a snapshot of the pool state.
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Stats{
		MaxConcurrency: p.max,
		Active:         p.active,
		QueueDepth:     len(p.queue.items),
		TotalServed:    p.served.Load(),
	}
}

func (p *Pool) releaser() func() {
	var once sync.Once
	return func() { once.Do(p.release) }
}

func (p *Pool) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue.items) > 0 && p.active <= p.max {
		// Direct handoff: the slot stays counted as active.
		p.grantNextLocked()
		return
	}
	p.active--
}

// grantNextLocked pops the best waiter (by effective priority at this
// instant) and signals it. The caller accounts for the slot.
func (p *Pool) grantNextLocked() {
	if p.aging > 0 {
		// Effective priorities drift with time, so re-establish the heap
		// invariant against the current clock before popping.
		p.queue.at = p.now()
		heap.Init(&p.queue)
	}
	w := heap.Pop(&p.queue).(*waiter)
	w.granted = true
	close(w.ready)
}

// waiterHeap is a max-heap on effective priority, FIFO on ties.
type waiterHeap struct {
	items []*waiter
	pool  *Pool
	at    time.Time
}

func (h *waiterHeap) effective(w *waiter) int {
	if h.pool.aging <= 0 {
		return w.priority
	}
	return w.priority + int(h.at.Sub(w.enqueued)/h.pool.aging)
}

func (h *waiterHeap) Len() int { return len(h.items) }

func (h *waiterHeap) Less(i, j int) bool {
	a, b := h.items[i], h.items[j]
	if pa, pb := h.effective(a), h.effective(b); pa != pb {
		return pa > pb
	}
	if !a.enqueued.Equal(b.enqueued) {
		return a.enqueued.Before(b.enqueued)
	}
	return a.seq < b.seq
}

func (h *waiterHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].index = i
	h.items[j].index = j
}

func (h *waiterHeap) Push(x any) {
	w := x.(*waiter)
	w.index = len(h.items)
	h.items = append(h.items, w)
}

func (h *waiterHeap) Pop() any {
	n := len(h.items)
	w := h.items[n-1]
	h.items[n-1] = nil
	h.items = h.items[:n-1]
	w.index = -1
	return w
}
