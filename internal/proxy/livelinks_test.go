package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
)

func linkHandler(t *testing.T, st *config.RuntimeState) (*Handler, *httptest.Server) {
	t.Helper()
	h := New(func() *config.RuntimeState { return st }, quiet())
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return h, s
}

// links returns in_flight per "pool/target_model" for endpoint openai/m.
func links(h *Handler) map[string]int64 {
	snap, _ := h.dashboardSnapshot()
	out := map[string]int64{}
	for _, e := range snap.Endpoints {
		if e.Name == "m" {
			for _, tg := range e.Targets {
				out[tg.Pool+"/"+tg.Target] = tg.InFlight
			}
		}
	}
	return out
}

func waitLinks(t *testing.T, h *Handler, key string, want int64) {
	t.Helper()
	dl := time.Now().Add(3 * time.Second)
	for links(h)[key] != want {
		if time.Now().After(dl) {
			t.Fatalf("link %s = %d, want %d", key, links(h)[key], want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLinkCounterConcurrentAndStreamEnd(t *testing.T) {
	hold := make(chan struct{})
	var hits int
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-hold
		_, _ = w.Write([]byte("data: x\n\n"))
	}))
	defer up.Close()
	st := mpState(t, 3, "5s", up.URL)
	h, s := linkHandler(t, st)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := post(t, s.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
			_, _ = io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}()
	}
	waitLinks(t, h, "a/real@medium", 3)
	close(hold) // stream ends
	wg.Wait()
	waitLinks(t, h, "a/real@medium", 0)
}

func TestLinkCounterFailoverDecrementsBeforeNext(t *testing.T) {
	var hb atomicInt
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer bad.Close()
	hold := make(chan struct{})
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hb.add()
		<-hold
		w.WriteHeader(200)
	}))
	defer good.Close()
	st := mpStateMode(t, "failover", 2, "5s", bad.URL, good.URL)
	h, s := linkHandler(t, st)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r := post(t, s.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
		r.Body.Close()
	}()
	waitLinks(t, h, "b/real@medium", 1)
	if got := links(h)["a/real@medium"]; got != 0 {
		t.Errorf("failed attempt still counted: a=%d", got)
	}
	close(hold)
	<-done
	waitLinks(t, h, "b/real@medium", 0)
}

type atomicInt struct {
	mu sync.Mutex
	n  int
}

func (a *atomicInt) add() { a.mu.Lock(); a.n++; a.mu.Unlock() }

func TestLinkCounterCancellation(t *testing.T) {
	hold := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	defer close(hold)
	st := mpState(t, 2, "5s", up.URL)
	h, s := linkHandler(t, st)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	go func() { _, _ = http.DefaultClient.Do(req) }()
	waitLinks(t, h, "a/real@medium", 1)
	cancel()
	waitLinks(t, h, "a/real@medium", 0)
	if st.Pools["a"].Stats().Active != 0 {
		t.Error("slot leaked")
	}
}

func TestLinkCounterSurvivesReload(t *testing.T) {
	hold := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
	}))
	defer up.Close()
	st1 := mpState(t, 2, "5s", up.URL)
	cur := st1
	var mu sync.Mutex
	h := New(func() *config.RuntimeState { mu.Lock(); defer mu.Unlock(); return cur }, quiet())
	s := httptest.NewServer(h)
	defer s.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r := post(t, s.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
		r.Body.Close()
	}()
	waitLinks(t, h, "a/real@medium", 1)

	// Hot reload renames the target model: the old link is gone from the
	// snapshot, the new one starts at 0, and nothing goes negative.
	st2 := mpState(t, 2, "5s", up.URL)
	st2.Config.Endpoints[config.ProtocolOpenAI]["m"].Targets[0].TargetModel = "renamed"
	mu.Lock()
	cur = st2
	mu.Unlock()
	if got := links(h); got["a/renamed"] != 0 || len(got) != 1 {
		t.Errorf("after reload: %v", got)
	}
	close(hold)
	<-done
	dl := time.Now().Add(2 * time.Second)
	for st1.Pools["a"].Stats().Active != 0 && time.Now().Before(dl) {
		time.Sleep(5 * time.Millisecond)
	}
	if v, ok := h.counters(counterKey("openai", "m")).links.Load(linkKey("a", "real@medium")); !ok || v.(interface{ Load() int64 }).Load() != 0 {
		t.Errorf("old link counter not back to zero")
	}
}

func TestDashboardLiveViewRoute(t *testing.T) {
	st := dashState(t, "{}")
	h := New(func() *config.RuntimeState { return st }, quiet())
	for _, host := range []struct{ host, path string }{{"dashboard.x.test", "/"}, {"proxy.test", "/dashboard/"}} {
		body := get(t, h, host.host, host.path).Body.String()
		for _, want := range []string{`href="#/live"`, `id="view-live"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%v: missing %s", host, want)
			}
		}
	}
	js := get(t, h, "dashboard.x.test", "/app.js").Body.String()
	if !strings.Contains(js, "'live'") {
		t.Error("app.js has no live view")
	}
}
