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
	"reflect"
	"regexp"
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
	// Coordination, when set, shares slot limits and the priority queue between
	// several proxy pods through a Valkey/Redis-protocol server. Changing it
	// needs a restart.
	Coordination *CoordinationConfig `yaml:"coordination"`
	// Endpoints maps protocol ("anthropic" or "openai") to endpoint name (the
	// client-facing model name) to its definition.
	Endpoints map[string]map[string]Endpoint `yaml:"endpoints"`

	// LegacyRoutes catches the removed top-level "routes:" key so it fails
	// with a pointer to the new form instead of a generic decode error.
	LegacyRoutes present `yaml:"routes"`

	// Clients enables client API keys: each name is a client whose key is the
	// file of the same name in server.client_keys_dir. Empty = auth disabled.
	// An entry may be empty/null.
	Clients map[string]ClientConfig `yaml:"clients"`
}

// CoordinationConfig selects the shared-state backend.
type CoordinationConfig struct {
	Valkey *ValkeyConfig `yaml:"valkey"`
}

// ValkeyConfig configures the Valkey/Redis coordination backend.
type ValkeyConfig struct {
	Address string `yaml:"address"`
	// PasswordEnv names the environment variable holding the password; the
	// password itself never appears in the manifest.
	PasswordEnv string `yaml:"password_env"`
	KeyPrefix   string `yaml:"key_prefix"`
	// LeaseTTL is the slot lease TTL, renewed while the request runs.
	LeaseTTL Duration `yaml:"lease_ttl"`
	// FailMode is "open" (default: fall back to the local pool when the
	// server is unreachable) or "closed" (reject with 503).
	FailMode string `yaml:"fail_mode"`
}

// ClientConfig holds optional per-client settings.
type ClientConfig struct {
	// Priority is the default priority of this client's requests; an
	// X-Priority header still overrides it. Nil keeps the endpoint/target priority.
	Priority *int `yaml:"priority"`
	// Endpoints is an optional allow-list of endpoint names (the request's
	// `model`, under any protocol); omitted means every endpoint.
	Endpoints []string `yaml:"endpoints"`
}

// present records that a key was set, whatever its value.
type present bool

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *present) UnmarshalYAML(*yaml.Node) error { *p = true; return nil }

// ServerConfig holds server-wide settings.
type ServerConfig struct {
	Listen        string   `yaml:"listen"`
	QueueTimeout  Duration `yaml:"queue_timeout"`
	AgingInterval Duration `yaml:"aging_interval"`
	// ClientKeysDir, when set, enables client API keys: a directory with one
	// file per client (file name = client name, content = the key), typically
	// a mounted Secret. Empty disables authentication. The keys themselves
	// never appear in the manifest.
	ClientKeysDir string `yaml:"client_keys_dir"`
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
	UpstreamURL string `yaml:"upstream_url"`
	APIKey      string `yaml:"api_key"`
	// MaxConcurrency >= 1 limits parallel requests; 0 (also when omitted) means
	// unlimited: no slot limit, no queue. Negative values are invalid.
	MaxConcurrency int `yaml:"max_concurrency"`
	// Protocol is the API the upstream speaks: "openai" (default) or
	// "anthropic". It decides which endpoints may target the pool and how
	// the API key is injected.
	Protocol string `yaml:"protocol"`
	// ConcurrencyGroup, when set, makes every pool naming the same group share
	// ONE slot limit and ONE wait queue (across protocols). All members must
	// declare the same max_concurrency.
	ConcurrencyGroup string `yaml:"concurrency_group"`
	// Auth selects how the upstream API key is injected: "bearer"
	// (Authorization: Bearer) or "x-api-key". Default: x-api-key for
	// anthropic pools, bearer for openai pools.
	Auth string `yaml:"auth"`

	// Parsed is the parsed UpstreamURL, set by Validate.
	Parsed *url.URL `yaml:"-"`
}

