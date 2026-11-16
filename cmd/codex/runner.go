package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Everything in this file follows the Codex app-server protocol as read from
// the openai/codex sources (codex-rs/app-server-protocol): newline-delimited
// JSON-RPC without the "jsonrpc" field, methods initialize / initialized
// (notification) / thread/start / turn/start / turn/interrupt, and
// notifications item/agentMessage/delta, thread/tokenUsage/updated, error and
// turn/completed. See the website guide for what remains unverified.

// Spec describes one app-server invocation (one request = one fresh process
// and one fresh, ephemeral thread).
type Spec struct {
	Bin       string
	CodexHome string
	Model     string
	Effort    string // "", low, medium, high
	System    string // developer instructions
	Prompt    string
}

// Result is what the turn produced.
type Result struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// Error kinds, used by the HTTP layer to pick a status code.
type Kind int

const (
	KindOther Kind = iota
	KindAuth
	KindRateLimit
	KindOverloaded
	KindBadRequest
)

// RunError is a failure reported by (or while talking to) the app-server.
type RunError struct {
	Kind Kind
	Msg  string
}

func (e *RunError) Error() string { return e.Msg }

func classify(info json.RawMessage) Kind {
	var s string
	if json.Unmarshal(info, &s) != nil {
		return KindOther
	}
	switch s {
	case "unauthorized":
		return KindAuth
	case "usageLimitExceeded", "rateLimitExceeded", "sessionBudgetExceeded":
		return KindRateLimit
	case "serverOverloaded", "policyCapacityUnavailable", "flexUnavailable":
		return KindOverloaded
	case "badRequest", "contextWindowExceeded", "modelNotFound":
		return KindBadRequest
	}
	return KindOther
}

// disableTools are `-c key=value` overrides (a global codex option) turning
// off the built-in tools whose feature keys exist in codex-rs/features. This
// is best effort: see the guide for what is and is not guaranteed.
var disableTools = []string{
	"features.shell_tool=false",
	"features.unified_exec=false",
	"features.view_image=false",
	"features.apply_patch_freeform=false",
	"features.js_repl=false",
	"features.code_mode=false",
	"web_search=\"disabled\"",
	"cli_auth_credentials_store=\"file\"",
}

func (s Spec) args() []string {
	var a []string
	for _, o := range disableTools {
		a = append(a, "-c", o)
	}
	return append(a, "app-server", "--listen", "stdio://")
}

type msg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type client struct {
	w    io.Writer
	next int
}

func (c *client) send(v map[string]any) error {
	b, _ := json.Marshal(v)
	_, err := c.w.Write(append(b, '\n'))
	return err
}

func (c *client) request(method string, params any) (int, error) {
	c.next++
	return c.next, c.send(map[string]any{"id": c.next, "method": method, "params": params})
}

