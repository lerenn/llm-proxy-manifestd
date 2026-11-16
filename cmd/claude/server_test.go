package main

import (
	"bufio"
	"encoding/json"
	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/common"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCLI writes a shell script standing in for the claude binary. It records
// its argv and stdin next to itself and prints the given stream-json lines.
func fakeCLI(t *testing.T, lines []string, exit int) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	out := filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + dir + "/argv'\ncat > '" + dir + "/stdin'\ncat '" + out + "'\nexit " + string(rune('0'+exit)) + "\n"
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func loggedIn(t *testing.T) string {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, ".credentials.json"), []byte(`{"x":1}`), 0o600)
	return d
}

const (
	delta1 = `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}}`
	delta2 = `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}`
	asst   = `{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"}]}}`
	result = `{"type":"result","subtype":"success","is_error":false,"result":"Hello","usage":{"input_tokens":5,"output_tokens":2}}`
)

func post(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	return rr
}

func TestStreamingAndArgs(t *testing.T) {
	bin, d := fakeCLI(t, []string{`{"type":"system","subtype":"init"}`, delta1, delta2, asst, result}, 0)
	s := &Server{Bin: bin, ConfigDir: loggedIn(t)}
	rr := post(t, s.Handler(), `{"model":"claude-sonnet-5-5@medium","stream":true,"system":"sys","messages":[{"role":"user","content":"hi"}]}`)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("%d %v", rr.Code, rr.Header())
	}
	var got strings.Builder
	var events []string
	sc := bufio.NewScanner(rr.Body)
	for sc.Scan() {
		l := sc.Text()
		if strings.HasPrefix(l, "event: ") {
			events = append(events, l[7:])
		}
		if strings.HasPrefix(l, "data: {") {
			var c struct {
				Delta struct{ Text string }
			}
			json.Unmarshal([]byte(l[6:]), &c)
			got.WriteString(c.Delta.Text)
		}
	}
	wantEv := "message_start content_block_start ping content_block_delta content_block_delta content_block_stop message_delta message_stop"
	if got.String() != "Hello" || strings.Join(events, " ") != wantEv {
		t.Fatalf("got %q events=%v\n%s", got.String(), events, rr.Body)
	}
	argv, _ := os.ReadFile(filepath.Join(d, "argv"))
	a := string(argv)
	for _, want := range []string{"--tools\n\n", "--no-session-persistence", "--model\nclaude-sonnet-5-5\n", "--effort\nmedium\n", "--system-prompt\nsys\n", "stream-json"} {
		if !strings.Contains(a, want) {
			t.Errorf("argv missing %q:\n%s", want, a)
		}
	}
	in, _ := os.ReadFile(filepath.Join(d, "stdin"))
	if !strings.Contains(string(in), `"content":"hi"`) || !strings.Contains(string(in), `"type":"user"`) {
		t.Errorf("stdin: %s", in)
	}
}

func TestNonStreamFallbackNoDeltas(t *testing.T) {
	bin, _ := fakeCLI(t, []string{asst, result}, 0)
	s := &Server{Bin: bin, ConfigDir: loggedIn(t)}
	rr := post(t, s.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	var r struct {
		Type    string
		Content []struct{ Text string }
	}
	json.Unmarshal(rr.Body.Bytes(), &r)
	if rr.Code != 200 || r.Type != "message" || len(r.Content) != 1 || r.Content[0].Text != "Hello" || !strings.Contains(rr.Body.String(), `"input_tokens":5`) {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
}

func TestErrors(t *testing.T) {
	// Not logged in: not ready, chat 503.
	s := &Server{Bin: "/nonexistent", ConfigDir: t.TempDir()}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/ready", nil))
	if rr.Code != 503 {
		t.Fatalf("ready: %d", rr.Code)
	}
	if rr := post(t, s.Handler(), `{"model":"m","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 503 {
		t.Fatalf("chat: %d", rr.Code)
	}
	// Logged in: ready.
	s.ConfigDir = loggedIn(t)
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/ready", nil))
	if rr.Code != 200 {
		t.Fatalf("ready: %d", rr.Code)
	}
	// Bad effort suffix.
	if rr := post(t, s.Handler(), `{"model":"m@max","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 400 {
		t.Fatalf("effort: %d", rr.Code)
	}
	// CLI reports an auth error -> 503.
	bin, _ := fakeCLI(t, []string{`{"type":"result","is_error":true,"result":"Not logged in · Please run /login"}`}, 1)
	s.Bin = bin
	if rr := post(t, s.Handler(), `{"model":"m","stream":true,"system":"sys","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 503 {
		t.Fatalf("auth: %d %s", rr.Code, rr.Body)
	}
	// Generic CLI failure before output -> 502 (proxy can fail over).
	bin, _ = fakeCLI(t, []string{`{"type":"result","is_error":true,"result":"overloaded"}`}, 1)
	s.Bin = bin
	if rr := post(t, s.Handler(), `{"model":"m","stream":true,"system":"sys","messages":[{"role":"user","content":"hi"}]}`); rr.Code != 502 {
		t.Fatalf("generic: %d", rr.Code)
	}
}

func TestProbe(t *testing.T) {
	s := &Server{Bin: "x", ConfigDir: loggedIn(t)}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")
	if common.Probe(addr) != 0 {
		t.Fatal("expected ready")
	}
	s.ConfigDir = t.TempDir()
	if common.Probe(addr) != 1 {
		t.Fatal("expected not ready")
	}
	if common.Probe("127.0.0.1:1") != 1 {
		t.Fatal("expected failure")
	}
}
