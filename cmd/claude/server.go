package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/anthropic"
	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/common"
)

// Server is the OpenAI-compatible HTTP front of the Claude CLI.
type Server struct {
	Bin       string
	ConfigDir string
	// run is swappable for tests; defaults to Run.
	run func(context.Context, Spec, func(string)) (Result, error)
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", s.ready)
	mux.HandleFunc("POST /v1/messages", s.messages)
	return mux
}

func (s *Server) loggedIn() bool {
	return common.NonEmptyFile(filepath.Join(s.ConfigDir, ".credentials.json"))
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	if !s.loggedIn() {
		http.Error(w, "not logged in: run `claude` in this container and use /login", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func writeErr(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(anthropic.ErrorBody(msg, typ))
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	var req anthropic.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	model, effort, err := common.SplitModel(req.Model)
	if err != nil || model == "" {
		msg := "model is required"
		if err != nil {
			msg = err.Error()
		}
		writeErr(w, 400, "invalid_request_error", msg)
		return
	}
	system, prompt := anthropic.Prompt(req)
	if prompt == "" {
		writeErr(w, 400, "invalid_request_error", "messages must contain a user message")
		return
	}
	if !s.loggedIn() {
		writeErr(w, 503, "authentication_error", "claude is not logged in")
		return
	}
	spec := Spec{Bin: s.Bin, ConfigDir: s.ConfigDir, Model: model, Effort: effort, System: system, Prompt: prompt}
	id := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	run := s.run
	if run == nil {
		run = Run
	}

	if !req.Stream {
		res, err := run(r.Context(), spec, func(string) {})
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content":     []any{map[string]any{"type": "text", "text": res.Text}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": anthropic.Usage{InputTokens: res.InputTokens, OutputTokens: res.OutputTokens},
		})
		return
	}

	// Streaming: headers are sent lazily with the first text so that an early
	// failure can still be reported as a real 5xx (which the proxy fails over on).
	started := false
	fl, _ := w.(http.Flusher)
	send := func(event string, v map[string]any) {
		v["type"] = event
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	start := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		send("message_start", map[string]any{"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthropic.Usage{},
		}})
		send("content_block_start", map[string]any{"index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		send("ping", map[string]any{})
	}
	res, err := run(r.Context(), spec, func(t string) {
		start()
		send("content_block_delta", map[string]any{"index": 0, "delta": map[string]any{"type": "text_delta", "text": t}})
	})
	if err != nil {
		if !started {
			s.fail(w, err)
			return
		}
		// Mid-stream failure: surface an error event and end the stream.
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", anthropic.ErrorBody(err.Error(), "api_error"))
		return
	}
	start()
	send("content_block_stop", map[string]any{"index": 0})
	send("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": anthropic.Usage{InputTokens: res.InputTokens, OutputTokens: res.OutputTokens},
	})
	send("message_stop", map[string]any{})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var ae *AuthError
	switch {
	case errors.As(err, &ae):
		writeErr(w, 503, "authentication_error", ae.Msg)
	case errors.Is(err, context.Canceled):
		writeErr(w, 499, "api_error", "client went away")
	default:
		writeErr(w, 502, "api_error", err.Error())
	}
}
