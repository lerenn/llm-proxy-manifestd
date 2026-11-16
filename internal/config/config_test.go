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
routes:
  m:
    pool: a
    target_model: gpt-x
    priority: 10
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
		"unknown pool":  "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1}\nroutes:\n  r: {pool: b}\n",
		"zero conc":     "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 0}\n",
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
