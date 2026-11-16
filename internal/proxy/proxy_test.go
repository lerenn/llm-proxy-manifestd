package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
	"github.com/lerenn/llm-proxy-manifestd/internal/pool"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newState(t *testing.T, upstream string, max int, queueTimeout time.Duration) *config.RuntimeState {
	t.Helper()
	yml := `
server: {queue_timeout: "` + queueTimeout.String() + `"}
pools:
  p: {upstream_url: "` + upstream + `", api_key: "sekret", max_concurrency: ` + string(rune('0'+max)) + `}
routes:
  fast: {pool: p, target_model: real-model, priority: 100}
  slow: {pool: p, target_model: real-model, priority: 1}
  same: {pool: p}
`
	cfg, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	return &config.RuntimeState{Config: cfg, Pools: map[string]*pool.Pool{"p": pool.New(max, 0)}}
}

func serve(t *testing.T, st *config.RuntimeState) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(New(func() *config.RuntimeState { return st }, quiet()))
	t.Cleanup(s.Close)
	return s
}

func post(t *testing.T, url, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRewriteAndHeaders(t *testing.T) {
	var gotBody map[string]any
	var gotHdr http.Header
	var gotPath string
	var gotLen int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHdr, gotPath, gotLen = r.Header.Clone(), r.URL.Path, r.ContentLength
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	s := serve(t, newState(t, up.URL, 1, time.Second))

	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		resp := post(t, s.URL+path, `{"model":"fast","stream":true}`, map[string]string{
			"Authorization": "Bearer client", "X-Session-Id": "abc", "OpenAI-Beta": "responses=v1", "X-Priority": "5",
		})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		if resp.Header.Get("X-Pool-Name") != "p" || resp.Header.Get("X-Queue-Wait-Ms") == "" {
			t.Fatalf("missing response headers: %v", resp.Header)
		}
		if gotPath != path {
			t.Fatalf("path %s != %s", gotPath, path)
		}
		if gotBody["model"] != "real-model" || gotBody["stream"] != true {
			t.Fatalf("body %v", gotBody)
		}
		if gotHdr.Get("Authorization") != "Bearer sekret" || gotHdr.Get("X-Session-Id") != "abc" ||
			gotHdr.Get("OpenAI-Beta") != "responses=v1" || gotHdr.Get("X-Priority") != "" {
			t.Fatalf("headers %v", gotHdr)
		}
		if gotLen <= 0 {
			t.Fatalf("content-length %d", gotLen)
		}
	}
}

func TestErrors(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s := serve(t, newState(t, up.URL, 1, time.Second))

	cases := []struct {
		body string
		hdr  map[string]string
		code int
	}{
		{`{"model":"nope"}`, nil, 404},
		{`{"x":1}`, nil, 400},
		{`not json`, nil, 400},
		{`{"model":"fast"}`, map[string]string{"X-Priority": "high"}, 400},
	}
	for _, c := range cases {
		resp := post(t, s.URL+"/v1/chat/completions", c.body, c.hdr)
		var e struct {
			Error struct{ Message, Type string }
		}
		json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != c.code || e.Error.Message == "" || e.Error.Type == "" {
			t.Errorf("%q: status %d err %+v", c.body, resp.StatusCode, e)
		}
	}
}

func TestModelsStatusHealth(t *testing.T) {
	st := newState(t, "http://x", 1, time.Second)
	s := serve(t, st)

	var models struct {
		Object string
		Data   []struct{ ID string }
	}
	get := func(path string, v any) int {
		resp, err := http.Get(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		json.NewDecoder(resp.Body).Decode(v)
		return resp.StatusCode
	}
	get("/v1/models", &models)
	if models.Object != "list" || len(models.Data) != 3 || models.Data[0].ID != "fast" {
		t.Fatalf("models %+v", models)
	}
	var status struct{ Pools map[string]pool.Stats }
	get("/status", &status)
	if status.Pools["p"].MaxConcurrency != 1 {
		t.Fatalf("status %+v", status)
	}
	var h map[string]string
	if get("/healthz", &h) != 200 || h["status"] != "ok" {
		t.Fatalf("health %v", h)
	}

	notReady := httptest.NewServer(New(func() *config.RuntimeState { return nil }, quiet()))
	defer notReady.Close()
	resp, _ := http.Get(notReady.URL + "/healthz")
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("healthz before config = %d", resp.StatusCode)
	}
}

