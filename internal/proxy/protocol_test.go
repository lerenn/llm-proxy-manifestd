package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
)

type seen struct {
	hdr  http.Header
	body string
	path string
}

func recorder(tag string, hits *atomic.Int32, status int, last *seen) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		if last != nil {
			*last = seen{r.Header.Clone(), string(b), r.URL.Path}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(tag))
	}))
}

func protoYAML(anth1, anth2, oai string) string {
	return `server: {queue_timeout: '500ms'}
pools:
  c1: {upstream_url: '` + anth1 + `', protocol: anthropic, api_key: 'ak1', max_concurrency: 1}
  c2: {upstream_url: '` + anth2 + `', protocol: anthropic, api_key: 'ak2', max_concurrency: 1}
  o1: {upstream_url: '` + oai + `', api_key: 'ok1', max_concurrency: 1}
endpoints:
  anthropic:
    fast: {priority: 10, targets: [{pool: c1, target_model: 'haiku@low'}, {pool: c2, target_model: 'haiku@low'}]}
    aonly: {targets: [{pool: c1}]}
  openai:
    fast: {priority: 10, targets: [{pool: o1, target_model: 'luna@low'}]}
    oonly: {targets: [{pool: o1, target_model: gpt}]}
`
}

func protoLoader(t *testing.T, y string) (*config.Loader, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	l := config.NewLoader(p, quiet())
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	return l, p
}

func TestProtocolRoutingAndAuth(t *testing.T) {
	var ha, hb, ho atomic.Int32
	var sa, so seen
	a, b, o := recorder("A", &ha, 200, &sa), recorder("B", &hb, 200, nil), recorder("O", &ho, 200, &so)
	defer a.Close()
	defer b.Close()
	defer o.Close()
	l, _ := protoLoader(t, protoYAML(a.URL, b.URL, o.URL))
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		r := post(t, srv.URL+path, `{"model":"fast","stream":true}`, map[string]string{
			"x-api-key": "client", "Authorization": "Bearer client", "anthropic-version": "2023-06-01", "anthropic-beta": "tools-2024",
		})
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || r.Header.Get("X-Pool-Name") != "c1" {
			t.Fatalf("%s: %d %q", path, r.StatusCode, r.Header.Get("X-Pool-Name"))
		}
		if sa.path != path || !strings.Contains(sa.body, `"model":"haiku@low"`) {
			t.Fatalf("%s: %+v", path, sa)
		}
		h := sa.hdr
		if h.Get("X-Api-Key") != "ak1" || h.Get("Authorization") != "" ||
			h.Get("Anthropic-Version") != "2023-06-01" || h.Get("Anthropic-Beta") != "tools-2024" {
			t.Fatalf("anthropic headers: %v", h)
		}
	}
	r := post(t, srv.URL+"/v1/chat/completions", `{"model":"fast"}`, map[string]string{"x-api-key": "client", "Authorization": "Bearer client"})
	r.Body.Close()
	if r.Header.Get("X-Pool-Name") != "o1" || !strings.Contains(so.body, `"model":"luna@low"`) {
		t.Fatalf("openai: %q %+v", r.Header.Get("X-Pool-Name"), so)
	}
	if so.hdr.Get("Authorization") != "Bearer ok1" || so.hdr.Get("X-Api-Key") != "" {
		t.Fatalf("openai headers: %v", so.hdr)
	}
	if hb.Load() != 0 {
		t.Fatal("pool c2 should be unused")
	}
}

func TestMissingEndpointErrorShapes(t *testing.T) {
	var h atomic.Int32
	u := recorder("x", &h, 200, nil)
	defer u.Close()
	l, _ := protoLoader(t, protoYAML(u.URL, u.URL, u.URL))
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()

	r := post(t, srv.URL+"/v1/messages", `{"model":"oonly"}`, nil)
	var a struct {
		Type  string
		Error struct{ Type, Message string }
	}
	_ = json.NewDecoder(r.Body).Decode(&a)
	r.Body.Close()
	if r.StatusCode != 404 || a.Type != "error" || a.Error.Type != "not_found_error" || !strings.Contains(a.Error.Message, "anthropic") {
		t.Fatalf("anthropic error: %d %+v", r.StatusCode, a)
	}
	r = post(t, srv.URL+"/v1/chat/completions", `{"model":"aonly"}`, nil)
	var o struct {
		Error struct{ Type, Code, Message string }
	}
	_ = json.NewDecoder(r.Body).Decode(&o)
	r.Body.Close()
	if r.StatusCode != 404 || o.Error.Code != "endpoint_not_configured" {
		t.Fatalf("openai error: %d %+v", r.StatusCode, o)
	}
	// Unknown model and bad body on the Anthropic API use its shape too.
	for _, body := range []string{`{"model":"nope"}`, `{}`} {
		r = post(t, srv.URL+"/v1/messages", body, nil)
		var e map[string]any
		_ = json.NewDecoder(r.Body).Decode(&e)
		r.Body.Close()
		if e["type"] != "error" || r.StatusCode < 400 {
			t.Fatalf("%s: %d %v", body, r.StatusCode, e)
		}
	}
	if h.Load() != 0 {
		t.Fatal("no request should reach an upstream")
	}
}

