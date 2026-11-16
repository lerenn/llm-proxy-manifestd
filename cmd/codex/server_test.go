package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/common"
)

// fakeAppServer writes a shell script standing in for `codex`. It logs argv and
// every JSON-RPC line it receives, answers initialize / thread/start /
// turn/start, then plays the given notification lines after turn/start.
func fakeAppServer(t *testing.T, afterTurn []string, hang bool) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	os.WriteFile(filepath.Join(dir, "after.jsonl"), []byte(strings.Join(afterTurn, "\n")+"\n"), 0o644)
	tail := "exit 0"
	if hang {
		tail = "while read -r l; do echo \"$l\" >> '" + dir + "/rx'; case \"$l\" in *turn/interrupt*) echo interrupted > '" + dir + "/interrupted'; exit 0;; esac; done"
	}
	script := `#!/bin/sh
printf '%s\n' "$@" > '` + dir + `/argv'
echo "$CODEX_HOME" > '` + dir + `/home'
while read -r line; do
  echo "$line" >> '` + dir + `/rx'
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*) echo '{"id":'$id',"result":{"userAgent":"fake","codexHome":"/x","platformFamily":"unix","platformOs":"linux"}}';;
    *'"method":"thread/start"'*) echo '{"id":'$id',"result":{"thread":{"id":"th-1"},"model":"m"}}';;
    *'"method":"turn/start"'*) echo '{"id":'$id',"result":{"turn":{"id":"tu-1","status":"inProgress"}}}'
       cat '` + dir + `/after.jsonl'
       ` + tail + `;;
  esac
done
`
	bin = filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func loggedIn(t *testing.T) string {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "auth.json"), []byte(`{"x":1}`), 0o600)
	return d
}

const (
	d1   = `{"method":"item/agentMessage/delta","params":{"threadId":"th-1","turnId":"tu-1","itemId":"i","delta":"Hel"}}`
	d2   = `{"method":"item/agentMessage/delta","params":{"threadId":"th-1","turnId":"tu-1","itemId":"i","delta":"lo"}}`
	usg  = `{"method":"thread/tokenUsage/updated","params":{"threadId":"th-1","turnId":"tu-1","tokenUsage":{"total":{"totalTokens":7,"inputTokens":5,"outputTokens":2},"last":{},"modelContextWindow":null}}}`
	done = `{"method":"turn/completed","params":{"threadId":"th-1","turn":{"id":"tu-1","status":"completed","error":null}}}`
)

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rr
}

