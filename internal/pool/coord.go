package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrCoordinationUnavailable is returned by Acquire when the Valkey server
// cannot be reached and the coordinator runs in fail-closed mode.
var ErrCoordinationUnavailable = errors.New("coordination backend unavailable")

// errFallback tells the pool to use its local in-memory path (fail-open).
var errFallback = errors.New("coordination backend unavailable, using local pool")

const (
	pollInterval  = 30 * time.Millisecond
	opTimeout     = time.Second
	downBackoff   = 2 * time.Second
	statsCacheTTL = 200 * time.Millisecond
	maxPriority   = 1000
)

// CoordConfig configures a Coordinator.
type CoordConfig struct {
	Address    string
	Password   string
	KeyPrefix  string // default "lpm"
	LeaseTTL   time.Duration
	FailClosed bool // reject with ErrCoordinationUnavailable instead of falling back
	Logger     *slog.Logger
}

// Coordinator shares slot limits and priority queues between several proxy
// pods through a Valkey/Redis-protocol server. See the "coordination" guide.
type Coordinator struct {
	cfg  CoordConfig
	rdb  *redis.Client
	log  *slog.Logger
	down atomic.Int64 // unix nanos until which the backend is considered down

	mu     sync.Mutex
	cache  map[string]cachedStats
	warned map[string]time.Time
}

type cachedStats struct {
	at     time.Time
	active int
	queue  map[int]int
}

// NewCoordinator creates a coordinator. It connects lazily.
func NewCoordinator(cfg CoordConfig) *Coordinator {
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "lpm"
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Coordinator{
		cfg: cfg,
		log: cfg.Logger,
		rdb: redis.NewClient(&redis.Options{
			Addr: cfg.Address, Password: cfg.Password,
			DialTimeout: opTimeout, ReadTimeout: opTimeout, WriteTimeout: opTimeout,
			MaxRetries: -1, // fail fast: the pool decides what to do
		}),
		cache:  map[string]cachedStats{},
		warned: map[string]time.Time{},
	}
}

// Close releases the connection pool.
func (c *Coordinator) Close() error { return c.rdb.Close() }

// Ping checks connectivity (used by tests and diagnostics).
func (c *Coordinator) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

func (c *Coordinator) isDown() bool { return time.Now().UnixNano() < c.down.Load() }

func (c *Coordinator) markDown(err error) {
	if !c.isDown() {
		c.log.Warn("coordination backend unavailable", "address", c.cfg.Address, "fail_mode", c.failMode(), "error", err)
	}
	c.down.Store(time.Now().Add(downBackoff).UnixNano())
}

func (c *Coordinator) failMode() string {
	if c.cfg.FailClosed {
		return "closed"
	}
	return "open"
}

// unavailable maps a backend failure to the configured fail mode.
func (c *Coordinator) unavailable() error {
	if c.cfg.FailClosed {
		return ErrCoordinationUnavailable
	}
	return errFallback
}

func (c *Coordinator) k(key, suffix string) string {
	return "{" + c.cfg.KeyPrefix + ":" + key + "}:" + suffix
}

func newMember(prio int) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return strconv.Itoa(prio) + ":" + hex.EncodeToString(b[:])
}

func memberPriority(m string) int {
	p, _, _ := strings.Cut(m, ":")
	n, _ := strconv.Atoi(p)
	return n
}