func TestModelsPerProtocol(t *testing.T) {
	var h atomic.Int32
	u := recorder("x", &h, 200, nil)
	defer u.Close()
	l, _ := protoLoader(t, protoYAML(u.URL, u.URL, u.URL))
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()
	get1 := func(hdr map[string]string) string {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	oai := get1(nil)
	if !strings.Contains(oai, `"fast"`) || !strings.Contains(oai, `"oonly"`) || strings.Contains(oai, `"aonly"`) {
		t.Fatalf("openai list: %s", oai)
	}
	var ol struct {
		Object string
		Data   []struct {
			ID, Object string
			OwnedBy    string `json:"owned_by"`
		}
	}
	if err := json.Unmarshal([]byte(oai), &ol); err != nil || ol.Object != "list" || len(ol.Data) != 2 ||
		ol.Data[0].ID != "fast" || ol.Data[1].ID != "oonly" || ol.Data[0].Object != "model" || ol.Data[0].OwnedBy == "" {
		t.Fatalf("openai list: %s", oai)
	}
	for _, hdr := range []map[string]string{{"anthropic-version": "2023-06-01"}, {"x-api-key": "k"}, {"x-api-key": "k", "Authorization": "Bearer k"}} {
		ant := get1(hdr)
		var al struct {
			Data []struct {
				Type, ID    string
				DisplayName string `json:"display_name"`
				CreatedAt   string `json:"created_at"`
			}
			HasMore bool   `json:"has_more"`
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal([]byte(ant), &al); err != nil || len(al.Data) != 2 || al.Data[0].ID != "aonly" || al.Data[1].ID != "fast" ||
			al.Data[0].Type != "model" || al.Data[0].DisplayName != "aonly" || al.Data[0].CreatedAt == "" ||
			al.HasMore || al.FirstID != "aonly" || al.LastID != "fast" || strings.Contains(ant, `"oonly"`) {
			t.Fatalf("anthropic list (%v): %s", hdr, ant)
		}
	}
}

func TestModelsEmptyAnthropicList(t *testing.T) {
	cfg, err := config.Parse([]byte("pools:\n  o: {upstream_url: 'http://x', max_concurrency: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(func() *config.RuntimeState { return &config.RuntimeState{Config: cfg} }, quiet())
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if b := rec.Body.String(); rec.Code != 200 || !strings.Contains(b, `"data":[]`) || !strings.Contains(b, `"first_id":null`) {
		t.Fatalf("%d %s", rec.Code, b)
	}
}

func TestAnthropicFailoverWithinEndpoint(t *testing.T) {
	var ha, hb, ho atomic.Int32
	a, b, o := recorder("A", &ha, 503, nil), recorder("B", &hb, 200, nil), recorder("O", &ho, 200, nil)
	defer a.Close()
	defer b.Close()
	defer o.Close()
	l, _ := protoLoader(t, protoYAML(a.URL, b.URL, o.URL))
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()
	r := post(t, srv.URL+"/v1/messages", `{"model":"fast"}`, nil)
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("X-Pool-Name") != "c2" || ha.Load() != 1 || ho.Load() != 0 {
		t.Fatalf("%d %q a=%d o=%d", r.StatusCode, r.Header.Get("X-Pool-Name"), ha.Load(), ho.Load())
	}
}

func TestEndpointsHotReloadAndDashboard(t *testing.T) {
	var h atomic.Int32
	u := recorder("x", &h, 200, nil)
	defer u.Close()
	y := protoYAML(u.URL, u.URL, u.URL)
	l, p := protoLoader(t, y)
	hd := New(l.State, quiet())
	srv := httptest.NewServer(hd)
	defer srv.Close()

	r := post(t, srv.URL+"/v1/messages", `{"model":"oonly"}`, nil)
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("want 404, got %d", r.StatusCode)
	}
	// Reload adds an anthropic endpoint with the same name.
	y2 := strings.Replace(y, "    aonly: {targets: [{pool: c1}]}",
		"    aonly: {targets: [{pool: c1}]}\n    oonly: {targets: [{pool: c1}]}", 1)
	if err := os.WriteFile(p, []byte(y2), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	r = post(t, srv.URL+"/v1/messages", `{"model":"oonly"}`, nil)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("after reload: %d", r.StatusCode)
	}
	// An invalid reload (protocol mismatch) keeps the previous state.
	prev := l.State()
	_ = os.WriteFile(p, []byte(strings.Replace(y2, "oonly: {targets: [{pool: c1}]}", "oonly: {targets: [{pool: o1}]}", 1)), 0o644)
	if l.Reload() == nil || l.State() != prev {
		t.Fatal("invalid reload must be rejected")
	}

	body := get(t, hd, "proxy.test", "/dashboard/api/state").Body.String()
	for _, want := range []string{`"endpoints":[{"protocol":"anthropic","name":"aonly"`, `"protocol":"anthropic","name":"oonly"`, `"protocol":"openai","name":"oonly"`, `{"pool":"c1","upstream"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in %s", want, body)
		}
	}
	for _, bad := range []string{"ak1", "ok1", "ak2", "http://"} {
		if strings.Contains(body, bad) {
			t.Errorf("dashboard leaks %q", bad)
		}
	}
}
