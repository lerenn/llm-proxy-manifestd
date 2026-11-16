package proxy

import (
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func coordYAML(url, addr, mode string) string {
	return `server: {queue_timeout: '2s'}
coordination:
  valkey: {address: '` + addr + `', fail_mode: ` + mode + `, lease_ttl: 5s}
pools:
  p: {upstream_url: '` + url + `', max_concurrency: 1}
endpoints:
  openai:
    m: {targets: [{pool: p}]}
`
}

func TestCoordinationSharedAndFailModes(t *testing.T) {
	var hits atomic.Int32
	var last seen
	up := recorder("U", &hits, 200, &last)
	defer up.Close()
	m := miniredis.RunT(t)
	addr := m.Addr()

	l, _ := protoLoader(t, coordYAML(up.URL, addr, "closed"))
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()
	if s := l.State().Pools["p"].Stats(); !s.Coordinated {
		t.Fatalf("pool should be coordinated: %+v", s)
	}
	r := post(t, srv.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}

	m.Close()
	r = post(t, srv.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 503 || !strings.Contains(string(b), "coordination_unavailable") {
		t.Fatalf("fail-closed: %d %s", r.StatusCode, b)
	}

	l2, _ := protoLoader(t, coordYAML(up.URL, addr, "open"))
	srv2 := httptest.NewServer(New(l2.State, quiet()))
	defer srv2.Close()
	r = post(t, srv2.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("fail-open should fall back to local: %d", r.StatusCode)
	}
}