func TestStreamingFlushAndSlotHeldUntilEnd(t *testing.T) {
	proceed := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: one\n\n"))
		w.(http.Flusher).Flush()
		<-proceed
		w.Write([]byte("data: two\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer up.Close()
	st := newState(t, up.URL, 1, time.Second)
	s := serve(t, st)

	resp := post(t, s.URL+"/v1/chat/completions", `{"model":"fast","stream":true}`, nil)
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf) // must arrive before upstream finishes
	if err != nil || !strings.Contains(string(buf[:n]), "data: one") {
		t.Fatalf("first chunk not flushed: %q %v", buf[:n], err)
	}
	if a := st.Pools["p"].Stats().Active; a != 1 {
		t.Fatalf("active = %d mid-stream, want 1", a)
	}

	// A second request must be rejected with a queue timeout while the slot is held.
	st.Config.Server.QueueTimeout = config.Duration(100 * time.Millisecond)
	resp2 := post(t, s.URL+"/v1/chat/completions", `{"model":"slow"}`, nil)
	resp2.Body.Close()
	if resp2.StatusCode != 503 {
		t.Fatalf("queued request status = %d, want 503", resp2.StatusCode)
	}

	close(proceed)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "data: two") {
		t.Fatalf("rest = %q", rest)
	}
	deadline := time.Now().Add(2 * time.Second)
	for st.Pools["p"].Stats().Active != 0 {
		if time.Now().After(deadline) {
			t.Fatal("slot not released after stream end")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPriorityOverrideHeaderOrdersQueue(t *testing.T) {
	release := make(chan struct{})
	order := make(chan string, 8)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Tag string }
		json.NewDecoder(r.Body).Decode(&b)
		order <- b.Tag
		if b.Tag == "hold" {
			<-release
		}
	}))
	defer up.Close()
	st := newState(t, up.URL, 1, 5*time.Second)
	s := serve(t, st)

	go bgPost(s.URL+"/v1/chat/completions", `{"model":"same","tag":"hold"}`, nil)
	if got := <-order; got != "hold" {
		t.Fatal(got)
	}
	// Both routes have equal default (0/1); override via header.
	go bgPost(s.URL+"/v1/chat/completions", `{"model":"slow","tag":"low"}`, nil)
	waitQueue(t, st, 1)
	go bgPost(s.URL+"/v1/chat/completions", `{"model":"slow","tag":"urgent"}`, map[string]string{"X-Priority": "999"})
	waitQueue(t, st, 2)
	close(release)
	if got := <-order; got != "urgent" {
		t.Fatalf("first after release = %s, want urgent", got)
	}
	if got := <-order; got != "low" {
		t.Fatalf("second = %s", got)
	}
}

func waitQueue(t *testing.T, st *config.RuntimeState, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for st.Pools["p"].Stats().QueueDepth != n {
		if time.Now().After(deadline) {
			t.Fatalf("queue depth want %d", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClientCancelWhileQueuedLeavesNoLeak(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer up.Close()
	defer close(release)
	st := newState(t, up.URL, 1, 5*time.Second)
	s := serve(t, st)

	go bgPost(s.URL+"/v1/chat/completions", `{"model":"fast"}`, nil)
	deadline := time.Now().Add(2 * time.Second)
	for st.Pools["p"].Stats().Active != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/v1/chat/completions", strings.NewReader(`{"model":"fast"}`))
	go http.DefaultClient.Do(req)
	waitQueue(t, st, 1)
	cancel()
	for st.Pools["p"].Stats().QueueDepth != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := st.Pools["p"].Stats(); s.QueueDepth != 0 || s.Active != 1 {
		t.Fatalf("stats %+v", s)
	}
}

// bgPost is safe to call from goroutines (never calls t.Fatal).
func bgPost(url, body string, hdr map[string]string) {
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if resp, err := http.DefaultClient.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
