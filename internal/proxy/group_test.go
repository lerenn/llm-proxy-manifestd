package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func groupYAML(url string, max string) string {
	return `server: {queue_timeout: '2s'}
pools:
  nm-anthropic: {upstream_url: '` + url + `', protocol: anthropic, auth: bearer, api_key: 'nmk', max_concurrency: ` + max + `, concurrency_group: nm}
  nm-openai: {upstream_url: '` + url + `', protocol: openai, api_key: 'nmk', max_concurrency: ` + max + `, concurrency_group: nm}
  solo: {upstream_url: '` + url + `', max_concurrency: 1}
endpoints:
  anthropic:
    m: {targets: [{pool: nm-anthropic, target_model: x}]}
  openai:
    m: {targets: [{pool: nm-openai, target_model: x}]}
`
}

func TestGroupSharesSlotAcrossProtocolsAndBearerAuth(t *testing.T) {
	var hits atomic.Int32
	var last seen
	hold := make(chan struct{})
	up := recorder("U", &hits, 200, &last)
	defer up.Close()
	l, path := protoLoader(t, groupYAML(up.URL, "1"))
	st := l.State()
	if st.Pools["nm-anthropic"] != st.Pools["nm-openai"] || st.Pools["solo"] == st.Pools["nm-openai"] {
		t.Fatal("group members must share one pool instance, others not")
	}
	srv := httptest.NewServer(New(l.State, quiet()))
	defer srv.Close()

	// Anthropic request: bearer auth, x-api-key stripped.
	r := post(t, srv.URL+"/v1/messages", `{"model":"m"}`, map[string]string{"x-api-key": "client"})
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if last.hdr.Get("Authorization") != "Bearer nmk" || last.hdr.Get("X-Api-Key") != "" {
		t.Fatalf("headers: %v", last.hdr)
	}

	// Hold the group's slot via the anthropic pool; the openai request must queue.
	rel, err := st.Pools["nm-anthropic"].Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int)
	go func() {
		r := post(t, srv.URL+"/v1/chat/completions", `{"model":"m"}`, nil)
		r.Body.Close()
		done <- r.StatusCode
	}()
	time.Sleep(200 * time.Millisecond)
	if s := st.Pools["nm-openai"].Stats(); s.Active != 1 || s.QueueDepth != 1 {
		t.Fatalf("stats %+v", s)
	}
	// /status and dashboard report the group.
	sr := httptest.NewRecorder()
	New(l.State, quiet()).ServeHTTP(sr, httptest.NewRequest("GET", "/status", nil))
	var status struct {
		Pools map[string]struct {
			Group string `json:"concurrency_group"`
			Queue int    `json:"queue_depth"`
		} `json:"pools"`
	}
	if err := json.Unmarshal(sr.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Pools["nm-anthropic"].Group != "nm" || status.Pools["nm-openai"].Queue != 1 || status.Pools["solo"].Group != "" {
		t.Fatalf("status %s", sr.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatal("queued request reached upstream early")
	}
	rel()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("status %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request not served after release")
	}
	close(hold)

	// Hot reload: resize keeps the shared instance; dropping a member keeps the group.
	_ = os.WriteFile(path, []byte(groupYAML(up.URL, "3")), 0o644)
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	st2 := l.State()
	if st2.Pools["nm-anthropic"] != st.Pools["nm-anthropic"] || st2.Pools["nm-openai"] != st.Pools["nm-anthropic"] {
		t.Fatal("group instance should survive reload")
	}
	if s := st2.Pools["nm-openai"].Stats(); s.MaxConcurrency != 3 {
		t.Fatalf("stats %+v", s)
	}
	y := strings.Replace(groupYAML(up.URL, "3"), "  nm-openai:", "  #nm-openai:", 1)
	y = strings.Replace(y, "  openai:\n    m: {targets: [{pool: nm-openai, target_model: x}]}\n", "", 1)
	_ = os.WriteFile(path, []byte(y), 0o644)
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	if l.State().Pools["nm-anthropic"] != st.Pools["nm-anthropic"] {
		t.Fatal("group instance should survive member removal")
	}
}