func TestStreamingAndProtocol(t *testing.T) {
	bin, d := fakeAppServer(t, []string{d1, d2, usg, done}, false)
	home := loggedIn(t)
	s := &Server{Bin: bin, CodexHome: home}
	rr := post(s.Handler(), `{"model":"gpt-5.4@high","stream":true,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %v %s", rr.Code, rr.Header(), rr.Body)
	}
	var got strings.Builder
	doneSeen := false
	raw := rr.Body.String()
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		l := sc.Text()
		if l == "data: [DONE]" {
			doneSeen = true
		}
		if strings.HasPrefix(l, "data: {") {
			var c struct {
				Choices []struct{ Delta struct{ Content string } }
			}
			json.Unmarshal([]byte(l[6:]), &c)
			got.WriteString(c.Choices[0].Delta.Content)
		}
	}
	if got.String() != "Hello" || !doneSeen || !strings.Contains(raw, `"prompt_tokens":5`) {
		t.Fatalf("got %q done=%v\n%s", got.String(), doneSeen, raw)
	}
	argv, _ := os.ReadFile(filepath.Join(d, "argv"))
	for _, want := range []string{"features.shell_tool=false", "app-server", "stdio://"} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("argv missing %q:\n%s", want, argv)
		}
	}
	if h, _ := os.ReadFile(filepath.Join(d, "home")); strings.TrimSpace(string(h)) != home {
		t.Errorf("CODEX_HOME = %q", h)
	}
	rx, _ := os.ReadFile(filepath.Join(d, "rx"))
	for _, want := range []string{
		`"method":"initialize"`, `"method":"initialized"`, `"approvalPolicy":"never"`, `"sandbox":"read-only"`,
		`"ephemeral":true`, `"model":"gpt-5.4"`, `"developerInstructions":"sys"`, `"effort":"high"`,
		`"threadId":"th-1"`, `"text":"hi"`,
	} {
		if !strings.Contains(string(rx), want) {
			t.Errorf("rpc log missing %q:\n%s", want, rx)
		}
	}
}

func TestNonStream(t *testing.T) {
	bin, _ := fakeAppServer(t, []string{d1, d2, usg, done}, false)
	s := &Server{Bin: bin, CodexHome: loggedIn(t)}
	rr := post(s.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	var r struct {
		Choices []struct{ Message struct{ Content string } }
	}
	json.Unmarshal(rr.Body.Bytes(), &r)
	if rr.Code != 200 || len(r.Choices) != 1 || r.Choices[0].Message.Content != "Hello" || !strings.Contains(rr.Body.String(), `"completion_tokens":2`) {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
}

func TestErrorsAndMapping(t *testing.T) {
	s := &Server{Bin: "/nonexistent", CodexHome: t.TempDir()}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/ready", nil))
	if rr.Code != 503 {
		t.Fatalf("ready: %d", rr.Code)
	}
	if rr := post(s.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 503 {
		t.Fatalf("chat not logged in: %d", rr.Code)
	}
	s.CodexHome = loggedIn(t)
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/ready", nil))
	if rr.Code != 200 {
		t.Fatalf("ready: %d", rr.Code)
	}
	if rr := post(s.Handler(), `{"model":"m@max","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 400 {
		t.Fatalf("effort: %d", rr.Code)
	}
	if rr := post(s.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 502 {
		t.Fatalf("missing binary should be 502: %d", rr.Code)
	}
	fail := func(info string) string {
		return `{"method":"turn/completed","params":{"threadId":"th-1","turn":{"id":"tu-1","status":"failed","error":{"message":"boom","codexErrorInfo":` + info + `}}}}`
	}
	for info, want := range map[string]int{`"unauthorized"`: 503, `"usageLimitExceeded"`: 503, `"serverOverloaded"`: 503, `"badRequest"`: 400, `"other"`: 502} {
		bin, _ := fakeAppServer(t, []string{fail(info)}, false)
		s.Bin = bin
		if rr := post(s.Handler(), `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`); rr.Code != want {
			t.Errorf("%s: got %d want %d (%s)", info, rr.Code, want, rr.Body)
		}
	}
	// error notification (not retrying) followed by an interrupted turn.
	bin, _ := fakeAppServer(t, []string{
		`{"method":"error","params":{"error":{"message":"limit","codexErrorInfo":"rateLimitExceeded"},"willRetry":false,"threadId":"th-1","turnId":"tu-1"}}`,
		`{"method":"turn/completed","params":{"threadId":"th-1","turn":{"id":"tu-1","status":"failed","error":null}}}`,
	}, false)
	s.Bin = bin
	if rr := post(s.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 503 || !strings.Contains(rr.Body.String(), "rate_limit_error") {
		t.Errorf("error notification: %d %s", rr.Code, rr.Body)
	}
}

func TestClientDisconnectInterruptsTurn(t *testing.T) {
	bin, d := fakeAppServer(t, []string{d1}, true) // streams one delta then waits
	s := &Server{Bin: bin, CodexHome: loggedIn(t)}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	ctx, cancel := contextWithCancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	for { // wait for first content chunk
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(l, `"content":"Hel"`) {
			break
		}
	}
	cancel()
	resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(d, "interrupted")); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("turn/interrupt was not sent after the client disconnected")
}

func TestProbe(t *testing.T) {
	s := &Server{Bin: "x", CodexHome: loggedIn(t)}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")
	if common.Probe(addr) != 0 {
		t.Fatal("expected ready")
	}
	s.CodexHome = t.TempDir()
	if common.Probe(addr) != 1 {
		t.Fatal("expected not ready")
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
