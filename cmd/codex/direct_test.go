package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

func TestResponsesLongToolNameRoundTripsAcrossContinuation(t *testing.T) {
	long := "mcp__codex_apps__codex_document_control___execute_document_command"
	if len(long) <= 64 {
		t.Fatalf("fixture length=%d, need >64", len(long))
	}
	alias := functionAlias(long)
	if len(alias) > 64 || !validFunctionName(alias) || alias == long {
		t.Fatalf("invalid alias %q", alias)
	}

	firstBody := `{"model":"gpt-6.1-sol","input":"read document","tools":[{"type":"function","name":"` + long + `","parameters":{"type":"object"}}]}`
	first, names, err := prepareResponsesBody([]byte(firstBody))
	if err != nil || names[alias] != long {
		t.Fatalf("body=%s aliases=%v err=%v", first, names, err)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(first, &forwarded); err != nil {
		t.Fatal(err)
	}
	tool := forwarded["tools"].([]any)[0].(map[string]any)
	if tool["name"] != alias {
		t.Fatalf("forwarded name=%v, want alias %s", tool["name"], alias)
	}

	// The provider streams a function call using the alias; clients must see
	// their original name so the next request can submit the matching output.
	line := `data: {"type":"response.output_item.done","item":{"type":"function_call","name":"` + alias + `","call_id":"call-long"}}`
	got := restoreFunctionAliases(line, names)
	if !strings.Contains(got, `"name":"`+long+`"`) {
		t.Fatalf("streamed call name not restored: %s", got)
	}

	continuation := `{"model":"gpt-6.1-sol","input":[{"type":"function_call","name":"` + alias + `","call_id":"call-long","arguments":"{}"},{"type":"function_call_output","call_id":"call-long","output":"done"}],"tools":[{"type":"function","name":"` + long + `","parameters":{}}]}`
	second, secondNames, err := prepareResponsesBody([]byte(continuation))
	if err != nil {
		t.Fatal(err)
	}
	if secondNames[alias] != long {
		t.Fatalf("continuation lost alias mapping: %v", secondNames)
	}
	if err := json.Unmarshal(second, &forwarded); err != nil {
		t.Fatal(err)
	}
	input := forwarded["input"].([]any)
	if input[0].(map[string]any)["name"] != alias || input[1].(map[string]any)["call_id"] != "call-long" {
		t.Fatalf("continuation names not restored: %s", second)
	}
}

func TestFunctionAliasLeavesValidShortNamesAlone(t *testing.T) {
	for _, name := range []string{"search_docs", "mcp__server__tool", "A-1"} {
		if got := functionAlias(name); got != name {
			t.Errorf("%q became %q", name, got)
		}
	}
	for _, name := range []string{"bad name", "a.b", strings.Repeat("x", 65)} {
		got := functionAlias(name)
		if len(got) > 64 || !validFunctionName(got) {
			t.Errorf("invalid alias %q for %q", got, name)
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
