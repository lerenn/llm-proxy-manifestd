package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/common"
	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/openai"
)

// Server is the OpenAI-compatible HTTP front of `codex app-server`.
type Server struct {
	Bin       string
	CodexHome string
	run       func(context.Context, Spec, func(string)) (Result, error) // swappable in tests
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", s.ready)
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	return mux
}

// loggedIn: with the default (file) credential store, `codex login` writes
// $CODEX_HOME/auth.json (codex-rs/login/src/auth/storage.rs).
func (s *Server) loggedIn() bool {
	return common.NonEmptyFile(filepath.Join(s.CodexHome, "auth.json"))
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	if !s.loggedIn() {
		http.Error(w, "not logged in: run `codex login --device-auth` in this container", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func writeErr(w http.ResponseWriter, code int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(openai.ErrorBody(msg, typ))
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var re *RunError
	switch {
	case errors.As(err, &re):
		switch re.Kind {
		case KindAuth:
			writeErr(w, 503, "authentication_error", re.Msg)
		case KindRateLimit:
			writeErr(w, 503, "rate_limit_error", re.Msg) // 5xx so the proxy fails over
		case KindOverloaded:
			writeErr(w, 503, "server_error", re.Msg)
		case KindBadRequest:
			writeErr(w, 400, "invalid_request_error", re.Msg)
		default:
			writeErr(w, 502, "upstream_error", re.Msg)
		}
	case errors.Is(err, context.Canceled):
		writeErr(w, 499, "client_closed_request", "client went away")
	default:
		writeErr(w, 502, "upstream_error", err.Error())
	}
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req openai.Request
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
	system, prompt := openai.Prompt(req.Messages)
	if prompt == "" {
		writeErr(w, 400, "invalid_request_error", "messages must contain a user message")
		return
	}
	if !s.loggedIn() {
		writeErr(w, 503, "authentication_error", "codex is not logged in")
		return
	}
	spec := Spec{Bin: s.Bin, CodexHome: s.CodexHome, Model: model, Effort: effort, System: system, Prompt: prompt}
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	run := s.run
	if run == nil {
		run = Run
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
	}
	usage := func(res Result) openai.Usage {
		return openai.Usage{PromptTokens: res.InputTokens, CompletionTokens: res.OutputTokens, TotalTokens: res.InputTokens + res.OutputTokens}
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
			"usage": usage(res),
		})
		return
	}

	// Headers are sent lazily with the first text so an early failure is still
	// a real 5xx the proxy can fail over on.
	started := false
	fl, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
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
		fmt.Fprintf(w, "data: %s\n\n", openai.ErrorBody(err.Error(), "upstream_error"))
		return
	}
	start()
	last := chunk(map[string]any{}, "stop")
	last["usage"] = usage(res)
	send(last)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}
