package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
	"github.com/lerenn/llm-proxy-manifestd/internal/pool"
)

func keyDir(t *testing.T, keys map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for n, k := range keys {
		if err := os.WriteFile(filepath.Join(d, n), []byte(k+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func authState(t *testing.T, upstream, dir, clients string, max int) *config.RuntimeState {
	t.Helper()
	yml := "server: {queue_timeout: 5s, client_keys_dir: '" + dir + "'}\n" +
		"pools:\n  p: {upstream_url: '" + upstream + "', max_concurrency: " + string(rune('0'+max)) + "}\n" +
		"  a: {upstream_url: '" + upstream + "', protocol: anthropic, max_concurrency: 1}\n" +
		"endpoints:\n  openai:\n    fast: {priority: 5, targets: [{pool: p, target_model: m}]}\n    slow: {targets: [{pool: p, target_model: m}]}\n" +
		"  anthropic:\n    claude: {targets: [{pool: a, target_model: c}]}\n" +
		"clients:\n" + clients
	cfg, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	return &config.RuntimeState{Config: cfg, Generation: 1, LoadedAt: time.Now(),
		Pools: map[string]*pool.Pool{"p": pool.New(max, 0), "a": pool.New(1, 0)}}
}

func ok200() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true}`) }))
}

func do(t *testing.T, method, url, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestClientKeyAuth(t *testing.T) {
	up := ok200()
	defer up.Close()
	dir := keyDir(t, map[string]string{"opencode": "llmp_secret-one", "ci": "llmp_secret-two", "orphan": "llmp_not-in-config"})
	st := authState(t, up.URL, dir, "  opencode:\n  ci: {priority: 9}\n  nokey:\n", 2)
	srv := serve(t, st)
	body := `{"model":"fast"}`
	msg := `{"model":"claude"}`

	cases := []struct {
		name, path, body string
		hdr              map[string]string
		want             int
	}{
		{"bearer ok", "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer llmp_secret-one"}, 200},
		{"x-api-key ok", "/v1/messages", msg, map[string]string{"X-Api-Key": "llmp_secret-two"}, 200},
		{"second client", "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer llmp_secret-two"}, 200},
		{"missing", "/v1/chat/completions", body, nil, 401},
		{"wrong bearer", "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer nope"}, 401},
		{"wrong x-api-key", "/v1/messages", msg, map[string]string{"X-Api-Key": "nope"}, 401},
		{"key of a client missing from config", "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer llmp_not-in-config"}, 401},
		{"client without a key file", "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer "}, 401},
	}
	for _, c := range cases {
		if code, b := do(t, "POST", srv.URL+c.path, c.body, c.hdr); code != c.want {
			t.Errorf("%s: %d %s", c.name, code, b)
		}
	}
	// Error shapes follow the protocol.
	if _, b := do(t, "POST", srv.URL+"/v1/messages", msg, nil); !strings.Contains(b, `"type":"error"`) || !strings.Contains(b, "authentication_error") {
		t.Errorf("anthropic 401 shape: %s", b)
	}
	if _, b := do(t, "POST", srv.URL+"/v1/chat/completions", body, nil); !strings.Contains(b, `"code":"invalid_api_key"`) {
		t.Errorf("openai 401 shape: %s", b)
	}
	// Open endpoints.
	if code, _ := do(t, "GET", srv.URL+"/healthz", "", nil); code != 200 {
		t.Errorf("healthz must stay open: %d", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/models", "", nil); code != 401 {
		t.Errorf("models must require a key: %d", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/status", "", nil); code != 401 {
		t.Errorf("status must require a key: %d", code)
	}
}

func TestClientKeyDisabledWhenNoClients(t *testing.T) {
	up := ok200()
	defer up.Close()
	st := newState(t, up.URL, 2, time.Second)
	srv := serve(t, st)
	if code, _ := do(t, "POST", srv.URL+"/v1/chat/completions", `{"model":"same"}`, nil); code != 200 {
		t.Fatalf("auth must be off without clients: %d", code)
	}
}

func TestClientAllowListAndModels(t *testing.T) {
	up := ok200()
	defer up.Close()
	dir := keyDir(t, map[string]string{"limited": "k1", "all": "k2"})
	st := authState(t, up.URL, dir, "  limited: {endpoints: [fast]}\n  all:\n", 2)
	srv := serve(t, st)
	h1 := map[string]string{"Authorization": "Bearer k1"}
	if code, _ := do(t, "POST", srv.URL+"/v1/chat/completions", `{"model":"fast"}`, h1); code != 200 {
		t.Errorf("allowed endpoint: %d", code)
	}
	if code, b := do(t, "POST", srv.URL+"/v1/chat/completions", `{"model":"slow"}`, h1); code != 403 || !strings.Contains(b, "model_not_allowed") {
		t.Errorf("forbidden endpoint: %d %s", code, b)
	}
	if _, b := do(t, "GET", srv.URL+"/v1/models", "", h1); !strings.Contains(b, `"fast"`) || strings.Contains(b, `"slow"`) {
		t.Errorf("models must be filtered: %s", b)
	}
	if _, b := do(t, "GET", srv.URL+"/v1/models", "", map[string]string{"Authorization": "Bearer k2"}); !strings.Contains(b, `"slow"`) {
		t.Errorf("unrestricted client sees all: %s", b)
	}
}

func TestClientKeyHotReloadAndDashboard(t *testing.T) {
	up := ok200()
	defer up.Close()
	dir := keyDir(t, map[string]string{"a": "key-a"})
	st := authState(t, up.URL, dir, "  a:\n  b:\n", 2)
	h := New(func() *config.RuntimeState { return st }, quiet())
	srv := httptest.NewServer(h)
	defer srv.Close()
	hb := map[string]string{"Authorization": "Bearer key-b"}
	if code, _ := do(t, "POST", srv.URL+"/v1/chat/completions", `{"model":"fast"}`, hb); code != 401 {
		t.Fatalf("b has no key yet: %d", code)
	}
	os.WriteFile(filepath.Join(dir, "b"), []byte("key-b"), 0o600)
	h.keys.mu.Lock()
	h.keys.loadedAt = time.Now().Add(-time.Minute) // force the periodic refresh
	h.keys.mu.Unlock()
	if code, _ := do(t, "POST", srv.URL+"/v1/chat/completions", `{"model":"fast"}`, hb); code != 200 {
		t.Fatalf("b key added without restart: %d", code)
	}
	do(t, "POST", srv.URL+"/v1/chat/completions", `{"model":"fast"}`, map[string]string{"Authorization": "Bearer key-a"})

	// Dashboard: names and counts, never keys.
	st.Config.Dashboard.Host = "dash.test"
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "dash.test"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `"auth_enabled":true`) || !strings.Contains(body, `"name":"a","requests":1`) || !strings.Contains(body, `"name":"b","requests":1`) {
		t.Fatalf("dashboard: %d %s", rec.Code, body)
	}
	for _, bad := range []string{"key-a", "key-b", dir} {
		if strings.Contains(body, bad) {
			t.Errorf("dashboard leaks %q", bad)
		}
	}
}

func TestClientPriorityDefault(t *testing.T) {
	// One slot; a blocker holds it, then a low-priority client and a
	// high-priority client queue (low first). The high-priority one must be
	// served first. X-Priority overrides the client default.
	var mu sync.Mutex
	var order []string
	release := make(chan struct{})
	first := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := r.Header.Get("X-Who")
		mu.Lock()
		order = append(order, who)
		mu.Unlock()
		if who == "blocker" {
			first <- struct{}{}
			<-release
		}
		io.WriteString(w, "{}")
	}))
	defer up.Close()
	dir := keyDir(t, map[string]string{"lo": "klo", "hi": "khi", "blk": "kblk"})
	st := authState(t, up.URL, dir, "  lo: {priority: 1}\n  hi: {priority: 50}\n  blk:\n", 1)
	srv := serve(t, st)

	var wg sync.WaitGroup
	send := func(key, who string, extra map[string]string) {
		defer wg.Done()
		hdr := map[string]string{"Authorization": "Bearer " + key, "X-Who": who}
		for k, v := range extra {
			hdr[k] = v
		}
		do(t, "POST", srv.URL+"/v1/chat/completions", `{"model":"slow"}`, hdr)
	}
	wg.Add(1)
	go send("kblk", "blocker", nil)
	<-first
	wg.Add(1)
	go send("klo", "lo", nil)
	time.Sleep(100 * time.Millisecond)
	wg.Add(1)
	go send("khi", "hi", nil)
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "blocker,hi,lo" {
		t.Fatalf("order %v (client priority not applied)", order)
	}
}

func TestClientAllowListAnthropicEndpoint(t *testing.T) {
	up := ok200()
	defer up.Close()
	dir := keyDir(t, map[string]string{"cc": "k3"})
	st := authState(t, up.URL, dir, "  cc: {endpoints: [claude]}\n", 2)
	srv := serve(t, st)
	h := map[string]string{"x-api-key": "k3", "anthropic-version": "2023-06-01"}
	if code, b := do(t, "POST", srv.URL+"/v1/messages", `{"model":"claude"}`, h); code != 200 {
		t.Errorf("allowed anthropic endpoint: %d %s", code, b)
	}
	if code, b := do(t, "POST", srv.URL+"/v1/messages", `{"model":"fast"}`, h); code != 404 {
		t.Errorf("fast has no anthropic endpoint: %d %s", code, b)
	}
	if code, b := do(t, "POST", srv.URL+"/v1/chat/completions", `{"model":"fast"}`, map[string]string{"Authorization": "Bearer k3"}); code != 403 {
		t.Errorf("fast not in allow-list: %d %s", code, b)
	}
	if _, b := do(t, "GET", srv.URL+"/v1/models", "", h); !strings.Contains(b, `"claude"`) {
		t.Errorf("anthropic models: %s", b)
	}
}
