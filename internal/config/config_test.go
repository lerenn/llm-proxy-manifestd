package config

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const valid = `
server:
  listen: ":9090"
  queue_timeout: "30s"
  aging_interval: "5s"
pools:
  a:
    upstream_url: http://up:8317
    api_key: "${TEST_KEY}"
    max_concurrency: 2
endpoints:
  openai:
    m:
      priority: 10
      targets:
        - pool: a
          target_model: gpt-x
`

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParseExpandsEnv(t *testing.T) {
	t.Setenv("TEST_KEY", "secret")
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pools["a"].APIKey != "secret" {
		t.Fatalf("api key = %q", c.Pools["a"].APIKey)
	}
	if c.Server.QueueTimeout.Std() != 30*time.Second || c.Server.AgingInterval.Std() != 5*time.Second {
		t.Fatalf("durations: %+v", c.Server)
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte("pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != ":8080" || c.Server.QueueTimeout.Std() != 120*time.Second {
		t.Fatalf("defaults: %+v", c.Server)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		"unknown pool":  "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: b}]}\n",
		"negative conc": "pools:\n  a: {upstream_url: 'http://x', max_concurrency: -1}\n",
		"bad url":       "pools:\n  a: {upstream_url: 'nope', max_concurrency: 1}\n",
		"bad duration":  "server: {queue_timeout: 'abc'}\npools:\n  a: {upstream_url: 'http://x', max_concurrency: 1}\n",
		"no pools":      "server: {}\n",
		"unknown field": "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1, bogus: 1}\n",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReloadKeepsPreviousOnInvalid(t *testing.T) {
	t.Setenv("TEST_KEY", "k")
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, valid)
	l := NewLoader(path, quiet())
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	first := l.State()
	write(t, path, "pools: {a: {upstream_url: 'bad', max_concurrency: 1}}")
	if err := l.Reload(); err == nil {
		t.Fatal("expected rejection")
	}
	if l.State() != first {
		t.Fatal("state replaced by invalid config")
	}
}

func TestReloadPreservesCompatiblePools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, valid)
	l := NewLoader(path, quiet())
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	first := l.State()
	rel, err := first.Pools["a"].Acquire(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	// Same upstream, new concurrency and a new route: pool instance kept.
	write(t, path, strings.Replace(valid, "max_concurrency: 2", "max_concurrency: 5", 1))
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	second := l.State()
	if second == first || second.Pools["a"] != first.Pools["a"] {
		t.Fatal("pool instance should be preserved")
	}
	if s := second.Pools["a"].Stats(); s.MaxConcurrency != 5 || s.Active != 1 {
		t.Fatalf("stats %+v", s)
	}

	// Upstream change: new pool; old release must not panic or affect it.
	write(t, path, strings.Replace(valid, "http://up:8317", "http://other:1", 1))
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	third := l.State()
	if third.Pools["a"] == first.Pools["a"] {
		t.Fatal("pool should be replaced when upstream changes")
	}
	rel()
	if a := first.Pools["a"].Stats().Active; a != 0 {
		t.Fatalf("old pool active = %d", a)
	}
	if a := third.Pools["a"].Stats().Active; a != 0 {
		t.Fatalf("new pool active = %d", a)
	}
}

func TestWatchDetectsSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	// Emulate Kubernetes: config.yaml -> ..data/config.yaml, ..data -> ..v1
	mk := func(ver, content string) {
		d := filepath.Join(dir, ver)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(d, "config.yaml"), content)
	}
	mk("..v1", valid)
	mk("..v2", strings.Replace(valid, "max_concurrency: 2", "max_concurrency: 7", 1))
	if err := os.Symlink("..v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join("..data", "config.yaml"), path); err != nil {
		t.Fatal(err)
	}

	l := NewLoader(path, quiet())
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = l.Watch(ctx) }()
	time.Sleep(100 * time.Millisecond)

	if err := os.Symlink("..v2", filepath.Join(dir, "..data_tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for l.State().Pools["a"].Stats().MaxConcurrency != 7 {
		if time.Now().After(deadline) {
			t.Fatal("reload not detected")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const twoPools = "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1}\n  b: {upstream_url: 'http://y', max_concurrency: 1}\n"

func ep(proto, name, body string) string {
	return "endpoints:\n  " + proto + ":\n    " + name + ": " + body + "\n"
}

func TestTargetsValidation(t *testing.T) {
	bad := map[string]string{
		"no targets":    ep("openai", "r", "{priority: 1}"),
		"empty targets": ep("openai", "r", "{targets: []}"),
		"no pool":       ep("openai", "r", "{targets: [{target_model: x}]}"),
		"unknown pool":  ep("openai", "r", "{targets: [{pool: a}, {pool: zzz}]}"),
		"duplicate":     ep("openai", "r", "{targets: [{pool: a, target_model: x}, {pool: b}, {pool: a, target_model: x}]}"),
		"bad field":     ep("openai", "r", "{targets: [{pool: a, poolz: b}]}"),
		"old pools key": ep("openai", "r", "{pools: [a]}"),
		"plural pools":  ep("openai", "r", "{targets: [{pools: [a]}]}"),
		"bad mode":      ep("openai", "r", "{mode: random, targets: [{pool: a}]}"),
		"empty name":    ep("openai", "''", "{targets: [{pool: a}]}"),
	}
	for name, r := range bad {
		if _, err := Parse([]byte(twoPools + r)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c, err := Parse([]byte(twoPools + `endpoints:
  openai:
    s: {targets: [{pool: a}]}
    m:
      mode: failover
      priority: 7
      targets:
        - {pool: b, target_model: 'claude-sonnet-5-5@medium'}
        - {pool: a, target_model: 'other', priority: 99}
        - {pool: a, target_model: 'third', priority: 0}
`))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := c.Endpoint("openai", "s")
	if s.Mode != ModeLeastInflight || s.Priority != 0 || s.QueuePriority(0) != 0 {
		t.Fatalf("defaults: %+v", s)
	}
	m, _ := c.Endpoint("openai", "m")
	if m.Mode != ModeFailover || len(m.Targets) != 3 || m.Targets[0].Pool != "b" || m.Targets[0].TargetModel != "claude-sonnet-5-5@medium" {
		t.Fatalf("order or target_model lost: %+v", m)
	}
	if m.QueuePriority(0) != 7 || m.QueuePriority(1) != 99 || m.QueuePriority(2) != 0 {
		t.Fatalf("priorities %d %d %d", m.QueuePriority(0), m.QueuePriority(1), m.QueuePriority(2))
	}
	if got := c.EndpointNames("openai"); len(got) != 2 || got[0] != "m" || got[1] != "s" {
		t.Fatalf("names %v", got)
	}
	if _, ok := c.Endpoint("anthropic", "s"); ok {
		t.Fatal("anthropic endpoint should not exist")
	}
}

func TestLegacyRoutesRejected(t *testing.T) {
	for _, r := range []string{"routes:\n  r: {pool: a}\n", "routes: {}\n"} {
		_, err := Parse([]byte(twoPools + r))
		if err == nil || !strings.Contains(err.Error(), "`routes:` key was removed") || !strings.Contains(err.Error(), "targets:") {
			t.Errorf("%q: want pointer to endpoints, got %v", r, err)
		}
	}
}

func TestReloadEndpointTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write(t, path, twoPools+ep("openai", "r", "{targets: [{pool: a}]}"))
	l := NewLoader(path, quiet())
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	first := l.State()
	write(t, path, twoPools+ep("openai", "r", "{targets: [{pool: a}, {pool: b}]}"))
	if err := l.Reload(); err != nil {
		t.Fatal(err)
	}
	second := l.State()
	if e, _ := second.Config.Endpoint("openai", "r"); len(e.Targets) != 2 || second.Pools["a"] != first.Pools["a"] {
		t.Fatal("targets not reloaded or pool instance not preserved")
	}
	write(t, path, twoPools+ep("openai", "r", "{targets: [{pool: a}, {pool: nope}]}"))
	if err := l.Reload(); err == nil || l.State() != second {
		t.Fatal("invalid endpoint must be rejected and previous state kept")
	}
}

const protoPools = `pools:
  ca: {upstream_url: 'http://x', protocol: anthropic, max_concurrency: 1}
  cb: {upstream_url: 'http://x2', protocol: anthropic, max_concurrency: 1}
  oa: {upstream_url: 'http://y', max_concurrency: 1}
`

func TestEndpointsProtocolValidation(t *testing.T) {
	bad := map[string]string{
		"unknown proto":  ep("gemini", "r", "{targets: [{pool: oa}]}"),
		"proto mismatch": ep("anthropic", "r", "{targets: [{pool: oa}]}"),
		"mismatch 2":     ep("openai", "r", "{targets: [{pool: oa}, {pool: ca}]}"),
		"bad pool proto": "pools:\n  p: {upstream_url: 'http://x', protocol: gemini, max_concurrency: 1}\n",
	}
	for name, r := range bad {
		in := protoPools + r
		if name == "bad pool proto" {
			in = r
		}
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	c, err := Parse([]byte(protoPools + `endpoints:
  anthropic:
    fast: {mode: round-robin, priority: 10, targets: [{pool: cb, target_model: "haiku@low"}, {pool: ca, target_model: "haiku@low"}]}
    aonly: {targets: [{pool: ca}]}
  openai:
    fast: {priority: 5, targets: [{pool: oa, target_model: "luna@low"}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.Endpoint("anthropic", "fast")
	if a.Mode != ModeRoundRobin || len(a.Targets) != 2 || a.Targets[0].Pool != "cb" || a.Priority != 10 {
		t.Fatalf("anthropic endpoint: %+v", a)
	}
	if o, _ := c.Endpoint("openai", "fast"); len(o.Targets) != 1 || o.Priority != 5 {
		t.Fatalf("openai endpoint: %+v", o)
	}
	if c.Pools["oa"].Protocol != "openai" {
		t.Fatal("default protocol not openai")
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], `"aonly" exists under anthropic but not openai`) {
		t.Fatalf("warnings %v", w)
	}
}

func TestNoWarningsForSingleProtocol(t *testing.T) {
	c, err := Parse([]byte(protoPools + ep("anthropic", "fast", "{targets: [{pool: ca}]}")))
	if err != nil {
		t.Fatal(err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Fatalf("unexpected warnings %v", w)
	}
}

// TestTargetUserConfig is the documented subscription-only setup, as the Helm
// chart renders it.
func TestTargetUserConfig(t *testing.T) {
	c, err := Parse([]byte(`server: {listen: ":8080", queue_timeout: "120s", aging_interval: "5s"}
pools:
  claude-subscription: {upstream_url: "http://localhost:9100", max_concurrency: 4, protocol: anthropic}
endpoints:
  anthropic:
    fast: {targets: [{pool: claude-subscription, target_model: "claude-haiku-5-5@low"}]}
    standard-low: {targets: [{pool: claude-subscription, target_model: "claude-sonnet-5-5@low"}]}
    standard-medium: {targets: [{pool: claude-subscription, target_model: "claude-sonnet-5-5@medium"}]}
    standard-high: {targets: [{pool: claude-subscription, target_model: "claude-sonnet-5-5@high"}]}
    premium-high: {targets: [{pool: claude-subscription, target_model: "claude-opus-5-5@high"}]}
    frontier-high: {targets: [{pool: claude-subscription, target_model: "claude-fable-5-2@high"}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(c.EndpointNames("anthropic")); n != 6 || len(c.Warnings()) != 0 {
		t.Fatalf("endpoints %d warnings %v", n, c.Warnings())
	}
}

func TestInvalidURLNotEchoed(t *testing.T) {
	_, err := Parse([]byte("pools:\n  p: {upstream_url: 'ftp://user:hunter2@x', max_concurrency: 1}\n"))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("want error without secret, got %v", err)
	}
}

func TestUnlimitedConcurrencyDefault(t *testing.T) {
	for name, y := range map[string]string{
		"omitted":  "pools:\n  a: {upstream_url: 'http://x'}\n",
		"explicit": "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 0}\n",
	} {
		c, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Pools["a"].MaxConcurrency != 0 {
			t.Fatalf("%s: want 0 (unlimited)", name)
		}
	}
}

func TestClientsConfig(t *testing.T) {
	base := "server: {client_keys_dir: /keys}\npools:\n  p: {upstream_url: 'http://x', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: p}]}\n"
	c, err := Parse([]byte(base + "clients:\n  a:\n  b: {}\n  c: {priority: 10, endpoints: [r]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Clients) != 3 || c.Clients["c"].Priority == nil || *c.Clients["c"].Priority != 10 || !c.Clients["c"].Allows("r") || c.Clients["a"].Allows("zz") != true {
		t.Fatalf("%+v", c.Clients)
	}
	for name, y := range map[string]string{
		"no dir":           "pools:\n  p: {upstream_url: 'http://x', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: p}]}\nclients:\n  a:\n",
		"unknown endpoint": base + "clients:\n  a: {endpoints: [nope]}\n",
		"bad name":         base + "clients:\n  '../x':\n",
		"unknown field":    base + "clients:\n  a: {bogus: 1}\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
