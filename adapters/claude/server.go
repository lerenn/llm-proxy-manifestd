package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/adapters/internal/oai"
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
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	return mux
}

func (s *Server) loggedIn() bool {
	st, err := os.Stat(filepath.Join(s.ConfigDir, ".credentials.json"))
	return err == nil && st.Size() > 0
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
	_, _ = w.Write(oai.ErrorBody(msg, typ))
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req oai.ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	model, effort, err := oai.SplitModel(req.Model)
	if err != nil || model == "" {
		msg := "model is required"
		if err != nil {
			msg = err.Error()
		}
		writeErr(w, 400, "invalid_request_error", msg)
		return
	}
	system, prompt := oai.Prompt(req.Messages)
	if prompt == "" {
		writeErr(w, 400, "invalid_request_error", "messages must contain a user message")
		return
	}
	if !s.loggedIn() {
		writeErr(w, 503, "not_logged_in", "claude is not logged in")
		return
	}
	spec := Spec{Bin: s.Bin, ConfigDir: s.ConfigDir, Model: model, Effort: effort, System: system, Prompt: prompt}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	run := s.run
	if run == nil {
		run = Run
	}

	chunk := func(delta map[string]any, finish any) []byte {
		b, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return b
	}

	if !req.Stream {
		res, err := run(r.Context(), spec, func(string) {})
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": res.Text}}},
			"usage": oai.Usage{PromptTokens: res.InputTokens, CompletionTokens: res.OutputTokens, TotalTokens: res.InputTokens + res.OutputTokens},
		})
		return
	}

	// Streaming: headers are sent lazily with the first text so that an early
	// failure can still be reported as a real 5xx (which the proxy fails over on).
	started := false
	fl, _ := w.(http.Flusher)
	send := func(b []byte) {
		fmt.Fprintf(w, "data: %s\n\n", b)
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
		send(chunk(map[string]any{"role": "assistant"}, nil))
	}
	res, err := run(r.Context(), spec, func(t string) {
		start()
		send(chunk(map[string]any{"content": t}, nil))
	})
	if err != nil {
		if !started {
			s.fail(w, err)
			return
		}
		// Mid-stream failure: surface an error event, then end without [DONE].
		fmt.Fprintf(w, "data: %s\n\n", oai.ErrorBody(err.Error(), "upstream_error"))
		return
	}
	start()
	last := chunk(map[string]any{}, "stop")
	var m map[string]any
	_ = json.Unmarshal(last, &m)
	m["usage"] = oai.Usage{PromptTokens: res.InputTokens, CompletionTokens: res.OutputTokens, TotalTokens: res.InputTokens + res.OutputTokens}
	last, _ = json.Marshal(m)
	send(last)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var ae *AuthError
	switch {
	case errors.As(err, &ae):
		writeErr(w, 503, "not_logged_in", ae.Msg)
	case errors.Is(err, context.Canceled):
		writeErr(w, 499, "client_closed_request", "client went away")
	default:
		writeErr(w, 502, "upstream_error", err.Error())
	}
}
