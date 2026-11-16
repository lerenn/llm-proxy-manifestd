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
	"sort"
	"strconv"
	"strings"
	"sync"
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

	version      string
	started      time.Time
	reloadStatus func() config.ReloadStatus
	routes       sync.Map // route name -> *routeCounters
	dashboard    http.Handler
}

// Option customises a Handler.
type Option func(*Handler)

// WithVersion sets the build version shown on the dashboard.
func WithVersion(v string) Option { return func(h *Handler) { h.version = v } }

// WithReloadStatus supplies the last config reload outcome.
func WithReloadStatus(f func() config.ReloadStatus) Option {
	return func(h *Handler) { h.reloadStatus = f }
}

// New creates a Handler.
func New(state StateFunc, logger *slog.Logger, opts ...Option) *Handler {
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
		mux:     http.NewServeMux(),
		started: time.Now(),
		version: "dev",
	}
	for _, o := range opts {
		o(h)
	}
	h.dashboard = h.newDashboard()
	h.mux.HandleFunc("GET /healthz", h.healthz)
	h.mux.HandleFunc("GET /status", h.status)
	h.mux.HandleFunc("GET /v1/models", h.models)
	h.mux.HandleFunc("/v1/", h.proxy)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.serveDashboard(w, r) {
		return
	}
	h.mux.ServeHTTP(w, r)
}

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

	rc := h.counters(model)
	rc.requests.Add(1)

	rt := &failoverTransport{h: h, st: st, route: route, priority: priority, body: body, rc: rc}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header.Del("X-Priority")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // client went away
			}
			var qt *queueTimeoutError
			if errors.As(err, &qt) {
				rc.errors.Add(1)
				w.Header().Set("X-Pool-Name", qt.pool)
				w.Header().Set("Retry-After", "5")
				writeError(w, http.StatusServiceUnavailable, "server_error", "queue_timeout",
					"timed out waiting for a free slot in pool '"+qt.pool+"'")
				return
			}
			var ae *acquireError
			if errors.As(err, &ae) {
				writeError(w, http.StatusInternalServerError, "server_error", "acquire_failed", ae.Error())
				return
			}
			rc.errors.Add(1)
			h.logger.Error("upstream error", "error", err)
			writeError(w, http.StatusBadGateway, "server_error", "upstream_error", "upstream request failed")
		},
		FlushInterval: -1, // flush every write: no SSE buffering
		Transport:     rt,
	}
	rp.ServeHTTP(w, r)
}

type queueTimeoutError struct{ pool string }

func (e *queueTimeoutError) Error() string { return "queue timeout in pool " + e.pool }

type acquireError struct{ err error }

func (e *acquireError) Error() string { return e.err.Error() }

// failoverTransport sends a request to the route's pools, trying further
// pools when an attempt fails before any response was produced. See
// README "Multi-pool routing" for the exact semantics.
type failoverTransport struct {
	h        *Handler
	st       *config.RuntimeState
	route    config.Route
	priority int
	body     []byte
	rc       *routeCounters
}

