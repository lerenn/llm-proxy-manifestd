// Package proxy implements the HTTP handler: endpoint resolution, priority queuing
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
	"sync/atomic"
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
	endpoints    sync.Map // counterKey -> *routeCounters
	rr           sync.Map // counterKey -> *atomic.Uint64 (round-robin cursor)
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
	writeProtoError(w, config.ProtocolOpenAI, code, typ, errCode, msg)
}

// writeProtoError writes an error in the error shape of the given protocol:
// OpenAI {"error":{message,type,param,code}} or Anthropic
// {"type":"error","error":{type,message}}.
func writeProtoError(w http.ResponseWriter, proto string, code int, typ, errCode, msg string) {
	if proto != config.ProtocolAnthropic {
		writeJSON(w, code, map[string]any{"error": map[string]any{
			"message": msg,
			"type":    typ,
			"param":   nil,
			"code":    errCode,
		}})
		return
	}
	at := "api_error"
	switch code {
	case http.StatusBadRequest:
		at = "invalid_request_error"
	case http.StatusNotFound:
		at = "not_found_error"
	case http.StatusServiceUnavailable:
		at = "overloaded_error"
	}
	writeJSON(w, code, map[string]any{"type": "error", "error": map[string]any{"type": at, "message": msg}})
}

// protocolOf returns the API a request path belongs to: /v1/messages and
// below are Anthropic, everything else OpenAI.
func protocolOf(path string) string {
	if path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/") {
		return config.ProtocolAnthropic
	}
	return config.ProtocolOpenAI
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

// modelsProtocol returns the API a GET /v1/models caller speaks: Anthropic
// when it sends an anthropic-version or x-api-key header (Anthropic SDKs
// always send one of them), OpenAI otherwise.
func modelsProtocol(r *http.Request) string {
	if r.Header.Get("Anthropic-Version") != "" || r.Header.Get("X-Api-Key") != "" {
		return config.ProtocolAnthropic
	}
	return config.ProtocolOpenAI
}

// models lists the endpoint names configured for the caller's protocol, in
// that protocol's list shape.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	proto := modelsProtocol(r)
	st := h.state()
	if st == nil {
		writeProtoError(w, proto, http.StatusServiceUnavailable, "server_error", "not_ready", "configuration not loaded")
		return
	}
	names := st.Config.EndpointNames(proto)
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if proto == config.ProtocolAnthropic {
			data = append(data, map[string]any{"type": "model", "id": name, "display_name": name, "created_at": "1970-01-01T00:00:00Z"})
			continue
		}
		data = append(data, map[string]any{"id": name, "object": "model", "created": 0, "owned_by": "llm-proxy-manifestd"})
	}
	if proto == config.ProtocolAnthropic {
		resp := map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil}
		if len(data) > 0 {
			resp["first_id"], resp["last_id"] = data[0]["id"], data[len(data)-1]["id"]
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) proxy(w http.ResponseWriter, r *http.Request) {
	st := h.state() // single snapshot for the whole request
	proto := protocolOf(r.URL.Path)
	if st == nil {
		writeProtoError(w, proto, http.StatusServiceUnavailable, "server_error", "not_ready", "configuration not loaded")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeProtoError(w, proto, http.StatusBadRequest, "invalid_request_error", "invalid_body", "could not read request body: "+err.Error())
		return
	}
	var obj map[string]json.RawMessage
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &obj) != nil {
		writeProtoError(w, proto, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must be a JSON object")
		return
	}
	var model string
	if raw, ok := obj["model"]; !ok || json.Unmarshal(raw, &model) != nil || model == "" {
		writeProtoError(w, proto, http.StatusBadRequest, "invalid_request_error", "missing_model", "the 'model' field is required and must be a string")
		return
	}
	ep, ok := st.Config.Endpoint(proto, model)
	if !ok {
		writeProtoError(w, proto, http.StatusNotFound, "invalid_request_error", "endpoint_not_configured",
			"no "+proto+" endpoint named '"+model+"' is configured")
		return
	}

	var prioOverride *int
	if v := r.Header.Get("X-Priority"); v != "" {
		p, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			writeProtoError(w, proto, http.StatusBadRequest, "invalid_request_error", "invalid_priority", "X-Priority must be an integer")
			return
		}
		prioOverride = &p
	}

	key := counterKey(proto, model)
	rc := h.counters(key)
	rc.requests.Add(1)

	rt := &failoverTransport{
		h: h, st: st, ep: ep, proto: proto, model: model, prioOverride: prioOverride,
		obj: obj, body: body, rc: rc, rr: h.rrCounter(key),
	}
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
				writeProtoError(w, proto, http.StatusServiceUnavailable, "server_error", "queue_timeout",
					"timed out waiting for a free slot in pool '"+qt.pool+"'")
				return
			}
			var ae *acquireError
			if errors.As(err, &ae) {
				writeProtoError(w, proto, http.StatusInternalServerError, "server_error", "acquire_failed", ae.Error())
				return
			}
			rc.errors.Add(1)
			h.logger.Error("upstream error", "error", err)
			writeProtoError(w, proto, http.StatusBadGateway, "server_error", "upstream_error", "upstream request failed")
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

