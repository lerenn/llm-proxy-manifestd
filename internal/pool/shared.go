package pool

import (
	"context"
	"sync"
	"time"
)

// sharedOn reports whether acquisitions go through the coordinator: it is
// configured and the pool is limited.
func (p *Pool) sharedOn() bool {
	if p.coord == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max != Unlimited
}

func (p *Pool) limits() (int, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max, p.aging
}

// tryShared takes a slot only when the shared queue is empty and a slot is
// free. err is errFallback when the local pool must be used instead, or
// ErrCoordinationUnavailable (fail-closed; ok is false then).
func (p *Pool) tryShared() (release func(), ok bool, err error) {
	c := p.coord
	if c.isDown() {
		return nil, false, c.unavailable()
	}
	max, _ := p.limits()
	member := newMember(0)
	st, prev, err := c.take(context.Background(), p.key, member, max, false)
	if err != nil {
		c.markDown(err)
		return nil, false, c.unavailable()
	}
	c.noteMax(p.key, max, prev)
	if st != 1 {
		return nil, false, nil
	}
	p.served.Add(1)
	return p.sharedReleaser(member), true, nil
}

func (p *Pool) acquireShared(ctx context.Context, prio int) (func(), error) {
	c := p.coord
	if c.isDown() {
		return nil, c.unavailable()
	}
	if prio > maxPriority {
		prio = maxPriority
	} else if prio < -maxPriority {
		prio = -maxPriority
	}
	member := newMember(prio)
	max, aging := p.limits()
	fail := func(err error) (func(), error) {
		if ctx.Err() != nil {
			_ = c.abandon(p.key, member)
			return nil, ctx.Err()
		}
		c.markDown(err)
		_ = c.abandon(p.key, member)
		return nil, c.unavailable()
	}
	if err := c.enqueue(ctx, p.key, member, prio, aging); err != nil {
		return fail(err)
	}
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		max, aging = p.limits()
		if max == Unlimited { // reloaded to "no limit" while waiting
			_ = c.abandon(p.key, member)
			p.mu.Lock()
			p.active++
			p.mu.Unlock()
			p.served.Add(1)
			return p.releaser(), nil
		}
		st, prev, err := c.take(ctx, p.key, member, max, true)
		if err != nil {
			return fail(err)
		}
		c.noteMax(p.key, max, prev)
		switch st {
		case 1:
			p.served.Add(1)
			return p.sharedReleaser(member), nil
		case -1: // waiter entry purged (this pod stalled): queue again
			if err := c.enqueue(ctx, p.key, member, prio, aging); err != nil {
				return fail(err)
			}
		}
		select {
		case <-ctx.Done():
			_ = c.abandon(p.key, member)
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// sharedReleaser starts the lease heartbeat and returns an idempotent release.
func (p *Pool) sharedReleaser(member string) func() {
	c := p.coord
	stop := make(chan struct{})
	go c.heartbeat(p.key, member, stop)
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			if err := c.abandon(p.key, member); err != nil {
				c.log.Warn("lease release failed; it will expire by TTL", "pool", p.key, "error", err)
			}
		})
	}
}
