package common

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSplitModel(t *testing.T) {
	for _, c := range []struct {
		in, m, e string
		bad      bool
	}{
		{"claude-sonnet-5-5@medium", "claude-sonnet-5-5", "medium", false},
		{"claude-haiku-5-5", "claude-haiku-5-5", "", false},
		{"x@ultra", "", "", true},
		{"@low", "", "", true},
	} {
		m, e, err := SplitModel(c.in)
		if (err != nil) != c.bad || m != c.m || e != c.e {
			t.Errorf("%q: got %q %q %v", c.in, m, e, err)
		}
	}
}

func TestRequireToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	do := func(h http.Handler, method, path, auth string) int {
		r := httptest.NewRequest(method, path, nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	h := RequireToken("s3cret", ok)
	for _, c := range []struct {
		method, path, auth string
		want               int
	}{
		{"POST", "/v1/messages", "", 401},
		{"POST", "/v1/messages", "Bearer wrong", 401},
		{"POST", "/v1/messages", "s3cret", 401},
		{"POST", "/v1/messages", "Bearer s3cret", 200},
		{"GET", "/ready", "", 200},
		{"POST", "/ready", "", 401},
	} {
		if got := do(h, c.method, c.path, c.auth); got != c.want {
			t.Errorf("%s %s %q: got %d want %d", c.method, c.path, c.auth, got, c.want)
		}
	}
	if got := do(RequireToken("", ok), "POST", "/v1/messages", ""); got != 200 {
		t.Errorf("empty token must disable the check, got %d", got)
	}
}