// Upstream authentication schemes for PoolConfig.Auth.
const (
	AuthBearer  = "bearer"
	AuthXAPIKey = "x-api-key"
)

// AuthScheme returns the effective upstream auth scheme of the pool.
func (p PoolConfig) AuthScheme() string {
	if p.Auth != "" {
		return p.Auth
	}
	if p.Protocol == ProtocolAnthropic {
		return AuthXAPIKey
	}
	return AuthBearer
}

// Supported pool/endpoint protocols.
const (
	ProtocolOpenAI    = "openai"
	ProtocolAnthropic = "anthropic"
)

// Protocols lists the supported protocols.
var Protocols = []string{ProtocolAnthropic, ProtocolOpenAI}

// Endpoint selection modes.
const (
	// ModeLeastInflight takes a free slot on the target with the fewest
	// in-flight requests (ties: list order), else queues on the least loaded.
	ModeLeastInflight = "least-inflight"
	// ModeRoundRobin rotates the starting target per request; the first
	// target with a free slot in rotation order wins, else the request
	// queues on the rotation's first target.
	ModeRoundRobin = "round-robin"
	// ModeFailover always uses the first remaining target (queuing on it when
	// busy); later targets are only used when an attempt fails.
	ModeFailover = "failover"
)

// Modes lists the supported endpoint modes.
var Modes = []string{ModeLeastInflight, ModeRoundRobin, ModeFailover}

// Endpoint is a client-facing model name served for one protocol: an
// ordered list of targets, a selection mode and a default queue priority.
type Endpoint struct {
	// Mode is one of Modes; it defaults to least-inflight.
	Mode string `yaml:"mode"`
	// Priority is the queue priority (higher first). Defaults to 0.
	Priority int      `yaml:"priority"`
	Targets  []Target `yaml:"targets"`
}

// Target is one pool an endpoint may send requests to.
//
// TargetModel is passed upstream verbatim as an opaque string; a suffix such
// as "@medium" has no meaning to the proxy. Empty keeps the client's model.
type Target struct {
	Pool        string `yaml:"pool"`
	TargetModel string `yaml:"target_model"`
	// Priority, when set, overrides the endpoint priority for requests
	// queued on this target.
	Priority *int `yaml:"priority"`
	// WhenConcurrencyFull says what a request does when this target's pool (or
	// concurrency group) has no free slot: "wait" (default) queues on it,
	// "skip" tries the next target at once. The last target of an endpoint is
	// never skipped: a request that skipped every other target waits on it.
	WhenConcurrencyFull string `yaml:"when_concurrency_full"`
}

// When-busy behaviours of a target.
const (
	WhenConcurrencyFullWait = "wait"
	WhenConcurrencyFullSkip = "skip"
)

// Skips reports whether the target is configured with when_concurrency_full: skip.
func (t Target) Skips() bool { return t.WhenConcurrencyFull == WhenConcurrencyFullSkip }

// QueuePriority returns the queue priority of target i of e.
func (e Endpoint) QueuePriority(i int) int {
	if p := e.Targets[i].Priority; p != nil {
		return *p
	}
	return e.Priority
}

// Endpoint returns the endpoint named name for protocol.
func (c *Config) Endpoint(protocol, name string) (Endpoint, bool) {
	e, ok := c.Endpoints[protocol][name]
	return e, ok
}

