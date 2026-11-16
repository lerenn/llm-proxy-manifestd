package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Spec describes one CLI invocation (one request = one fresh process).
type Spec struct {
	Bin       string // path to the claude binary
	ConfigDir string // CLAUDE_CONFIG_DIR
	Model     string
	Effort    string // "", low, medium, high
	System    string
	Prompt    string
}

// Result is what the CLI reported at the end of the run.
type Result struct {
	Text         string
	InputTokens  int
	OutputTokens int
}

// AuthError is returned when the CLI reports it is not logged in.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

func (s Spec) args() []string {
	a := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--tools", "", // no tools at all
		"--no-session-persistence", // nothing written to disk
		"--disable-slash-commands",
		"--strict-mcp-config",   // no MCP servers
		"--setting-sources", "", // ignore user/project/local settings
		"--model", s.Model,
	}
	if s.Effort != "" {
		a = append(a, "--effort", s.Effort)
	}
	if s.System != "" {
		a = append(a, "--system-prompt", s.System)
	}
	return a
}

// event is the subset of the CLI's stream-json lines we interpret.
type event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Event   struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Run spawns the CLI, writes the prompt as a single stream-json user message,
// and calls onText for each text delta as it arrives.
func Run(ctx context.Context, s Spec, onText func(string)) (Result, error) {
	dir, err := os.MkdirTemp("", "adapter-run-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)

	cmd := exec.CommandContext(ctx, s.Bin, s.args()...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CLAUDE_CONFIG_DIR="+s.ConfigDir,
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	)
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
		return Result{}, fmt.Errorf("start claude: %w", err)
	}
	in, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": s.Prompt},
	})
	_, _ = stdin.Write(append(in, '\n'))
	_ = stdin.Close()

	var (
		res       Result
		gotDelta  bool
		assistant strings.Builder
		final     *event
		sc        = bufio.NewScanner(stdout)
	)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "stream_event":
			if ev.Event.Type == "content_block_delta" && ev.Event.Delta.Type == "text_delta" && ev.Event.Delta.Text != "" {
				gotDelta = true
				res.Text += ev.Event.Delta.Text
				onText(ev.Event.Delta.Text)
			}
		case "assistant":
			for _, c := range ev.Message.Content {
				if c.Type == "text" {
					assistant.WriteString(c.Text)
				}
			}
		case "result":
			e := ev
			final = &e
		}
	}
	waitErr := cmd.Wait()

	if final == nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if waitErr != nil {
			return res, fmt.Errorf("claude exited without a result: %v: %s", waitErr, tail(msg))
		}
		return res, errors.New("claude produced no result event")
	}
	if final.IsError {
		msg := final.Result
		if looksLikeAuth(msg) {
			return res, &AuthError{Msg: msg}
		}
		return res, fmt.Errorf("claude error: %s", msg)
	}
	if !gotDelta {
		// No partial events: fall back to the complete text.
		text := assistant.String()
		if text == "" {
			text = final.Result
		}
		res.Text = text
		if text != "" {
			onText(text)
		}
	}
	res.InputTokens, res.OutputTokens = final.Usage.InputTokens, final.Usage.OutputTokens
	return res, nil
}

func looksLikeAuth(m string) bool {
	m = strings.ToLower(m)
	return strings.Contains(m, "not logged in") || strings.Contains(m, "/login") ||
		strings.Contains(m, "authentication") || strings.Contains(m, "invalid api key")
}

func tail(s string) string {
	if len(s) > 500 {
		return s[len(s)-500:]
	}
	return s
}
