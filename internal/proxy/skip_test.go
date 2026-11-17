package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// skipYAML: endpoint m = [a (limit 1, when_concurrency_full), b (limit 5)].
func skipYAML(a, b, whenBusy, extra string) string {
	return `server: {queue_timeout: '300ms'}
` + extra + `pools:
  a: {upstream_url: '` + a + `', max_concurrency: 1}
  b: {upstream_url: '` + b + `', max_concurrency: 5}
endpoints:
  openai:
    m: {mode: failover, targets: [{pool: a, when_concurrency_full: ` + whenBusy + `}, {pool: b}]}
`
}

func fire(t *testing.T, url string) chan *http.Response {
	ch := make(chan *http.Response, 1)
	go func() { ch <- post(t, url+"/v1/x", `{"model":"m"}`, nil) }()
	return ch
}

func skipped(t *testing.T, h *Handler, ep, pool string) int64 {
	t.Helper()
	snap, _ := h.dashboardSnapshot()
	for _, e := range snap.Endpoints {
		if e.Name == ep {
			for _, tg := range e.Targets {
				if tg.Pool == pool {
					return tg.SkippedConcurrencyFull
				}
			}
		}
	}
	t.Fatal("target not found")
	return 0
}

func TestSkipSpillsOverImmediately(t *testing.T) {
	var ha, hb atomic.Int32
	hold := make(chan struct{})
	a, b := named("a", &ha, 200, hold), named("b", &hb, 200, nil)
	defer a.Close()
	defer b.Close()
	l, _ := protoLoader(t, skipYAML(a.URL, b.URL, "skip", ""))
	h := New(l.State, quiet())
	srv := httptest.NewServer(h)
	defer srv.Close()
	st := l.State()

	first := fire(t, srv.URL)
	waitFor(t, func() bool { return st.Pools["a"].Stats().Active == 1 })
	start := time.Now()
	r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "b" || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("pool %q after %s", r.Header.Get("X-Pool-Name"), time.Since(start))
	}
	if st.Pools["a"].Stats().QueueDepth != 0 {
		t.Fatal("skip must not leave a waiter")
	}
	if n := skipped(t, h, "m", "a"); n != 1 {
		t.Fatalf("skipped_concurrency_full %d", n)
	}
	// The skipped target never counts in flight.
	snap, _ := h.dashboardSnapshot()
	if x := snap.Endpoints[0].Targets[0].InFlight; x != 1 {
		t.Fatalf("a in_flight %d", x)
	}
	close(hold)
	r1 := <-first
	r1.Body.Close()
	if r1.Header.Get("X-Pool-Name") != "a" {
		t.Fatal("first request must use a")
	}
	// Free slot again -> first target, no extra skip.
	r = post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "a" || skipped(t, h, "m", "a") != 1 {
		t.Fatalf("pool %q", r.Header.Get("X-Pool-Name"))
	}
}

func TestWaitIsDefaultAndQueues(t *testing.T) {
	var ha, hb atomic.Int32
	hold := make(chan struct{})
	a, b := named("a", &ha, 200, hold), named("b", &hb, 200, nil)
	defer a.Close()
	defer b.Close()
	l, _ := protoLoader(t, skipYAML(a.URL, b.URL, "wait", ""))
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()
	st := l.State()
	first := fire(t, srv.URL)
	waitFor(t, func() bool { return st.Pools["a"].Stats().Active == 1 })
	second := fire(t, srv.URL)
	waitFor(t, func() bool { return st.Pools["a"].Stats().QueueDepth == 1 })
	close(hold)
	(<-first).Body.Close()
	r := <-second
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "a" || hb.Load() != 0 {
		t.Fatal("wait must queue on a")
	}
}

func TestSkipWhenQueueNotEmpty(t *testing.T) {
	var ha, hb atomic.Int32
	a, b := named("a", &ha, 200, nil), named("b", &hb, 200, nil)
	defer a.Close()
	defer b.Close()
	l, _ := protoLoader(t, skipYAML(a.URL, b.URL, "skip", ""))
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()
	st := l.State()
	// Hold a's slot and put a waiter in its queue, then free the slot only
	// by releasing: with a waiter ahead, a newcomer must skip, not jump.
	rel, _ := st.Pools["a"].Acquire(context.Background(), 0)
	done := make(chan func(), 1)
	go func() { r, _ := st.Pools["a"].Acquire(context.Background(), 0); done <- r }()
	waitFor(t, func() bool { return st.Pools["a"].Stats().QueueDepth == 1 })
	r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "b" {
		t.Fatalf("pool %q", r.Header.Get("X-Pool-Name"))
	}
	rel()
	(<-done)()
}