// EndpointNames returns the sorted endpoint names of protocol.
func (c *Config) EndpointNames(protocol string) []string {
	names := make([]string, 0, len(c.Endpoints[protocol]))
	for n := range c.Endpoints[protocol] {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// EndpointCount returns the number of endpoints over all protocols.
func (c *Config) EndpointCount() int {
	n := 0
	for _, eps := range c.Endpoints {
		n += len(eps)
	}
	return n
}

// Warnings returns non-fatal remarks about a valid config. When both
// protocols define endpoints, a name present under only one of them is
// reported: clients of the other API get 404 endpoint_not_configured for it.
// A config serving a single protocol produces no such warning.
func (c *Config) Warnings() []string {
	var out []string
	if v := c.valkey(); v != nil && v.LeaseTTL < Duration(3*time.Second) {
		out = append(out, fmt.Sprintf("coordination.valkey.lease_ttl %s is below 3s: a slow Valkey round trip may expire live leases", v.LeaseTTL.Std()))
	}
	for _, proto := range Protocols {
		for _, n := range c.EndpointNames(proto) {
			e := c.Endpoints[proto][n]
			if last := len(e.Targets) - 1; e.Targets[last].Skips() {
				out = append(out, fmt.Sprintf("endpoint %q (%s): the last target has when_concurrency_full: skip, which has no effect there: the last target is never skipped and requests wait on it", n, proto))
			}
		}
	}
	a, o := c.Endpoints[ProtocolAnthropic], c.Endpoints[ProtocolOpenAI]
	if len(a) == 0 || len(o) == 0 {
		return out
	}
	for _, n := range c.EndpointNames(ProtocolAnthropic) {
		if _, ok := o[n]; !ok {
			out = append(out, fmt.Sprintf("endpoint %q exists under anthropic but not openai: OpenAI clients get 404 for it", n))
		}
	}
	for _, n := range c.EndpointNames(ProtocolOpenAI) {
		if _, ok := a[n]; !ok {
			out = append(out, fmt.Sprintf("endpoint %q exists under openai but not anthropic: Anthropic clients get 404 for it", n))
		}
	}
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

func (c *Config) valkey() *ValkeyConfig {
	if c.Coordination == nil {
		return nil
	}
	return c.Coordination.Valkey
}

func (c *Config) validateCoordination() error {
	if c.Coordination == nil {
		return nil
	}
	v := c.Coordination.Valkey
	if v == nil {
		return errors.New("coordination: a backend is required (coordination.valkey)")
	}
	if strings.TrimSpace(v.Address) == "" {
		return errors.New("coordination.valkey.address is required")
	}
	if v.KeyPrefix == "" {
		v.KeyPrefix = "lpm"
	}
	if v.LeaseTTL == 0 {
		v.LeaseTTL = Duration(30 * time.Second)
	}
	if v.LeaseTTL < 0 {
		return errors.New("coordination.valkey.lease_ttl must be positive")
	}
	switch v.FailMode {
	case "":
		v.FailMode = "open"
	case "open", "closed":
	default:
		return fmt.Errorf("coordination.valkey.fail_mode must be open or closed, got %q", v.FailMode)
	}
	return nil
}

func (c *Config) validate() error {
	if err := c.validateCoordination(); err != nil {
		return err
	}
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
	if c.LegacyRoutes {
		return errors.New(legacyRoutesError)
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
		if p.MaxConcurrency < 0 {
			return fmt.Errorf("pool %q: max_concurrency must be >= 0 (0 = unlimited)", name)
		}
		switch p.Protocol {
		case "":
			p.Protocol = ProtocolOpenAI
		case ProtocolOpenAI, ProtocolAnthropic:
		default:
			return fmt.Errorf("pool %q: unknown protocol %q (want anthropic or openai)", name, p.Protocol)
		}
		switch p.Auth {
		case "", AuthBearer, AuthXAPIKey:
		default:
			return fmt.Errorf("pool %q: unknown auth %q (want bearer or x-api-key)", name, p.Auth)
		}
		u, err := url.Parse(p.UpstreamURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			// The value is deliberately not echoed: it may embed credentials.
			return fmt.Errorf("pool %q: invalid upstream_url (must be an absolute http(s) URL)", name)
		}
		p.Parsed = u
		c.Pools[name] = p
	}
	groupMax := map[string]string{} // group -> first member
	for _, name := range sortedPoolNames(c.Pools) {
		p := c.Pools[name]
		if p.ConcurrencyGroup == "" {
			continue
		}
		if first, ok := groupMax[p.ConcurrencyGroup]; ok {
			if c.Pools[first].MaxConcurrency != p.MaxConcurrency {
				return fmt.Errorf("pool %q: concurrency_group %q members must declare the same max_concurrency (%q has %d, %q has %d)",
					name, p.ConcurrencyGroup, first, c.Pools[first].MaxConcurrency, name, p.MaxConcurrency)
			}
		} else {
			groupMax[p.ConcurrencyGroup] = name
		}
	}
	for proto, eps := range c.Endpoints {
		if proto != ProtocolOpenAI && proto != ProtocolAnthropic {
			return fmt.Errorf("endpoints: unknown protocol %q (want anthropic or openai)", proto)
		}
		for name, e := range eps {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("endpoints.%s: endpoint name must not be empty", proto)
			}
			if err := c.validateEndpoint(fmt.Sprintf("endpoints.%s.%s", proto, name), proto, &e); err != nil {
				return err
			}
			eps[name] = e
		}
	}
	if len(c.Clients) > 0 && c.Server.ClientKeysDir == "" {
		return errors.New("clients requires server.client_keys_dir (the directory holding one key file per client)")
	}
	for name, cl := range c.Clients {
		if !clientName.MatchString(name) {
			return fmt.Errorf("client %q: name must match %s (it is also the key file name)", name, clientName)
		}
		for _, en := range cl.Endpoints {
			_, a := c.Endpoints[ProtocolAnthropic][en]
			_, o := c.Endpoints[ProtocolOpenAI][en]
			if !a && !o {
				return fmt.Errorf("client %q: endpoints references unknown endpoint %q (not under endpoints.anthropic nor endpoints.openai)", name, en)
			}
		}
	}
	return nil
}

// legacyRoutesError explains the removal of the top-level routes key.
const legacyRoutesError = "the top-level `routes:` key was removed: declare endpoints per protocol instead, e.g.\n" +
	"  endpoints:\n" +
	"    openai:      # or anthropic (/v1/messages)\n" +
	"      <name>:    # the model name clients send\n" +
	"        mode: least-inflight   # or round-robin, failover\n" +
	"        priority: 50\n" +
	"        targets:\n" +
	"          - {pool: <pool>, target_model: <upstream model>}\n" +
	"see https://llm-proxy-manifestd.lerenn.dev/guide/protocol-endpoints"

var clientName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Allows reports whether the client may use the given endpoint name.
func (cl ClientConfig) Allows(name string) bool {
	if len(cl.Endpoints) == 0 {
		return true
	}
	for _, e := range cl.Endpoints {
		if e == name {
			return true
		}
	}
	return false
}

func (c *Config) validateEndpoint(label, proto string, e *Endpoint) error {
	switch e.Mode {
	case "":
		e.Mode = ModeLeastInflight
	case ModeLeastInflight, ModeRoundRobin, ModeFailover:
	default:
		return fmt.Errorf("%s: unknown mode %q (want %s)", label, e.Mode, strings.Join(Modes, ", "))
	}
	if len(e.Targets) == 0 {
		return fmt.Errorf("%s: targets must list at least one {pool, target_model}", label)
	}
	type key struct{ pool, model string }
	seen := make(map[key]bool, len(e.Targets))
	for i, t := range e.Targets {
		if t.Pool == "" {
			return fmt.Errorf("%s: targets[%d]: pool is required", label, i)
		}
		pc, ok := c.Pools[t.Pool]
		if !ok {
			return fmt.Errorf("%s: targets[%d]: unknown pool %q", label, i, t.Pool)
		}
		if pc.Protocol != proto {
			return fmt.Errorf("%s: targets[%d]: pool %q has protocol %q, want %q", label, i, t.Pool, pc.Protocol, proto)
		}
		switch t.WhenConcurrencyFull {
		case "":
			e.Targets[i].WhenConcurrencyFull = WhenConcurrencyFullWait
		case WhenConcurrencyFullWait, WhenConcurrencyFullSkip:
		default:
			return fmt.Errorf("%s: targets[%d]: unknown when_concurrency_full %q (want %s or %s)", label, i, t.WhenConcurrencyFull, WhenConcurrencyFullWait, WhenConcurrencyFullSkip)
		}
		k := key{t.Pool, t.TargetModel}
		if seen[k] {
			return fmt.Errorf("%s: targets[%d]: duplicate target (pool %q, target_model %q)", label, i, t.Pool, t.TargetModel)
		}
		seen[k] = true
	}
	return nil
}

// RuntimeState is an immutable snapshot of the active configuration together
// with the live pool instances. Requests load it once and keep using it, so
// in-flight requests release on the pool they acquired from.
type RuntimeState struct {
	Config *Config
	Pools  map[string]*pool.Pool
	// Coordinator is the shared-state backend, nil without coordination.
	Coordinator *pool.Coordinator
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
	if prev != nil && !reflect.DeepEqual(prev.Config.Coordination, cfg.Coordination) {
		l.logger.Warn("coordination changed; requires restart, ignoring")
		cfg.Coordination = prev.Config.Coordination
	}
	l.state.Store(buildState(cfg, prev, l.logger))
	l.lastRaw = raw
	l.logger.Info("config loaded", "path", l.path, "pools", len(cfg.Pools), "endpoints", cfg.EndpointCount())
	return nil
}

// buildState creates the runtime state, reusing pool instances (and thus
// their semaphores and queues) from prev when the upstream is unchanged.
// Concurrency and aging changes are applied to the live pool.
func buildState(cfg *Config, prev *RuntimeState, logger *slog.Logger) *RuntimeState {
	aging := cfg.Server.AgingInterval.Std()
	st := &RuntimeState{Config: cfg, Pools: make(map[string]*pool.Pool, len(cfg.Pools)), Generation: 1, LoadedAt: time.Now()}
	if prev != nil {
		st.Generation = prev.Generation + 1
	}
	switch {
	case prev != nil:
		st.Coordinator = prev.Coordinator
	case cfg.valkey() != nil:
		v := cfg.valkey()
		st.Coordinator = pool.NewCoordinator(pool.CoordConfig{
			Address: v.Address, Password: os.Getenv(v.PasswordEnv), KeyPrefix: v.KeyPrefix,
			LeaseTTL: v.LeaseTTL.Std(), FailClosed: v.FailMode == "closed", Logger: logger,
		})
	}
	newPool := func(name, group string, max int) *pool.Pool {
		if st.Coordinator == nil {
			return pool.New(max, aging)
		}
		key := "pool:" + name
		if group != "" {
			key = "group:" + group
		}
		return pool.NewShared(max, aging, st.Coordinator, key)
	}
	groups := map[string]*pool.Pool{} // instances shared by concurrency groups
	for _, name := range sortedPoolNames(cfg.Pools) {
		pc := cfg.Pools[name]
		if g := pc.ConcurrencyGroup; g != "" {
			if p, ok := groups[g]; ok {
				st.Pools[name] = p
				continue
			}
			var p *pool.Pool
			if prev != nil {
				// Keep the live group (slots, queue) if any member of it existed.
				for pn, opc := range prev.Config.Pools {
					if opc.ConcurrencyGroup == g {
						p = prev.Pools[pn]
						break
					}
				}
			}
			if p != nil {
				p.Resize(pc.MaxConcurrency, aging)
			} else {
				p = newPool(name, g, pc.MaxConcurrency)
			}
			groups[g] = p
			st.Pools[name] = p
			continue
		}
		if prev != nil {
			if old, ok := prev.Pools[name]; ok {
				oc := prev.Config.Pools[name]
				if oc.ConcurrencyGroup == "" && oc.UpstreamURL == pc.UpstreamURL && oc.APIKey == pc.APIKey {
					old.Resize(pc.MaxConcurrency, aging)
					st.Pools[name] = old
					continue
				}
			}
		}
		st.Pools[name] = newPool(name, "", pc.MaxConcurrency)
	}
	return st
}

func sortedPoolNames(m map[string]PoolConfig) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
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