// failoverTransport sends a request to the endpoint's targets, trying further
// targets when an attempt fails before any response was produced. See the
// "Multi-pool routing" guide for the exact semantics.
type failoverTransport struct {
	h            *Handler
	st           *config.RuntimeState
	ep           config.Endpoint
	proto        string
	model        string // client-facing model (endpoint name)
	prioOverride *int   // X-Priority, when sent
	obj          map[string]json.RawMessage
	body         []byte // original body
	rc           *routeCounters
	rr           *atomic.Uint64
}

func (t *failoverTransport) priority(i int) int {
	if t.prioOverride != nil {
		return *t.prioOverride
	}
	return t.ep.QueuePriority(i)
}

// bodyFor returns the request body for target i, with the model rewritten to
// the target's target_model when it differs.
func (t *failoverTransport) bodyFor(i int) ([]byte, error) {
	tm := t.ep.Targets[i].TargetModel
	if tm == "" || tm == t.model {
		return t.body, nil
	}
	raw, _ := json.Marshal(tm)
	obj := make(map[string]json.RawMessage, len(t.obj))
	for k, v := range t.obj {
		obj[k] = v
	}
	obj["model"] = raw
	return json.Marshal(obj)
}

// order returns the remaining targets (indexes into ep.Targets) in the order
// the endpoint's mode tries them for a free slot, and whether only the first
// may be used (failover mode).
func (t *failoverTransport) order(remaining []int, rrStart int) (cands []int, firstOnly bool) {
	cands = append([]int(nil), remaining...)
	switch t.ep.Mode {
	case config.ModeFailover:
		return cands, true
	case config.ModeRoundRobin:
		n := len(t.ep.Targets)
		sort.SliceStable(cands, func(a, b int) bool {
			return (cands[a]-rrStart+n)%n < (cands[b]-rrStart+n)%n
		})
		return cands, false
	}
	// least-inflight: ascending in-flight count, ties by list order.
	active := make(map[int]int, len(cands))
	for _, i := range cands {
		active[i] = t.st.Pools[t.ep.Targets[i].Pool].Stats().Active
	}
	sort.SliceStable(cands, func(a, b int) bool { return active[cands[a]] < active[cands[b]] })
	return cands, false
}

// acquire picks a target among remaining and takes a slot on its pool. The
// first candidate (in mode order) with a free slot wins. If none has one the
// request queues: on the least loaded pool (active+queued, ties by list
// order) in least-inflight mode, on the first candidate otherwise.
func (t *failoverTransport) acquire(ctx context.Context, remaining []int, rrStart int) (idx int, release func(), wait time.Duration, err error) {
	start := time.Now()
	cands, firstOnly := t.order(remaining, rrStart)
	if firstOnly {
		cands = cands[:1]
	}
	for _, i := range cands {
		if rel, ok := t.st.Pools[t.ep.Targets[i].Pool].TryAcquire(); ok {
			return i, rel, time.Since(start), nil
		}
	}
	best := cands[0]
	if t.ep.Mode == config.ModeLeastInflight {
		bestLoad := -1
		for _, i := range remaining {
			s := t.st.Pools[t.ep.Targets[i].Pool].Stats()
			if l := s.Active + s.QueueDepth; bestLoad < 0 || l < bestLoad {
				best, bestLoad = i, l
			}
		}
	}
	name := t.ep.Targets[best].Pool
	t.rc.queued.Add(1)
	qctx, cancel := context.WithTimeout(ctx, t.st.Config.Server.QueueTimeout.Std())
	rel, aerr := t.st.Pools[name].Acquire(qctx, t.priority(best))
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
	return best, rel, time.Since(start), nil
}

func (t *failoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	remaining := make([]int, len(t.ep.Targets))
	for i := range remaining {
		remaining[i] = i
	}
	rrStart := 0
	if t.ep.Mode == config.ModeRoundRobin {
		rrStart = int((t.rr.Add(1) - 1) % uint64(len(t.ep.Targets)))
	}
	for {
		idx, release, wait, err := t.acquire(req.Context(), remaining, rrStart)
		if err != nil {
			return nil, err
		}
		name := t.ep.Targets[idx].Pool
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
		if t.proto == config.ProtocolAnthropic {
			// Anthropic upstreams authenticate with x-api-key; the client's
			// credentials are never forwarded. anthropic-version and
			// anthropic-beta pass through untouched.
			out.Header.Del("X-Api-Key")
			if pc.APIKey != "" {
				out.Header.Set("X-Api-Key", pc.APIKey)
			}
		} else if pc.APIKey != "" {
			out.Header.Del("X-Api-Key")
			out.Header.Set("Authorization", "Bearer "+pc.APIKey)
		}
		body, err := t.bodyFor(idx)
		if err != nil {
			release()
			return nil, err
		}
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.ContentLength = int64(len(body))
		out.Header.Set("Content-Length", strconv.Itoa(len(body)))
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
