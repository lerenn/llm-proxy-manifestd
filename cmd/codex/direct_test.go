package main

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fakeJWT(exp int64) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none"}`) + "." + enc(`{"exp":`+strconv.FormatInt(exp, 10)+`}`) + ".sig"
}

func TestResponsesPassthroughKeepsToolsAndEffort(t *testing.T) {
	var gotAuth, gotAcct, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotAuth, gotAcct, gotBody = r.Header.Get("Authorization"), r.Header.Get("chatgpt-account-id"), string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {}\n\n"))
	}))
	defer up.Close()
	home := t.TempDir()
	tok := fakeJWT(time.Now().Add(time.Hour).Unix())
	_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"access_token":"`+tok+`","refresh_token":"r","account_id":"acct"}}`), 0o600)
	s := &Server{CodexHome: home, upstream: up.URL}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"gpt-6.1-sol@high","input":"hi","tools":[{"type":"function","name":"f","parameters":{}}]}`))
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("code=%d body=%q", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer "+tok || gotAcct != "acct" {
		t.Fatalf("headers auth=%q acct=%q", gotAuth, gotAcct)
	}
	for _, want := range []string{`"model":"gpt-6.1-sol"`, `"effort":"high"`, `"store":false`, `"stream":true`, `"name":"f"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("upstream body missing %s: %s", want, gotBody)
		}
	}
}

func TestTokenRefreshWhenExpired(t *testing.T) {
	newTok := fakeJWT(time.Now().Add(time.Hour).Unix())
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "old-r" || r.Form.Get("client_id") != codexClientID {
			http.Error(w, "bad", 400)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"` + newTok + `","refresh_token":"new-r"}`))
	}))
	defer tsrv.Close()
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"OPENAI_API_KEY":null,"tokens":{"access_token":"`+fakeJWT(1)+`","refresh_token":"old-r","account_id":"a"}}`), 0o600)
	ts := newTokenSource(home)
	ts.tokURL = tsrv.URL
	got, acct, err := ts.Token(context.Background())
	if err != nil || got != newTok || acct != "a" {
		t.Fatalf("got=%q acct=%q err=%v", got, acct, err)
	}
	saved, _ := os.ReadFile(filepath.Join(home, "auth.json"))
	if !strings.Contains(string(saved), "new-r") || !strings.Contains(string(saved), "OPENAI_API_KEY") {
		t.Fatalf("auth.json not rotated/preserved: %s", saved)
	}
}