// Scripts. Time always comes from the server (TIME), never from a pod clock.
const luaNow = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local nowS = string.format('%.0f', now)
`

// KEYS: queue, leases, waiter-liveness. ARGV: member, priority, aging_ms, waiter_ttl_ms.
// Linear aging makes the ordering key static: score = enqueue/aging - priority
// (ascending), so each waiter's score is computed once.
var enqueueScript = redis.NewScript(luaNow + `
local prio = tonumber(ARGV[2]); local aging = tonumber(ARGV[3])
local rel = now - 1735689600000
local score
if aging > 0 then score = rel / aging - prio else score = rel - prio * 1e12 end
redis.call('ZADD', KEYS[1], score, ARGV[1])
redis.call('ZADD', KEYS[3], string.format('%.0f', now + tonumber(ARGV[4])), ARGV[1])
return 1`)

// KEYS: queue, leases, waiters, max-marker. ARGV: member, max, lease_ttl_ms,
// waiter_ttl_ms, queued(0/1). Returns {status, previous stored max} with
// status 1 = lease taken, 0 = not yet, -1 = member not in the queue any more.
var takeScript = redis.NewScript(luaNow + `
local q, l, w, mk = KEYS[1], KEYS[2], KEYS[3], KEYS[4]
local id = ARGV[1]
local max = tonumber(ARGV[2])
redis.call('ZREMRANGEBYSCORE', l, '-inf', nowS)
local stale = redis.call('ZRANGEBYSCORE', w, '-inf', nowS)
for i = 1, #stale, 500 do
  local chunk = {}
  for j = i, math.min(i + 499, #stale) do chunk[#chunk + 1] = stale[j] end
  redis.call('ZREM', q, unpack(chunk)); redis.call('ZREM', w, unpack(chunk))
end
local prev = redis.call('GET', mk)
redis.call('SET', mk, ARGV[2], 'EX', 86400)
if ARGV[5] == '1' then
  if not redis.call('ZSCORE', q, id) then return {-1, prev or ''} end
  redis.call('ZADD', w, string.format('%.0f', now + tonumber(ARGV[4])), id)
end
local head = redis.call('ZRANGE', q, 0, 0)[1]
if (head == nil or head == id) and (max == 0 or redis.call('ZCARD', l) < max) then
  redis.call('ZADD', l, string.format('%.0f', now + tonumber(ARGV[3])), id)
  redis.call('ZREM', q, id); redis.call('ZREM', w, id)
  return {1, prev or ''}
end
return {0, prev or ''}`)

// KEYS: leases. ARGV: member, lease_ttl_ms. Returns 1 if the lease still exists.
var renewScript = redis.NewScript(luaNow + `
if redis.call('ZSCORE', KEYS[1], ARGV[1]) then
  redis.call('ZADD', KEYS[1], string.format('%.0f', now + tonumber(ARGV[2])), ARGV[1])
  return 1
end
return 0`)

// KEYS: queue, leases, waiters. Returns {live leases, {queued members...}}.
var statsScript = redis.NewScript(luaNow + `
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', nowS)
local stale = redis.call('ZRANGEBYSCORE', KEYS[3], '-inf', nowS)
for i = 1, #stale, 500 do
  local chunk = {}
  for j = i, math.min(i + 499, #stale) do chunk[#chunk + 1] = stale[j] end
  redis.call('ZREM', KEYS[1], unpack(chunk)); redis.call('ZREM', KEYS[3], unpack(chunk))
end
return {redis.call('ZCARD', KEYS[2]), redis.call('ZRANGE', KEYS[1], 0, 999)}`)

func (c *Coordinator) keys(key string) []string {
	return []string{c.k(key, "q"), c.k(key, "l"), c.k(key, "w")}
}

func (c *Coordinator) enqueue(ctx context.Context, key, member string, prio int, aging time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	defer c.invalidate(key)
	return enqueueScript.Run(ctx, c.rdb, c.keys(key), member, prio, aging.Milliseconds(), c.cfg.LeaseTTL.Milliseconds()).Err()
}

// take attempts to convert member into a lease. queued says the member is in
// the wait queue (otherwise it only succeeds when the queue is empty).
func (c *Coordinator) take(ctx context.Context, key, member string, max int, queued bool) (status int, prevMax string, err error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	defer c.invalidate(key)
	q := "0"
	if queued {
		q = "1"
	}
	keys := append(c.keys(key), c.k(key, "max"))
	res, err := takeScript.Run(ctx, c.rdb, keys, member, max, c.cfg.LeaseTTL.Milliseconds(), c.cfg.LeaseTTL.Milliseconds(), q).Slice()
	if err != nil {
		return 0, "", err
	}
	if len(res) != 2 {
		return 0, "", fmt.Errorf("unexpected take reply %v", res)
	}
	st, _ := res[0].(int64)
	pm, _ := res[1].(string)
	return int(st), pm, nil
}

// abandon removes member from the queue and from the leases, best effort, on a
// fresh context (the caller's is usually already cancelled).
func (c *Coordinator) abandon(key, member string) error {
	defer c.invalidate(key)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	k := c.keys(key)
	_, err := c.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.ZRem(ctx, k[0], member)
		p.ZRem(ctx, k[1], member)
		p.ZRem(ctx, k[2], member)
		return nil
	})
	return err
}

func (c *Coordinator) renew(ctx context.Context, key, member string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	n, err := renewScript.Run(ctx, c.rdb, []string{c.k(key, "l")}, member, c.cfg.LeaseTTL.Milliseconds()).Int()
	return n == 1, err
}

// heartbeat renews the lease until stop is closed.
func (c *Coordinator) heartbeat(key, member string, stop <-chan struct{}) {
	t := time.NewTicker(c.cfg.LeaseTTL / 3)
	defer t.Stop()
	lost := false
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			ok, err := c.renew(context.Background(), key, member)
			switch {
			case err != nil:
				if !lost {
					c.log.Warn("lease renewal failed", "pool", key, "error", err)
				}
				lost = true
			case !ok:
				if !lost {
					c.log.Warn("lease expired before renewal; slot no longer counted by other pods", "pool", key)
				}
				lost = true
			default:
				lost = false
			}
		}
	}
}

// snapshot returns the live lease count and queued waiters per priority,
// cached for a short time. ok is false when the backend is unavailable.
func (c *Coordinator) snapshot(key string) (active int, queue map[int]int, ok bool) {
	c.mu.Lock()
	if cs, hit := c.cache[key]; hit && time.Since(cs.at) < statsCacheTTL {
		c.mu.Unlock()
		return cs.active, cs.queue, true
	}
	c.mu.Unlock()
	if c.isDown() {
		return 0, nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout/2)
	defer cancel()
	res, err := statsScript.Run(ctx, c.rdb, c.keys(key)).Slice()
	if err != nil || len(res) != 2 {
		if err != nil {
			c.markDown(err)
		}
		return 0, nil, false
	}
	n, _ := res[0].(int64)
	queue = map[int]int{}
	if ms, _ := res[1].([]any); ms != nil {
		for _, m := range ms {
			if s, _ := m.(string); s != "" {
				queue[memberPriority(s)]++
			}
		}
	}
	c.mu.Lock()
	c.cache[key] = cachedStats{at: time.Now(), active: int(n), queue: queue}
	c.mu.Unlock()
	return int(n), queue, true
}

// noteMax warns (at most every 5 minutes per pool) when another pod stored a
// different limit for the same shared pool.
func (c *Coordinator) noteMax(key string, max int, prev string) {
	if prev == "" || prev == strconv.Itoa(max) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.warned[key]; ok && time.Since(t) < 5*time.Minute {
		return
	}
	c.warned[key] = time.Now()
	c.log.Warn("another pod uses a different max_concurrency for this shared pool; every pod must have the same value",
		"pool", key, "this_pod", max, "stored", prev)
}

// invalidate drops the cached stats of key after a local state change.
func (c *Coordinator) invalidate(key string) {
	c.mu.Lock()
	delete(c.cache, key)
	c.mu.Unlock()
}
