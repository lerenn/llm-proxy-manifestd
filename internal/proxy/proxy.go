// Package proxy implements the HTTP handler: model routing, priority queuing
// and zero-buffer streaming reverse proxying.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
)

const maxBodyBytes = 64 << 20

// StateFunc returns the active runtime state (nil until a config is loaded).
type StateFunc func() *config.RuntimeState

// Handler serves the proxy and the management endpoints.
type Handler struct {
	state     StateFunc
	logger    *slog.Logger
	transport http.RoundTripper
	mux       *http.ServeMux
}

// New creates a Handler.
func New(state StateFunc, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Handler{
		state:  state,
		logger: logger,
		transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 0, // LLM upstreams may think for a long time
			DisableCompression:    true,
		},
		mux: http.NewServeMux(),
	}
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /status", h.status)
	h.mux.HandleFunc("GET /v1/models", h.models)
	h.mux.HandleFunc("/v1/", h.proxy)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, typ, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"message": msg,
		"type":    typ,
		"param":   nil,
		"code":    errCode,
	}})
}

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	if h.state() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) status(w http.ResponseWriter, _ *http.Request) {
	st := h.state()
	if st == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	out := make(map[string]any, len(st.Pools))
	for name, p := range st.Pools {
		out[name] = p.Stats()
	}
	writeJSON(w, http.StatusOK, map[string]any{"pools": out})
}

func (h *Handler) models(w http.ResponseWriter, _ *http.Request) {
	st := h.state()
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "not_ready", "configuration not loaded")
		return
	}
	data := make([]map[string]any, 0, len(st.Config.Routes))
	for _, name := range st.Config.RouteNames() {
		data = append(data, map[string]any{
			"id":       name,
			"object":   "model",
			"created":  0,
			"owned_by": "llm-proxy-manifestd",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) proxy(w http.ResponseWriter, r *http.Request) {
	st := h.state() // single snapshot for the whole request
	if st == nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "not_ready", "configuration not loaded")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", "could not read request body: "+err.Error())
		return
	}
	var obj map[string]json.RawMessage
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &obj) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must be a JSON object")
		return
	}
	var model string
	if raw, ok := obj["model"]; !ok || json.Unmarshal(raw, &model) != nil || model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "the 'model' field is required and must be a string")
		return
	}
	route, ok := st.Config.Routes[model]
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "the model '"+model+"' does not exist")
		return
	}
	// Route.Pool existence is guaranteed by config validation.
	poolCfg := st.Config.Pools[route.Pool]
	pl := st.Pools[route.Pool]

	if route.TargetModel != "" && route.TargetModel != model {
		tm, _ := json.Marshal(route.TargetModel)
		obj["model"] = tm
		if body, err = json.Marshal(obj); err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "rewrite_failed", err.Error())
			return
		}
	}

	priority := route.Priority
	if v := r.Header.Get("X-Priority"); v != "" {
		p, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_priority", "X-Priority must be an integer")
			return
		}
		priority = p
	}

	start := time.Now()
	qctx, cancel := context.WithTimeout(r.Context(), st.Config.Server.QueueTimeout.Std())
	release, err := pl.Acquire(qctx, priority)
	cancel()
	if err != nil {
		if r.Context().Err() != nil {
			return // client went away while queued
		}
		if errors.Is(err, context.DeadlineExceeded) {
			w.Header().Set("X-Pool-Name", route.Pool)
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusServiceUnavailable, "server_error", "queue_timeout",
				"timed out waiting for a free slot in pool '"+route.Pool+"'")
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error", "acquire_failed", err.Error())
		return
	}
	// The slot is held for the entire response stream, not just the headers.
	defer release()
	wait := time.Since(start)

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(poolCfg.Parsed)
			pr.Out.Host = poolCfg.Parsed.Host
			pr.Out.Header.Del("X-Priority")
			pr.Out.Header.Del("Authorization")
			if poolCfg.APIKey != "" {
				pr.Out.Header.Del("X-Api-Key")
				pr.Out.Header.Set("Authorization", "Bearer "+poolCfg.APIKey)
			}
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
			pr.Out.Header.Set("Content-Length", strconv.Itoa(len(body)))
			pr.Out.Header.Del("Transfer-Encoding")
			pr.Out.TransferEncoding = nil
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Set("X-Queue-Wait-Ms", strconv.FormatInt(wait.Milliseconds(), 10))
			resp.Header.Set("X-Pool-Name", route.Pool)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			h.logger.Error("upstream error", "pool", route.Pool, "error", err)
			writeError(w, http.StatusBadGateway, "server_error", "upstream_error", "upstream request failed")
		},
		FlushInterval: -1, // flush every write: no SSE buffering
		Transport:     h.transport,
	}
	rp.ServeHTTP(w, r)
}
