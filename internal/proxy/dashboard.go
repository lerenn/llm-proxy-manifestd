package proxy

import (
	"net"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/ui"
)

type routeCounters struct {
	inflight atomic.Int64
	queued   atomic.Int64
	requests atomic.Int64
	errors   atomic.Int64
}

func (h *Handler) counters(route string) *routeCounters {
	if v, ok := h.routes.Load(route); ok {
		return v.(*routeCounters)
	}
	v, _ := h.routes.LoadOrStore(route, &routeCounters{})
	return v.(*routeCounters)
}

// Dashboard JSON shapes. Everything here is served publicly: only names,
// upstream host names, counts and timings may appear. Never add credentials,
// full URLs, bodies or headers.
type dashboardState struct {
	Status        string           `json:"status"`
	Version       string           `json:"version"`
	Now           time.Time        `json:"now"`
	UptimeSeconds int64            `json:"uptime_seconds"`
	Config        dashboardConfig  `json:"config"`
	Totals        dashboardTotals  `json:"totals"`
	Pools         []dashboardPool  `json:"pools"`
	Routes        []dashboardRoute `json:"routes"`
}

type dashboardConfig struct {
	Generation   int        `json:"generation"`
	LoadedAt     time.Time  `json:"loaded_at"`
	LastAttempt  *time.Time `json:"last_reload_attempt,omitempty"`
	ReloadFailed bool       `json:"last_reload_failed"`
}

type dashboardTotals struct {
	Slots    int   `json:"slots"`
	Active   int   `json:"active_slots"`
	Queued   int   `json:"queued"`
	InFlight int64 `json:"in_flight"`
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
}

type dashboardQueue struct {
	Priority int `json:"priority"`
	Count    int `json:"count"`
}

type dashboardPool struct {
	Name           string           `json:"name"`
	Protocol       string           `json:"protocol"`
	Upstream       string           `json:"upstream"`
	MaxConcurrency int              `json:"max_concurrency"`
	Active         int              `json:"active_slots"`
	Queued         int              `json:"queued"`
	QueueByPrio    []dashboardQueue `json:"queue_by_priority"`
	TotalServed    int64            `json:"total_served"`
}

type dashboardEndpoint struct {
	Protocol  string   `json:"protocol"`
	Pools     []string `json:"pools"`
	Upstreams []string `json:"upstreams"`
	Target    string   `json:"target_model,omitempty"`
	Priority  int      `json:"priority"`
}

type dashboardRoute struct {
	Name string `json:"name"`
	// Pool and Upstream describe the first pool, kept for compatibility.
	Pool      string   `json:"pool"`
	Upstream  string   `json:"upstream"`
	Pools     []string `json:"pools"`
	Upstreams []string `json:"upstreams"`
	Target    string   `json:"target_model,omitempty"`
	Priority  int      `json:"priority"`
	// Endpoints lists the per-protocol endpoints; the flat fields above
	// mirror the openai endpoint (or the first one when there is none).
	Endpoints []dashboardEndpoint `json:"endpoints"`
	InFlight  int64               `json:"in_flight"`
	Queued    int64               `json:"queued"`
	Requests  int64               `json:"requests"`
	Errors    int64               `json:"errors"`
}

