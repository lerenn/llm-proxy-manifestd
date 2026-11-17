package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func check(t *testing.T, yml string) (int, string, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := run([]string{"-f", p}, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckerValidUnsetEnv(t *testing.T) {
	os.Unsetenv("NOPE_KEY")
	code, out, _ := check(t, "pools:\n  p: {upstream_url: 'http://x', api_key: '${NOPE_KEY}', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: p}]}\n")
	if code != 0 || !strings.Contains(out, "1 pools, 0 anthropic endpoints, 1 openai endpoints") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestCheckerInvalidNoSecret(t *testing.T) {
	t.Setenv("K", "topsecret")
	code, out, errs := check(t, "pools:\n  p: {upstream_url: 'http://u:topsecret@', api_key: '${K}', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: zz}]}\n")
	if code != 1 || strings.Contains(out+errs, "topsecret") || errs == "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestCheckerEndpointErrors(t *testing.T) {
	code, _, errs := check(t, "pools:\n  p: {upstream_url: 'http://x', max_concurrency: 1}\nendpoints:\n  anthropic:\n    r: {targets: [{pool: p}]}\n")
	if code != 1 || !strings.Contains(errs, "protocol") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestCheckerLegacyRoutesRejected(t *testing.T) {
	code, _, errs := check(t, "pools:\n  p: {upstream_url: 'http://x', max_concurrency: 1}\nroutes:\n  r: {pool: p}\n")
	if code != 1 || !strings.Contains(errs, "routes") || !strings.Contains(errs, "endpoints:") {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestCheckerWarnsOnOneSidedName(t *testing.T) {
	y := "pools:\n  a: {upstream_url: 'http://x', protocol: anthropic, max_concurrency: 1}\n  o: {upstream_url: 'http://y', max_concurrency: 1}\n" +
		"endpoints:\n  anthropic:\n    both: {targets: [{pool: a}]}\n    aonly: {targets: [{pool: a}]}\n  openai:\n    both: {targets: [{pool: o}]}\n"
	code, out, errs := check(t, y)
	if code != 0 || !strings.Contains(out, "config ok") || !strings.Contains(errs, `warning: endpoint "aonly" exists under anthropic but not openai`) ||
		strings.Contains(errs, `"both"`) {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestCheckerUsageAndMissingFile(t *testing.T) {
	var o, e bytes.Buffer
	if run(nil, &o, &e) != 2 {
		t.Fatal("want usage exit 2")
	}
	if run([]string{"-f", "/nonexistent.yaml"}, &o, &e) != 1 {
		t.Fatal("want exit 1 for missing file")
	}
}

func TestCheckerUnlimitedAndOmitted(t *testing.T) {
	for _, y := range []string{"max_concurrency: 0", ""} {
		if code, _, _ := check(t, "pools:\n  p: {upstream_url: 'http://x', "+y+"}\n"); code != 0 {
			t.Fatalf("%q must be valid (unlimited), code=%d", y, code)
		}
	}
	if code, _, _ := check(t, "pools:\n  p: {upstream_url: 'http://x', max_concurrency: -1}\n"); code == 0 {
		t.Fatal("negative must be invalid")
	}
}

func TestCheckerWarnsMissingClientKey(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "has"), []byte("sekret-key"), 0o600)
	code, out, errs := check(t, "server: {client_keys_dir: '"+dir+"'}\npools:\n  p: {upstream_url: 'http://x', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: p}]}\nclients:\n  has:\n  missing:\n")
	if code != 0 || !strings.Contains(errs, `client "missing" has no key`) || strings.Contains(errs, `"has"`) || strings.Contains(errs+out, "sekret-key") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

func TestCheckerConcurrencyGroup(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/c.yaml"
	y := "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1, concurrency_group: g}\n  b: {upstream_url: 'http://x', max_concurrency: 1, concurrency_group: g}\nendpoints:\n  openai:\n    m: {targets: [{pool: a}]}\n"
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-f", p}, &out, &errb); code != 0 || !strings.Contains(out.String(), "1 concurrency groups") {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
	}
	y = strings.Replace(y, "max_concurrency: 1, concurrency_group: g}\nendpoints", "max_concurrency: 2, concurrency_group: g}\nendpoints", 1)
	_ = os.WriteFile(p, []byte(y), 0o644)
	if code := run([]string{"-f", p}, &out, &errb); code != 1 {
		t.Fatalf("mismatch should fail, code=%d", code)
	}
}

func TestCheckerWarnsLastTargetSkip(t *testing.T) {
	y := "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1}\n  b: {upstream_url: 'http://y', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: a, when_concurrency_full: skip}, {pool: b, when_concurrency_full: skip}]}\n"
	code, _, errs := check(t, y)
	if code != 0 || !strings.Contains(errs, "last target has when_concurrency_full: skip") || strings.Count(errs, "warning") != 1 {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	code, _, errs = check(t, strings.Replace(y, "{pool: b, when_concurrency_full: skip}", "{pool: b}", 1))
	if code != 0 || errs != "" {
		t.Fatalf("code=%d err=%q", code, errs)
	}
}

func TestCheckerRejectsUnknownWhenConcurrencyFull(t *testing.T) {
	code, _, errs := check(t, "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1}\nendpoints:\n  openai:\n    r: {targets: [{pool: a, when_concurrency_full: nope}]}\n")
	if code != 1 || !strings.Contains(errs, `unknown when_concurrency_full "nope"`) {
		t.Fatalf("code=%d err=%q", code, errs)
	}
	code, _, errs = check(t, "pools:\n  a: {upstream_url: 'http://x', max_concurrency: 1, when_concurrency_full: skip}\nendpoints:\n  openai:\n    r: {targets: [{pool: a}]}\n")
	if code != 1 || !strings.Contains(errs, "when_concurrency_full") {
		t.Fatalf("pool-level when_concurrency_full must be rejected: code=%d err=%q", code, errs)
	}
}