func TestSkipLastTargetWaits(t *testing.T) {
	var ha, hb atomic.Int32
	hold := make(chan struct{})
	a, b := named("a", &ha, 200, hold), named("b", &hb, 200, hold)
	defer a.Close()
	defer b.Close()
	y := `server: {queue_timeout: '5s'}
pools:
  a: {upstream_url: '` + a.URL + `', max_concurrency: 1}
  b: {upstream_url: '` + b.URL + `', max_concurrency: 1}
endpoints:
  openai:
    m: {mode: failover, targets: [{pool: a, when_concurrency_full: skip}, {pool: b, when_concurrency_full: skip}]}
`
	l, _ := protoLoader(t, y)
	h := New(l.State, quiet())
	srv := httptest.NewServer(h)
	defer srv.Close()
	st := l.State()
	c1 := fire(t, srv.URL) // a
	waitFor(t, func() bool { return st.Pools["a"].Stats().Active == 1 })
	c2 := fire(t, srv.URL) // skips a -> b
	waitFor(t, func() bool { return st.Pools["b"].Stats().Active == 1 })
	c3 := fire(t, srv.URL) // skips a, waits on b (last)
	waitFor(t, func() bool { return st.Pools["b"].Stats().QueueDepth == 1 })
	if skipped(t, h, "m", "b") != 0 || skipped(t, h, "m", "a") != 2 {
		t.Fatalf("a=%d b=%d", skipped(t, h, "m", "a"), skipped(t, h, "m", "b"))
	}
	close(hold)
	for _, c := range []chan *http.Response{c1, c2, c3} {
		(<-c).Body.Close()
	}
}

func TestSkipFailoverCombined(t *testing.T) {
	var ha, hb, hc atomic.Int32
	hold := make(chan struct{})
	a, b, c := named("a", &ha, 200, hold), named("b", &hb, 503, nil), named("c", &hc, 200, nil)
	defer a.Close()
	defer b.Close()
	defer c.Close()
	y := `server: {queue_timeout: '300ms'}
pools:
  a: {upstream_url: '` + a.URL + `', max_concurrency: 1}
  b: {upstream_url: '` + b.URL + `', max_concurrency: 1}
  c: {upstream_url: '` + c.URL + `', max_concurrency: 1}
endpoints:
  openai:
    m: {mode: failover, targets: [{pool: a, when_concurrency_full: skip}, {pool: b}, {pool: c}]}
`
	l, _ := protoLoader(t, y)
	h := New(l.State, quiet())
	srv := httptest.NewServer(h)
	defer srv.Close()
	st := l.State()
	first := fire(t, srv.URL)
	waitFor(t, func() bool { return st.Pools["a"].Stats().Active == 1 })
	// a skipped (not an error), b answers 503 -> fails over to c; a is not retried
	// and no error is counted for the skip.
	r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "c" || ha.Load() != 1 || hb.Load() != 1 || hc.Load() != 1 {
		t.Fatalf("pool %q hits %d %d %d", r.Header.Get("X-Pool-Name"), ha.Load(), hb.Load(), hc.Load())
	}
	if n := skipped(t, h, "m", "a"); n != 1 {
		t.Fatalf("skipped %d", n)
	}
	close(hold)
	(<-first).Body.Close()
}

func TestSkipLeastInflightAndRoundRobinExcludeSkipped(t *testing.T) {
	for _, mode := range []string{"least-inflight", "round-robin"} {
		var ha, hb atomic.Int32
		hold := make(chan struct{})
		a, b := named("a", &ha, 200, hold), named("b", &hb, 200, nil)
		y := `server: {queue_timeout: '300ms'}
pools:
  a: {upstream_url: '` + a.URL + `', max_concurrency: 1}
  b: {upstream_url: '` + b.URL + `', max_concurrency: 1}
endpoints:
  openai:
    m: {mode: ` + mode + `, targets: [{pool: a, when_concurrency_full: skip}, {pool: b}]}
`
		l, _ := protoLoader(t, y)
		srv := httptest.NewServer(New(l.State, quiet()))
		st := l.State()
		first := fire(t, srv.URL)
		waitFor(t, func() bool { return st.Pools["a"].Stats().Active == 1 || st.Pools["b"].Stats().Active == 1 })
		// Occupy a explicitly so the outcome is deterministic.
		rel, ok := st.Pools["a"].TryAcquire()
		if ok {
			defer rel()
		}
		for i := 0; i < 3; i++ {
			r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
			r.Body.Close()
			if r.Header.Get("X-Pool-Name") == "a" {
				t.Fatalf("%s: busy skip target used", mode)
			}
		}
		if st.Pools["a"].Stats().QueueDepth != 0 {
			t.Fatalf("%s: queued on skipped target", mode)
		}
		close(hold)
		(<-first).Body.Close()
		srv.Close()
		a.Close()
		b.Close()
	}
}

