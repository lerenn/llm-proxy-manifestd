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
