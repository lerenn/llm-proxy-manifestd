// Package config loads, validates and hot-reloads the manifest.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"

	"github.com/lerenn/llm-proxy-manifestd/internal/pool"
)

// Duration is a time.Duration that unmarshals from strings such as "120s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the parsed manifest.
type Config struct {
	Server    ServerConfig          `yaml:"server"`
	Dashboard DashboardConfig       `yaml:"dashboard"`
	Pools     map[string]PoolConfig `yaml:"pools"`
	Routes    map[string]Route      `yaml:"routes"`
}

// ServerConfig holds server-wide settings.
type ServerConfig struct {
	Listen        string   `yaml:"listen"`
	QueueTimeout  Duration `yaml:"queue_timeout"`
	AgingInterval Duration `yaml:"aging_interval"`
}

// DashboardConfig configures the public read-only dashboard.
type DashboardConfig struct {
	// Enabled defaults to true when unset.
	Enabled *bool `yaml:"enabled"`
	// Host, when set, is the exact hostname (port ignored) serving the
	// dashboard at "/". When empty, any host starting with "dashboard." does.
	Host string `yaml:"host"`
	// Path is the prefix serving the dashboard on every host. Defaults to
	// "/dashboard"; "off" disables the path mount.
	Path string `yaml:"path"`
}

// IsEnabled reports whether the dashboard is enabled.
func (d DashboardConfig) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// PathPrefix returns the mount prefix without trailing slash, or "" if the
// path mount is disabled.
func (d DashboardConfig) PathPrefix() string {
	if d.Path == "off" {
		return ""
	}
	return d.Path
}

// PoolConfig describes one upstream account.
type PoolConfig struct {
	UpstreamURL    string `yaml:"upstream_url"`
	APIKey         string `yaml:"api_key"`
	MaxConcurrency int    `yaml:"max_concurrency"`
	// Protocol is the API the upstream speaks: "openai" (default) or
	// "anthropic". It decides which route endpoint may use the pool and how
	// the API key is injected.
	Protocol string `yaml:"protocol"`

	// Parsed is the parsed UpstreamURL, set by Validate.
	Parsed *url.URL `yaml:"-"`
}

// Supported pool/endpoint protocols.
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
)

// Protocols lists the supported protocols.
var Protocols = []string{ProtocolAnthropic, ProtocolOpenAI}

// Endpoint is how a route is served for one protocol: an ordered list of pools
// (all of that protocol), the model name sent upstream and a queue priority.
//
// TargetModel is passed upstream verbatim as an opaque string; a suffix such
// as "@medium" has no meaning to the proxy.
type Endpoint struct {
	Pool        string   `yaml:"pool"`
	Pools       []string `yaml:"pools"`
	TargetModel string   `yaml:"target_model"`
	Priority    int      `yaml:"priority"`
}

// Route maps a client-facing model name to per-protocol endpoints. Either use
// the shorthand (pool/pools, target_model, priority), which defines the
// "openai" endpoint, or the explicit Endpoints map, never both. After
// validation Endpoints is always populated and the shorthand fields are
// cleared.
type Route struct {
	Pool        string              `yaml:"pool"`
	Pools       []string            `yaml:"pools"`
	TargetModel string              `yaml:"target_model"`
	Priority    int                 `yaml:"priority"`
	Endpoints   map[string]Endpoint `yaml:"endpoints"`
}

// Endpoint returns the route's endpoint for protocol.
func (r Route) Endpoint(protocol string) (Endpoint, bool) {
	e, ok := r.Endpoints[protocol]
	return e, ok
}

// EndpointProtocols returns the protocols the route serves, sorted.
func (r Route) EndpointProtocols() []string {
	out := make([]string, 0, len(r.Endpoints))
	for p := range r.Endpoints {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// LoadFile reads and parses the manifest at path. It is the single entry point
// used by both the proxy and the checker binary. Environment variables are
// expanded as in Parse.
func LoadFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw)
}