func TestSkipGroupSharedAcrossPoolsAndUnlimited(t *testing.T) {
	var hits atomic.Int32
	up := named("u", &hits, 200, nil)
	defer up.Close()
	y := `server: {queue_timeout: '300ms'}
pools:
  g1: {upstream_url: '` + up.URL + `', max_concurrency: 1, concurrency_group: g}
  g2: {upstream_url: '` + up.URL + `', max_concurrency: 1, concurrency_group: g}
  free: {upstream_url: '` + up.URL + `'}
  other: {upstream_url: '` + up.URL + `', max_concurrency: 5}
endpoints:
  openai:
    m: {mode: failover, targets: [{pool: g2, when_concurrency_full: skip}, {pool: other}]}
    u: {mode: failover, targets: [{pool: free, when_concurrency_full: skip}, {pool: other}]}
`
	l, _ := protoLoader(t, y)
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()
	st := l.State()
	rel, _ := st.Pools["g1"].Acquire(context.Background(), 0) // holds the group's slot
	r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "other" {
		t.Fatalf("group busy via g1 must skip g2, got %q", r.Header.Get("X-Pool-Name"))
	}
	rel()
	r = post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "g2" {
		t.Fatalf("free group slot: %q", r.Header.Get("X-Pool-Name"))
	}
	for i := 0; i < 4; i++ {
		r = post(t, srv.URL+"/v1/x", `{"model":"u"}`, nil)
		r.Body.Close()
		if r.Header.Get("X-Pool-Name") != "free" {
			t.Fatal("unlimited pool is never busy")
		}
	}
}

func TestSkipHotReload(t *testing.T) {
	var ha, hb atomic.Int32
	hold := make(chan struct{})
	a, b := named("a", &ha, 200, hold), named("b", &hb, 200, nil)
	defer a.Close()
	defer b.Close()
	l, path := protoLoader(t, skipYAML(a.URL, b.URL, "wait", ""))
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()
	first := fire(t, srv.URL)
	waitFor(t, func() bool { return l.State().Pools["a"].Stats().Active == 1 })
	if err := os.WriteFile(path, []byte(skipYAML(a.URL, b.URL, "skip", "")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "b" {
		t.Fatalf("after reload to skip: %q", r.Header.Get("X-Pool-Name"))
	}
	// Back to wait: the next request queues (and times out at 300ms).
	_ = os.WriteFile(path, []byte(skipYAML(a.URL, b.URL, "wait", "")), 0o644)
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	r = post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.StatusCode != 503 {
		t.Fatalf("wait again must queue and time out, got %d", r.StatusCode)
	}
	close(hold)
	(<-first).Body.Close()
}

func TestSkipCoordinatedAcrossPods(t *testing.T) {
	mr := miniredis.RunT(t)
	var ha, hb atomic.Int32
	hold := make(chan struct{})
	a, b := named("a", &ha, 200, hold), named("b", &hb, 200, nil)
	defer a.Close()
	defer b.Close()
	extra := "coordination: {valkey: {address: '" + mr.Addr() + "'}}\n"
	y := skipYAML(a.URL, b.URL, "skip", extra)
	l1, _ := protoLoader(t, y)
	l2, _ := protoLoader(t, y)
	s1, s2 := httptest.NewServer(New(l1.State, quiet())), httptest.NewServer(New(l2.State, quiet()))
	defer s1.Close()
	defer s2.Close()
	first := fire(t, s1.URL)
	waitFor(t, func() bool { return l2.State().Pools["a"].Stats().Active == 1 })
	start := time.Now()
	r := post(t, s2.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "b" || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("pod 2 must spill to b at once: %q %s", r.Header.Get("X-Pool-Name"), time.Since(start))
	}
	if q := l2.State().Pools["a"].Stats().QueueDepth; q != 0 {
		t.Fatalf("queue %d", q)
	}
	close(hold)
	(<-first).Body.Close()
}

func TestSkippedConcurrencyFullInDashboardJSON(t *testing.T) {
	var ha, hb atomic.Int32
	a, b := named("a", &ha, 200, nil), named("b", &hb, 200, nil)
	defer a.Close()
	defer b.Close()
	l, _ := protoLoader(t, skipYAML(a.URL, b.URL, "skip", ""))
	h := New(l.State, quiet())
	snap, _ := h.dashboardSnapshot()
	raw, _ := json.Marshal(snap.Endpoints[0].Targets)
	if !strings.Contains(string(raw), `"skipped_concurrency_full":0`) || !strings.Contains(string(raw), `"when_concurrency_full":"skip"`) {
		t.Fatal(string(raw))
	}
}
