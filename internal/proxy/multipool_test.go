package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
)

// mpState builds a state with pools named after the given upstream URLs
// (a, b, c...) each with the given max concurrency, and route "m".
func mpState(t *testing.T, max int, qt string, urls ...string) *config.RuntimeState {
	t.Helper()
	var pools, names []string
	for i, u := range urls {
		n := string(rune('a' + i))
		names = append(names, n)
		pools = append(pools, "  "+n+": {upstream_url: '"+u+"', api_key: 'k-"+n+"', max_concurrency: "+string(rune('0'+max))+"}")
	}
	y := "server: {queue_timeout: '" + qt + "'}\npools:\n" + strings.Join(pools, "\n") +
		"\nroutes:\n  m: {pools: [" + strings.Join(names, ", ") + "], target_model: 'real@medium'}\n"
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	l := config.NewLoader(p, quiet())
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	return l.State()
}

func named(name string, hits *atomic.Int32, status int, hold chan struct{}) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		if hold != nil {
			<-hold
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(name + ":" + r.Header.Get("Authorization") + ":" + string(b)))
	}))
	return s
}

func TestMultiPoolPrefersLeastLoadedAndOpaqueSuffix(t *testing.T) {
	var ha, hb atomic.Int32
	hold := make(chan struct{})
	a, b := named("a", &ha, 200, hold), named("b", &hb, 200, nil)
	defer a.Close()
	defer b.Close()
	st := mpState(t, 2, "5s", a.URL, b.URL)
	srv := serve(t, st)

	// First request goes to a (tie -> order) and is held.
	done := make(chan *http.Response, 1)
	go func() { done <- post(t, srv.URL+"/v1/chat/completions", `{"model":"m"}`, nil) }()
	deadline := time.Now().Add(2 * time.Second)
	for st.Pools["a"].Stats().Active != 1 {
		if time.Now().After(deadline) {
			t.Fatal("first request never reached pool a")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Second: a has 1 in flight, b has 0 -> b.
	resp := post(t, srv.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("X-Pool-Name") != "b" {
		t.Fatalf("pool %q", resp.Header.Get("X-Pool-Name"))
	}
	if !strings.Contains(string(body), `"model":"real@medium"`) || !strings.Contains(string(body), "Bearer k-b") {
		t.Fatalf("body %s", body)
	}
	close(hold)
	r := <-done
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "a" {
		t.Fatalf("first pool %q", r.Header.Get("X-Pool-Name"))
	}
}

func TestMultiPoolAllFullQueuesOnLeastLoaded(t *testing.T) {
	var ha, hb atomic.Int32
	hold := make(chan struct{})
	a, b := named("a", &ha, 200, hold), named("b", &hb, 200, hold)
	defer a.Close()
	defer b.Close()
	st := mpState(t, 1, "5s", a.URL, b.URL)
	srv := serve(t, st)
	res := make(chan string, 3)
	fire := func() {
		go func() {
			r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
			r.Body.Close()
			res <- r.Header.Get("X-Pool-Name")
		}()
	}
	wait := func(cond func() bool) {
		t.Helper()
		for i := 0; i < 400 && !cond(); i++ {
			time.Sleep(5 * time.Millisecond)
		}
		if !cond() {
			t.Fatal("condition not reached")
		}
	}
	fire()
	wait(func() bool { return st.Pools["a"].Stats().Active == 1 })
	fire()
	wait(func() bool { return st.Pools["b"].Stats().Active == 1 })
	fire() // both full, equal load -> first by order (a) queues
	wait(func() bool { return st.Pools["a"].Stats().QueueDepth == 1 })
	if st.Pools["b"].Stats().QueueDepth != 0 {
		t.Fatal("should have queued on a only")
	}
	close(hold)
	for i := 0; i < 3; i++ {
		<-res
	}
}

func TestFailoverOn5xxAndTransportError(t *testing.T) {
	var ha, hb, hc atomic.Int32
	a, b, c := named("a", &ha, 503, nil), named("b", &hb, 200, nil), named("c", &hc, 200, nil)
	defer a.Close()
	defer c.Close()
	dead := b.URL
	b.Close() // transport error (connection refused)
	srv := serve(t, mpState(t, 1, "5s", a.URL, dead, c.URL))
	resp := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Pool-Name") != "c" || !strings.Contains(string(body), "Bearer k-c") {
		t.Fatalf("%d %q %s", resp.StatusCode, resp.Header.Get("X-Pool-Name"), body)
	}
	if ha.Load() != 1 {
		t.Fatalf("a hits %d", ha.Load())
	}
}

func TestFailoverExhaustedReturnsLastResponse(t *testing.T) {
	var ha, hb atomic.Int32
	a, b := named("a", &ha, 502, nil), named("b", &hb, 503, nil)
	defer a.Close()
	defer b.Close()
	st := mpState(t, 1, "5s", a.URL, b.URL)
	srv := serve(t, st)
	resp := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 503 || ha.Load() != 1 || hb.Load() != 1 {
		t.Fatalf("status %d hits %d/%d", resp.StatusCode, ha.Load(), hb.Load())
	}
	for n, p := range st.Pools {
		if p.Stats().Active != 0 {
			t.Fatalf("pool %s leaked a slot", n)
		}
	}
}

func TestNoFailoverOn4xxOrAfterStreamingStarted(t *testing.T) {
	var ha, hb atomic.Int32
	a, b := named("a", &ha, 429, nil), named("b", &hb, 200, nil)
	defer a.Close()
	defer b.Close()
	srv := serve(t, mpState(t, 1, "5s", a.URL, b.URL))
	resp := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 429 || hb.Load() != 0 {
		t.Fatalf("4xx must not fail over: %d, b hits %d", resp.StatusCode, hb.Load())
	}

	// Upstream a sends headers + a chunk, then drops the connection.
	var hb2 atomic.Int32
	a2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: partial\n\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer a2.Close()
	b2 := named("b", &hb2, 200, nil)
	defer b2.Close()
	srv2 := serve(t, mpState(t, 1, "5s", a2.URL, b2.URL))
	resp = post(t, srv2.URL+"/v1/x", `{"model":"m"}`, nil)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if hb2.Load() != 0 || !strings.Contains(string(data), "partial") {
		t.Fatalf("failover after streaming started: hits %d body %q", hb2.Load(), data)
	}
}

func TestMultiPoolQueueTimeoutAndHotReload(t *testing.T) {
	var ha atomic.Int32
	hold := make(chan struct{})
	a := named("a", &ha, 200, hold)
	defer a.Close()
	path := filepath.Join(t.TempDir(), "c.yaml")
	y := func(routes string) string {
		return "server: {queue_timeout: '300ms'}\npools:\n  a: {upstream_url: '" + a.URL + "', max_concurrency: 1}\n  b: {upstream_url: '" + a.URL + "', max_concurrency: 1}\nroutes:\n" + routes
	}
	_ = os.WriteFile(path, []byte(y("  m: {pool: a}\n")), 0o644)
	l := config.NewLoader(path, quiet())
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()

	go func() { r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil); r.Body.Close() }()
	for i := 0; i < 400 && l.State().Pools["a"].Stats().Active != 1; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	r := post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.StatusCode != 503 || r.Header.Get("X-Pool-Name") != "a" {
		t.Fatalf("expected queue timeout on a, got %d %q", r.StatusCode, r.Header.Get("X-Pool-Name"))
	}

	// Hot-reload: route now spans both pools; same request is served by b.
	_ = os.WriteFile(path, []byte(y("  m: {pools: [a, b]}\n")), 0o644)
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(50 * time.Millisecond); close(hold) }()
	r = post(t, srv.URL+"/v1/x", `{"model":"m"}`, nil)
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("X-Pool-Name") != "b" {
		t.Fatalf("after reload: %d %q", r.StatusCode, r.Header.Get("X-Pool-Name"))
	}
}

func TestDashboardShowsPoolsNoSecrets(t *testing.T) {
	st := mpState(t, 1, "5s", "https://u:hunter2@h1.example.com/v1?token=q", "http://h2.example.com:9000")
	h := New(func() *config.RuntimeState { return st }, quiet())
	rec := get(t, h, "proxy.test", "/dashboard/api/state")
	body := rec.Body.String()
	for _, want := range []string{`"pools":["a","b"]`, `"upstreams":["h1.example.com","h2.example.com:9000"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	for _, bad := range []string{"hunter2", "token=q", "k-a", "k-b", "https://"} {
		if strings.Contains(body, bad) {
			t.Errorf("leaks %q", bad)
		}
	}
}