// acquire picks a pool among remaining (indexes into route.Pools) and takes a
// slot. Pools with a free slot are tried by ascending in-flight count (ties by
// route order). If none has a free slot it queues on the pool with the lowest
// active+queued load (ties by route order).
func (t *failoverTransport) acquire(ctx context.Context, remaining []int) (idx int, release func(), wait time.Duration, err error) {
	start := time.Now()
	type cand struct{ idx, active, load int }
	cands := make([]cand, 0, len(remaining))
	for _, i := range remaining {
		s := t.st.Pools[t.route.Pools[i]].Stats()
		cands = append(cands, cand{i, s.Active, s.Active + s.QueueDepth})
	}
	sort.SliceStable(cands, func(a, b int) bool { return cands[a].active < cands[b].active })
	for _, c := range cands {
		if rel, ok := t.st.Pools[t.route.Pools[c.idx]].TryAcquire(); ok {
			return c.idx, rel, time.Since(start), nil
		}
	}
	best := cands[0]
	for _, c := range cands {
		if c.load < best.load || (c.load == best.load && c.idx < best.idx) {
			best = c
		}
	}
	name := t.route.Pools[best.idx]
	t.rc.queued.Add(1)
	qctx, cancel := context.WithTimeout(ctx, t.st.Config.Server.QueueTimeout.Std())
	rel, aerr := t.st.Pools[name].Acquire(qctx, t.priority)
	cancel()
	t.rc.queued.Add(-1)
	if aerr != nil {
		if ctx.Err() != nil {
			return 0, nil, 0, ctx.Err()
		}
		if errors.Is(aerr, context.DeadlineExceeded) {
			return 0, nil, 0, &queueTimeoutError{pool: name}
		}
		return 0, nil, 0, &acquireError{aerr}
	}
	return best.idx, rel, time.Since(start), nil
}

func (t *failoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	remaining := make([]int, len(t.route.Pools))
	for i := range remaining {
		remaining[i] = i
	}
	for {
		idx, release, wait, err := t.acquire(req.Context(), remaining)
		if err != nil {
			return nil, err
		}
		name := t.route.Pools[idx]
		pc := t.st.Config.Pools[name]
		for k, v := range remaining {
			if v == idx {
				remaining = append(remaining[:k:k], remaining[k+1:]...)
				break
			}
		}
		last := len(remaining) == 0

		out := req.Clone(req.Context())
		out.URL.Scheme, out.URL.Host = pc.Parsed.Scheme, pc.Parsed.Host
		out.URL.Path = joinPath(pc.Parsed.Path, req.URL.Path)
		out.URL.RawPath = ""
		out.Host = pc.Parsed.Host
		out.Header.Del("Authorization")
		if pc.APIKey != "" {
			out.Header.Del("X-Api-Key")
			out.Header.Set("Authorization", "Bearer "+pc.APIKey)
		}
		out.Body = io.NopCloser(bytes.NewReader(t.body))
		out.ContentLength = int64(len(t.body))
		out.Header.Set("Content-Length", strconv.Itoa(len(t.body)))
		out.Header.Del("Transfer-Encoding")
		out.TransferEncoding = nil

		resp, err := t.h.transport.RoundTrip(out)
		if err == nil && resp.StatusCode >= 500 && !last {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			err = errors.New("upstream status " + strconv.Itoa(resp.StatusCode))
			resp = nil
		}
		if err != nil {
			release()
			if req.Context().Err() != nil {
				return nil, err
			}
			if last {
				t.h.logger.Error("upstream error", "pool", name, "error", err)
				return nil, err
			}
			t.h.logger.Warn("upstream failed, failing over", "pool", name, "error", err)
			continue
		}
		if resp.StatusCode >= 500 {
			t.rc.errors.Add(1)
		}
		resp.Header.Set("X-Queue-Wait-Ms", strconv.FormatInt(wait.Milliseconds(), 10))
		resp.Header.Set("X-Pool-Name", name)
		// The slot is held for the entire response stream, not just the headers.
		t.rc.inflight.Add(1)
		var once sync.Once
		resp.Body = &releaseBody{ReadCloser: resp.Body, done: func() {
			once.Do(func() { t.rc.inflight.Add(-1); release() })
		}}
		return resp, nil
	}
}

type releaseBody struct {
	io.ReadCloser
	done func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.done()
	return err
}

// joinPath mirrors httputil.ProxyRequest.SetURL path joining.
func joinPath(base, p string) string {
	switch {
	case base == "":
		return p
	case strings.HasSuffix(base, "/") && strings.HasPrefix(p, "/"):
		return base + p[1:]
	case !strings.HasSuffix(base, "/") && !strings.HasPrefix(p, "/"):
		return base + "/" + p
	}
	return base + p
}