// Run executes one chat turn and calls onText for every agent-message delta.
func Run(ctx context.Context, s Spec, onText func(string)) (Result, error) {
	dir, err := os.MkdirTemp("", "adapter-run-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	cmd := exec.Command(s.Bin, s.args()...) // not CommandContext: we interrupt first
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CODEX_HOME="+s.CodexHome)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start codex: %w", err)
	}
	c := &client{w: stdin}

	var (
		res         Result
		threadID    string
		turnID      string
		initID      int
		threadReq   int
		turnReq     int
		turnDone    bool
		fail        *RunError
		interrupted bool
	)
	lines := make(chan msg)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var m msg
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				select {
				case lines <- m:
				case <-quit:
					return
				}
			}
		}
	}()

	finish := func(e *RunError) (Result, error) {
		_ = stdin.Close()
		// Give the server a moment to exit on EOF, then kill it. The thread is
		// ephemeral, so nothing persists.
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		if e != nil {
			return res, e
		}
		return res, nil
	}

	initID, err = c.request("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "llm-proxy-manifestd-adapter", "title": nil, "version": "0"},
		"capabilities": map[string]any{"experimentalApi": false, "requestAttestation": false},
	})
	if err != nil {
		return finish(&RunError{Msg: "write to codex: " + err.Error()})
	}

	done := ctx.Done()
	for {
		select {
		case <-done:
			// Client went away: interrupt the turn (best effort), then stop.
			if turnID != "" && !interrupted {
				interrupted = true
				_, _ = c.request("turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
			}
			_, _ = finish(nil)
			return res, ctx.Err()
		case m, ok := <-lines:
			if !ok {
				if turnDone {
					return finish(nil)
				}
				text := strings.TrimSpace(stderr.String())
				if len(text) > 500 {
					text = text[len(text)-500:]
				}
				return finish(&RunError{Msg: "codex exited before the turn completed: " + text})
			}
			if len(m.ID) > 0 && m.Method != "" {
				// Server-initiated request (approvals, tool calls, ...): refuse.
				_ = c.send(map[string]any{"id": json.RawMessage(m.ID), "error": map[string]any{"code": -32601, "message": "not supported by this client"}})
				continue
			}
			if len(m.ID) > 0 && m.Method == "" {
				var id int
				_ = json.Unmarshal(m.ID, &id)
				if m.Error != nil {
					return finish(&RunError{Msg: "codex: " + m.Error.Message})
				}
				switch id {
				case initID:
					_ = c.send(map[string]any{"method": "initialized"})
					threadReq, err = c.request("thread/start", threadParams(s, dir))
				case threadReq:
					var r struct {
						Thread struct{ ID string } `json:"thread"`
					}
					_ = json.Unmarshal(m.Result, &r)
					if r.Thread.ID == "" {
						return finish(&RunError{Msg: "codex: thread/start returned no thread id"})
					}
					threadID = r.Thread.ID
					turnReq, err = c.request("turn/start", turnParams(s, threadID))
				case turnReq:
					var r struct {
						Turn struct{ ID string } `json:"turn"`
					}
					_ = json.Unmarshal(m.Result, &r)
					turnID = r.Turn.ID
					select {
					case <-done: // cancelled while starting: interrupt now
						interrupted = true
						_, _ = c.request("turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
					default:
					}
				}
				if err != nil {
					return finish(&RunError{Msg: "write to codex: " + err.Error()})
				}
				continue
			}
			switch m.Method {
			case "item/agentMessage/delta":
				var p struct{ Delta string }
				_ = json.Unmarshal(m.Params, &p)
				if p.Delta != "" {
					res.Text += p.Delta
					onText(p.Delta)
				}
			case "thread/tokenUsage/updated":
				var p struct {
					TokenUsage struct {
						Total struct{ InputTokens, OutputTokens int }
					}
				}
				_ = json.Unmarshal(m.Params, &p)
				res.InputTokens, res.OutputTokens = p.TokenUsage.Total.InputTokens, p.TokenUsage.Total.OutputTokens
			case "error":
				var p struct {
					Error struct {
						Message        string
						CodexErrorInfo json.RawMessage
					}
					WillRetry bool
				}
				_ = json.Unmarshal(m.Params, &p)
				if !p.WillRetry {
					fail = &RunError{Kind: classify(p.Error.CodexErrorInfo), Msg: p.Error.Message}
				}
			case "turn/completed":
				var p struct {
					Turn struct {
						Status string
						Error  *struct {
							Message        string
							CodexErrorInfo json.RawMessage
						}
					}
				}
				_ = json.Unmarshal(m.Params, &p)
				turnDone = true
				switch p.Turn.Status {
				case "completed":
					return finish(nil)
				default:
					if p.Turn.Error != nil {
						fail = &RunError{Kind: classify(p.Turn.Error.CodexErrorInfo), Msg: p.Turn.Error.Message}
					}
					if fail == nil {
						fail = &RunError{Msg: "codex turn ended with status " + p.Turn.Status}
					}
					return finish(fail)
				}
			}
		}
	}
}

func threadParams(s Spec, cwd string) map[string]any {
	p := map[string]any{
		"model":          s.Model,
		"cwd":            cwd,
		"approvalPolicy": "never",
		"sandbox":        "read-only",
		"ephemeral":      true,
	}
	if s.System != "" {
		p["developerInstructions"] = s.System
	}
	return p
}

func turnParams(s Spec, threadID string) map[string]any {
	p := map[string]any{
		"threadId": threadID,
		"input":    []any{map[string]any{"type": "text", "text": s.Prompt, "text_elements": []any{}}},
	}
	if s.Effort != "" {
		p["effort"] = s.Effort
	}
	return p
}