func (h *Handler) dashboardSnapshot() (dashboardState, bool) {
	st := h.state()
	if st == nil {
		return dashboardState{}, false
	}
	now := time.Now()
	out := dashboardState{
		Status:        "ok",
		Version:       h.version,
		Now:           now.UTC(),
		UptimeSeconds: int64(now.Sub(h.started).Seconds()),
		Config:        dashboardConfig{Generation: st.Generation, LoadedAt: st.LoadedAt.UTC()},
		Pools:         []dashboardPool{},
		Routes:        []dashboardRoute{},
	}
	if h.reloadStatus != nil {
		if rs := h.reloadStatus(); !rs.LastAttempt.IsZero() {
			t := rs.LastAttempt.UTC()
			out.Config.LastAttempt = &t
			out.Config.ReloadFailed = rs.Failed
			if rs.Failed {
				out.Status = "degraded"
			}
		}
	}

	upstream := map[string]string{}
	names := make([]string, 0, len(st.Pools))
	for n := range st.Pools {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := st.Pools[n]
		s := p.Stats()
		host := ""
		if pc, ok := st.Config.Pools[n]; ok && pc.Parsed != nil {
			host = pc.Parsed.Host // host[:port] only: no userinfo, path or query
		}
		upstream[n] = host
		prios := []dashboardQueue{}
		for prio, c := range p.QueueByPriority() {
			prios = append(prios, dashboardQueue{prio, c})
		}
		sort.Slice(prios, func(i, j int) bool { return prios[i].Priority > prios[j].Priority })
		out.Pools = append(out.Pools, dashboardPool{
			Name: n, Protocol: st.Config.Pools[n].Protocol, Upstream: host, MaxConcurrency: s.MaxConcurrency, Active: s.Active,
			Queued: s.QueueDepth, QueueByPrio: prios, TotalServed: s.TotalServed,
		})
		out.Totals.Slots += s.MaxConcurrency
		out.Totals.Active += s.Active
		out.Totals.Queued += s.QueueDepth
	}
	for _, n := range st.Config.RouteNames() {
		r := st.Config.Routes[n]
		rc := h.counters(n)
		var eps []dashboardEndpoint
		for _, proto := range r.EndpointProtocols() {
			e := r.Endpoints[proto]
			ups := make([]string, len(e.Pools))
			for i, pn := range e.Pools {
				ups[i] = upstream[pn]
			}
			eps = append(eps, dashboardEndpoint{Protocol: proto, Pools: e.Pools, Upstreams: ups, Target: e.TargetModel, Priority: e.Priority})
		}
		first := eps[0]
		for _, e := range eps {
			if e.Protocol == "openai" {
				first = e
			}
		}
		dr := dashboardRoute{
			Name: n, Pool: first.Pools[0], Upstream: first.Upstreams[0], Pools: first.Pools, Upstreams: first.Upstreams,
			Target: first.Target, Priority: first.Priority, Endpoints: eps,
			InFlight: rc.inflight.Load(), Queued: rc.queued.Load(),
			Requests: rc.requests.Load(), Errors: rc.errors.Load(),
		}
		out.Routes = append(out.Routes, dr)
		out.Totals.InFlight += dr.InFlight
		out.Totals.Requests += dr.Requests
		out.Totals.Errors += dr.Errors
	}
	return out, true
}

func (h *Handler) newDashboard() http.Handler {
	mux := http.NewServeMux()
	static := http.FileServerFS(ui.FS)
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		snap, ok := h.dashboardSnapshot()
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, snap)
	})
	mux.Handle("GET /", static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		mux.ServeHTTP(w, r)
	})
}

// serveDashboard handles dashboard requests and reports whether it did. On
// the dashboard host only dashboard paths are served, so the proxy API is
// never reachable through it; on other hosts only the path prefix is claimed,
// so /v1/... is never shadowed.
func (h *Handler) serveDashboard(w http.ResponseWriter, r *http.Request) bool {
	st := h.state()
	if st == nil {
		return false
	}
	dc := st.Config.Dashboard
	if !dc.IsEnabled() {
		return false
	}
	host := strings.ToLower(r.Host)
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	onHost := (dc.Host != "" && host == dc.Host) || (dc.Host == "" && strings.HasPrefix(host, "dashboard."))
	if onHost {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		h.dashboard.ServeHTTP(w, r)
		return true
	}
	prefix := dc.PathPrefix()
	if prefix == "" {
		return false
	}
	if r.URL.Path == prefix {
		http.Redirect(w, r, prefix+"/", http.StatusMovedPermanently)
		return true
	}
	if strings.HasPrefix(r.URL.Path, prefix+"/") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		http.StripPrefix(prefix, h.dashboard).ServeHTTP(w, r)
		return true
	}
	return false
}
