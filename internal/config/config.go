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
	Server ServerConfig          `yaml:"server"`
	Pools  map[string]PoolConfig `yaml:"pools"`
	Routes map[string]Route      `yaml:"routes"`
}

// ServerConfig holds server-wide settings.
type ServerConfig struct {
	Listen        string   `yaml:"listen"`
	QueueTimeout  Duration `yaml:"queue_timeout"`
	AgingInterval Duration `yaml:"aging_interval"`
}

// PoolConfig describes one upstream account.
type PoolConfig struct {
	UpstreamURL    string `yaml:"upstream_url"`
	APIKey         string `yaml:"api_key"`
	MaxConcurrency int    `yaml:"max_concurrency"`

	// Parsed is the parsed UpstreamURL, set by Validate.
	Parsed *url.URL `yaml:"-"`
}

// Route maps a client-facing model name to a pool.
type Route struct {
	Pool        string `yaml:"pool"`
	TargetModel string `yaml:"target_model"`
	Priority    int    `yaml:"priority"`
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
	if len(c.Pools) == 0 {
		return errors.New("at least one pool is required")
	}
	for name, p := range c.Pools {
		if p.MaxConcurrency < 1 {
			return fmt.Errorf("pool %q: max_concurrency must be >= 1", name)
		}
		u, err := url.Parse(p.UpstreamURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("pool %q: invalid upstream_url %q", name, p.UpstreamURL)
		}
		p.Parsed = u
		c.Pools[name] = p
	}
	for name, r := range c.Routes {
		if _, ok := c.Pools[r.Pool]; !ok {
			return fmt.Errorf("route %q: unknown pool %q", name, r.Pool)
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
}

// Loader owns the active RuntimeState and reloads it from disk.
type Loader struct {
	path   string
	logger *slog.Logger
	state  atomic.Pointer[RuntimeState]

	mu      sync.Mutex // serialises reloads
	lastRaw []byte
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

// Reload reads and applies the config file. On any error the previous state
// is kept and the error returned.
func (l *Loader) Reload() error {
	l.mu.Lock()
	defer l.mu.Unlock()

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
	st := &RuntimeState{Config: cfg, Pools: make(map[string]*pool.Pool, len(cfg.Pools))}
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