// Parse expands environment variables in raw, unmarshals and validates it.
func Parse(raw []byte) (*Config, error) {
	expanded := os.ExpandEnv(string(raw))
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader([]byte(expanded)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.QueueTimeout == 0 {
		c.Server.QueueTimeout = Duration(120 * time.Second)
	}
	if c.Server.QueueTimeout < 0 {
		return errors.New("server.queue_timeout must be positive")
	}
	if c.Server.AgingInterval < 0 {
		return errors.New("server.aging_interval must not be negative")
	}
	d := &c.Dashboard
	if d.Path == "" {
		d.Path = "/dashboard"
	}
	if d.Path != "off" {
		d.Path = strings.TrimRight(d.Path, "/")
		if !strings.HasPrefix(d.Path, "/") {
			return errors.New(`dashboard.path must start with "/" (or be "off")`)
		}
		for _, reserved := range []string{"/v1", "/healthz", "/status"} {
			if d.Path == reserved || strings.HasPrefix(d.Path, reserved+"/") {
				return fmt.Errorf("dashboard.path %q conflicts with %s", d.Path, reserved)
			}
		}
	}
	d.Host = strings.ToLower(strings.TrimSpace(d.Host))
	if len(c.Pools) == 0 {
		return errors.New("at least one pool is required")
	}
	for name, p := range c.Pools {
		if p.MaxConcurrency < 1 {
			return fmt.Errorf("pool %q: max_concurrency must be >= 1", name)
		}
		switch p.Protocol {
		case "":
			p.Protocol = ProtocolOpenAI
		case ProtocolOpenAI, ProtocolAnthropic:
		default:
			return fmt.Errorf("pool %q: unknown protocol %q (want anthropic or openai)", name, p.Protocol)
		}
		u, err := url.Parse(p.UpstreamURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			// The value is deliberately not echoed: it may embed credentials.
			return fmt.Errorf("pool %q: invalid upstream_url (must be an absolute http(s) URL)", name)
		}
		p.Parsed = u
		c.Pools[name] = p
	}
	for name, r := range c.Routes {
		short := r.Pool != "" || r.Pools != nil || r.TargetModel != "" || r.Priority != 0
		switch {
		case short && r.Endpoints != nil:
			return fmt.Errorf("route %q: endpoints cannot be combined with pool/pools/target_model/priority", name)
		case r.Endpoints == nil:
			if !short {
				return fmt.Errorf("route %q: one of pool, pools or endpoints is required", name)
			}
			r.Endpoints = map[string]Endpoint{ProtocolOpenAI: {
				Pool: r.Pool, Pools: r.Pools, TargetModel: r.TargetModel, Priority: r.Priority,
			}}
		case len(r.Endpoints) == 0:
			return fmt.Errorf("route %q: endpoints must not be empty", name)
		}
		for proto, e := range r.Endpoints {
			if proto != ProtocolOpenAI && proto != ProtocolAnthropic {
				return fmt.Errorf("route %q: unknown endpoint protocol %q (want anthropic or openai)", name, proto)
			}
			if err := c.validateEndpoint(fmt.Sprintf("route %q endpoint %q", name, proto), proto, &e); err != nil {
				return err
			}
			r.Endpoints[proto] = e
		}
		r.Pool, r.Pools, r.TargetModel, r.Priority = "", nil, "", 0
		c.Routes[name] = r
	}
	return nil
}

func (c *Config) validateEndpoint(label, proto string, e *Endpoint) error {
	switch {
	case e.Pool != "" && e.Pools != nil:
		return fmt.Errorf("%s: pool and pools are mutually exclusive", label)
	case e.Pools != nil && len(e.Pools) == 0:
		return fmt.Errorf("%s: pools must not be empty", label)
	case e.Pool == "" && e.Pools == nil:
		return fmt.Errorf("%s: one of pool or pools is required", label)
	case e.Pools == nil:
		e.Pools = []string{e.Pool}
	}
	e.Pool = ""
	seen := make(map[string]bool, len(e.Pools))
	for _, pn := range e.Pools {
		pc, ok := c.Pools[pn]
		if !ok {
			return fmt.Errorf("%s: unknown pool %q", label, pn)
		}
		if seen[pn] {
			return fmt.Errorf("%s: duplicate pool %q", label, pn)
		}
		seen[pn] = true
		if pc.Protocol != proto {
			return fmt.Errorf("%s: pool %q has protocol %q, want %q", label, pn, pc.Protocol, proto)
		}
	}
	return nil
}

// RouteNames returns the sorted route names.
func (c *Config) RouteNames() []string {
	names := make([]string, 0, len(c.Routes))
	for n := range c.Routes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RuntimeState is an immutable snapshot of the active configuration together
// with the live pool instances. Requests load it once and keep using it, so
// in-flight requests release on the pool they acquired from.
type RuntimeState struct {
	Config *Config
	Pools  map[string]*pool.Pool
	// Generation increments on every applied config; LoadedAt is when it was
	// applied.
	Generation int
	LoadedAt   time.Time
}

// ReloadStatus describes the last reload attempt. It deliberately carries no
// error text, which may echo config content.
type ReloadStatus struct {
	LastAttempt time.Time
	Failed      bool
}

// Loader owns the active RuntimeState and reloads it from disk.
type Loader struct {
	path   string
	logger *slog.Logger
	state  atomic.Pointer[RuntimeState]

	mu      sync.Mutex // serialises reloads
	lastRaw []byte
	status  atomic.Pointer[ReloadStatus]
}

// NewLoader creates a loader for the config file at path.
func NewLoader(path string, logger *slog.Logger) *Loader {
	if logger == nil {
		logger = slog.Default()
	}
	return &Loader{path: path, logger: logger}
}

// State returns the active state, or nil if no valid config was loaded yet.
func (l *Loader) State() *RuntimeState { return l.state.Load() }

// ReloadStatus returns the outcome of the last reload attempt (zero if none).
func (l *Loader) ReloadStatus() ReloadStatus {
	if s := l.status.Load(); s != nil {
		return *s
	}
	return ReloadStatus{}
}

// Reload reads and applies the config file. On any error the previous state
// is kept and the error returned.
func (l *Loader) Reload() (err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	defer func() { l.status.Store(&ReloadStatus{LastAttempt: time.Now(), Failed: err != nil}) }()

	raw, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if l.lastRaw != nil && bytes.Equal(raw, l.lastRaw) {
		return nil
	}
	cfg, err := Parse(raw)
	if err != nil {
		return err
	}
	prev := l.state.Load()
	if prev != nil && prev.Config.Server.Listen != cfg.Server.Listen {
		l.logger.Warn("server.listen changed; requires restart, ignoring",
			"old", prev.Config.Server.Listen, "new", cfg.Server.Listen)
		cfg.Server.Listen = prev.Config.Server.Listen
	}
	l.state.Store(buildState(cfg, prev))
	l.lastRaw = raw
	l.logger.Info("config loaded", "path", l.path, "pools", len(cfg.Pools), "routes", len(cfg.Routes))
	return nil
}

// buildState creates the runtime state, reusing pool instances (and thus
// their semaphores and queues) from prev when the upstream is unchanged.
// Concurrency and aging changes are applied to the live pool.
func buildState(cfg *Config, prev *RuntimeState) *RuntimeState {
	aging := cfg.Server.AgingInterval.Std()
	st := &RuntimeState{Config: cfg, Pools: make(map[string]*pool.Pool, len(cfg.Pools)), Generation: 1, LoadedAt: time.Now()}
	if prev != nil {
		st.Generation = prev.Generation + 1
	}
	for name, pc := range cfg.Pools {
		if prev != nil {
			if old, ok := prev.Pools[name]; ok {
				oc := prev.Config.Pools[name]
				if oc.UpstreamURL == pc.UpstreamURL && oc.APIKey == pc.APIKey {
					old.Resize(pc.MaxConcurrency, aging)
					st.Pools[name] = old
					continue
				}
			}
		}
		st.Pools[name] = pool.New(pc.MaxConcurrency, aging)
	}
	return st
}

// Watch reloads the config on file changes until ctx is done. It watches the
// parent directory so Kubernetes' atomic ..data symlink swaps are detected.
func (l *Loader) Watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := w.Add(filepath.Dir(l.path)); err != nil {
		return err
	}

	const debounce = 200 * time.Millisecond
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0 {
				timer.Reset(debounce)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			l.logger.Error("config watcher error", "error", err)
		case <-timer.C:
			if err := l.Reload(); err != nil {
				l.logger.Error("config reload rejected, keeping previous config", "error", err)
			}
		}
	}
}
