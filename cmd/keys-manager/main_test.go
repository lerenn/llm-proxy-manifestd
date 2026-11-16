package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fake struct {
	mu        sync.Mutex
	sec       *secret
	conflicts int
	getCode   int
	writes    int
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(401)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if f.getCode != 0 {
			w.WriteHeader(f.getCode)
			return
		}
		if f.sec == nil {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(f.sec)
	case http.MethodPost, http.MethodPut:
		f.writes++
		if f.conflicts > 0 {
			f.conflicts--
			w.WriteHeader(409)
			return
		}
		var s secret
		json.NewDecoder(r.Body).Decode(&s)
		f.sec = &s
		w.WriteHeader(200)
	}
}

func runWith(t *testing.T, f *fake, clients string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d := t.TempDir()
	cfg := "pools:\n  p: {upstream_url: 'http://x', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: p}]}\nserver:\n  client_keys_dir: /k\nclients:\n" + clients
	cp, tp := filepath.Join(d, "c.yaml"), filepath.Join(d, "tok")
	os.WriteFile(cp, []byte(cfg), 0o600)
	os.WriteFile(tp, []byte("tok\n"), 0o600)
	var so, se bytes.Buffer
	code := run([]string{"init", "--config", cp, "-secret", "ks", "-namespace", "ns", "-api", srv.URL,
		"-token-file", tp, "-ca-file", "/nonexistent", "-label", "a=b"}, &so, &se)
	return code, so.String() + se.String()
}

func dec(t *testing.T, s *secret, n string) string {
	b, err := base64.StdEncoding.DecodeString(s.Data[n])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFreshCreate(t *testing.T) {
	f := &fake{}
	code, out := runWith(t, f, "  a:\n  b: {}\n")
	if code != 0 || f.sec == nil || len(f.sec.Data) != 2 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if f.sec.Type != "Opaque" || f.sec.Metadata.Labels["a"] != "b" {
		t.Fatalf("bad secret %+v", f.sec)
	}
	k := dec(t, f.sec, "a")
	if !strings.HasPrefix(k, "llmp_") || len(k) != 45 || strings.Contains(out, k) {
		t.Fatalf("bad key/log: %q %s", k, out)
	}
}

func TestAddMissingKeepExistingDeleteRemoved(t *testing.T) {
	old := base64.StdEncoding.EncodeToString([]byte("llmp_old"))
	f := &fake{sec: &secret{Metadata: meta{Name: "ks", ResourceVersion: "7"},
		Data: map[string]string{"a": old, "gone": old}}}
	code, out := runWith(t, f, "  a:\n  b:\n")
	if code != 0 {
		t.Fatal(out)
	}
	if dec(t, f.sec, "a") != "llmp_old" {
		t.Fatal("existing key overwritten")
	}
	if _, ok := f.sec.Data["gone"]; ok {
		t.Fatal("removed client kept")
	}
	if f.sec.Data["b"] == "" || f.sec.Metadata.ResourceVersion != "7" {
		t.Fatalf("missing b or resourceVersion: %+v", f.sec)
	}
}

func TestIdempotent(t *testing.T) {
	f := &fake{}
	runWith(t, f, "  a:\n")
	w := f.writes
	k := f.sec.Data["a"]
	if code, out := runWith(t, f, "  a:\n"); code != 0 || f.writes != w || f.sec.Data["a"] != k {
		t.Fatalf("not idempotent: %s", out)
	}
}

func TestConflictRetry(t *testing.T) {
	f := &fake{conflicts: 2}
	if code, out := runWith(t, f, "  a:\n"); code != 0 || f.writes != 3 || f.sec == nil {
		t.Fatalf("code=%d writes=%d %s", code, f.writes, out)
	}
}

func TestConflictGivesUp(t *testing.T) {
	f := &fake{conflicts: 100}
	if code, _ := runWith(t, f, "  a:\n"); code != 1 {
		t.Fatal("want exit 1")
	}
}

func TestAPIError(t *testing.T) {
	f := &fake{getCode: 403}
	if code, out := runWith(t, f, "  a:\n"); code != 1 || !strings.Contains(out, "403") {
		t.Fatalf("code=%d %s", code, out)
	}
}

func TestUsage(t *testing.T) {
	for _, a := range [][]string{nil, {"bogus"}, {"-f", "x"}} {
		var o bytes.Buffer
		if run(a, &o, &o) != 2 || !strings.Contains(o.String(), "usage") {
			t.Fatalf("args %v: %s", a, o.String())
		}
	}
}
