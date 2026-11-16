package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
	"github.com/lerenn/llm-proxy-manifestd/internal/pool"
)

const secretYAML = `
server: {queue_timeout: "5s"}
dashboard: %s
pools:
  p: {upstream_url: "https://user:hunter2pass@api.example.com:8443/v1/secretpath?token=qtoken", api_key: "sk-SUPERSECRET", max_concurrency: 2}
endpoints:
  openai:
    fast: {priority: 100, targets: [{pool: p, target_model: real-model}]}
`

func dashState(t *testing.T, dash string) *config.RuntimeState {
	t.Helper()
	cfg, err := config.Parse([]byte(strings.Replace(secretYAML, "%s", dash, 1)))
	if err != nil {
		t.Fatal(err)
	}
	return &config.RuntimeState{Config: cfg, Generation: 3, LoadedAt: time.Now(),
		Pools: map[string]*pool.Pool{"p": pool.New(2, 0)}}
}

func get(t *testing.T, h http.Handler, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDashboardNoSecrets(t *testing.T) {
	st := dashState(t, "{}")
	h := New(func() *config.RuntimeState { return st }, quiet(), WithVersion("1.2.3"),
		WithReloadStatus(func() config.ReloadStatus { return config.ReloadStatus{LastAttempt: time.Now(), Failed: true} }))
	var all strings.Builder
	for _, c := range []struct{ host, path string }{
		{"dashboard.x.test", "/api/state"}, {"dashboard.x.test", "/"}, {"dashboard.x.test", "/app.js"},
		{"proxy.test", "/dashboard/api/state"}, {"proxy.test", "/dashboard/"},
	} {
		rec := get(t, h, c.host, c.path)
		if rec.Code != 200 {
			t.Fatalf("%v: %d", c, rec.Code)
		}
		all.WriteString(rec.Body.String())
	}
	body := all.String()
	for _, bad := range []string{"SUPERSECRET", "hunter2", "secretpath", "qtoken", "user:", "https://", "/v1/"} {
		if strings.Contains(body, bad) {
			t.Errorf("dashboard output leaks %q", bad)
		}
	}
	for _, want := range []string{"api.example.com:8443", "fast", "1.2.3", `"last_reload_failed":true`, `"status":"degraded"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestDashboardRouting(t *testing.T) {
	st := dashState(t, "{}")
	h := New(func() *config.RuntimeState { return st }, quiet())

	if rec := get(t, h, "proxy.test", "/v1/models"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"fast"`) {
		t.Errorf("/v1/models shadowed: %d %s", rec.Code, rec.Body)
	}
	if rec := get(t, h, "proxy.test", "/"); rec.Code != 404 {
		t.Errorf("dashboard must not claim / on other hosts: %d", rec.Code)
	}
	if rec := get(t, h, "dashboard.x.test:8080", "/v1/models"); rec.Code != 404 {
		t.Errorf("proxy API reachable on dashboard host: %d", rec.Code)
	}
	if rec := get(t, h, "dashboard.x.test", "/healthz"); rec.Code != 404 {
		t.Errorf("/healthz on dashboard host: %d", rec.Code)
	}
	if rec := get(t, h, "proxy.test", "/dashboard"); rec.Code != 301 {
		t.Errorf("want redirect, got %d", rec.Code)
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	req.Host = "dashboard.x.test"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Errorf("POST on dashboard host: %d", rec.Code)
	}
	if csp := get(t, h, "dashboard.x.test", "/").Header().Get("Content-Security-Policy"); csp == "" {
		t.Error("missing CSP")
	}
}

func TestDashboardConfigOptions(t *testing.T) {
	// Explicit host: "dashboard." prefix no longer matches, exact host does.
	st := dashState(t, `{host: "Stats.Example.com", path: "off"}`)
	h := New(func() *config.RuntimeState { return st }, quiet())
	if get(t, h, "stats.example.com", "/").Code != 200 {
		t.Error("explicit host should serve")
	}
	if get(t, h, "dashboard.x.test", "/").Code != 404 {
		t.Error("prefix host should not match when host set")
	}
	if get(t, h, "proxy.test", "/dashboard/").Code != 404 {
		t.Error("path mount should be off")
	}

	st = dashState(t, `{enabled: false}`)
	h = New(func() *config.RuntimeState { return st }, quiet())
	if get(t, h, "dashboard.x.test", "/").Code != 404 || get(t, h, "proxy.test", "/dashboard/").Code != 404 {
		t.Error("disabled dashboard must not serve")
	}

	for _, bad := range []string{`{path: "/v1/ui"}`, `{path: "ui"}`, `{path: "/status"}`} {
		if _, err := config.Parse([]byte(strings.Replace(secretYAML, "%s", bad, 1))); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
}

func TestDashboardLiveCounters(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(500)
	}))
	defer up.Close()
	st := newState(t, up.URL, 1, time.Second)
	s := serve(t, st)
	post(t, s.URL+"/v1/chat/completions", `{"model":"fast"}`, nil).Body.Close()

	resp, err := http.Get(s.URL + "/dashboard/api/state")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"requests":1,"errors":1`) {
		t.Errorf("counters not recorded: %s", b)
	}
}

func TestDashboardUnlimitedPool(t *testing.T) {
	cfg, err := config.Parse([]byte("pools:\n  u: {upstream_url: 'https://user:hunter2pass@h.example.com/v1', api_key: 'sk-SUPERSECRET', max_concurrency: 0}\n  l: {upstream_url: 'http://l', max_concurrency: 2}\nroutes:\n  r: {pools: [u, l], target_model: m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	st := &config.RuntimeState{Config: cfg, Generation: 1, LoadedAt: time.Now(),
		Pools: map[string]*pool.Pool{"u": pool.New(0, 0), "l": pool.New(2, 0)}}
	h := New(func() *config.RuntimeState { return st }, quiet())
	rec := get(t, h, "proxy.test", "/dashboard/api/state")
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"unlimited":true`, `"slots":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	for _, bad := range []string{"SUPERSECRET", "hunter2"} {
		if strings.Contains(body, bad) {
			t.Errorf("leak %q", bad)
		}
	}
}
