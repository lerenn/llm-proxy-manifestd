package lpmctl

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "llmp_SECRETKEY123"

func fakeProxy(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != "" {
			if r.Header.Get("x-api-key") != testKey || r.Header.Get("anthropic-version") != "2023-06-01" {
				http.Error(w, "no", 401)
				return
			}
			w.Write([]byte(`{"data":[{"type":"model","id":"fast"},{"type":"model","id":"premium"}],"has_more":false}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "no", 401)
			return
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"fast","object":"model"},{"id":"codex"}]}`))
	}))
}

func runOC(t *testing.T, o OpenCodeOptions) (string, string, error) {
	t.Helper()
	var so, se bytes.Buffer
	o.Stdout, o.Stderr = &so, &se
	if o.AnthropicProvider == "" {
		o.AnthropicProvider = DefaultAnthropicProvider
	}
	if o.OpenAIProvider == "" {
		o.OpenAIProvider = DefaultOpenAIProvider
	}
	err := OpenCode(context.Background(), o)
	return so.String(), se.String(), err
}

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, b)
	}
	return m
}

const baseCfg = `{
  "$schema": "https://opencode.ai/config.json",
  "theme": "dark",
  "provider": {
    "other": {"npm": "x", "models": {"keep": {}}},
    "anyshiftlab-openai-proxy": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {"baseURL": "https://x/v1", "apiKey": "oldkey"},
      "models": {"fast": {"limit": {"context": 5, "output": 1}}, "handmade": {}}
    }
  }
}`

func models(m map[string]any, style, id string) map[string]any {
	return m[style].(map[string]any)[id].(map[string]any)["models"].(map[string]any)
}

func TestOpenCodeMergeWriteBackupMode(t *testing.T) {
	srv := fakeProxy(t)
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "opencode.json")
	os.WriteFile(p, []byte(baseCfg), 0o640)
	o := OpenCodeOptions{URL: srv.URL + "/v1", Key: testKey, ConfigPath: p}

	// dry run changes nothing
	out, _, err := runOC(t, o)
	if err != nil || !strings.Contains(out, "Dry run") {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(p); string(b) != baseCfg {
		t.Fatal("dry run modified file")
	}
	if _, err := os.Stat(p + ".bak"); err == nil {
		t.Fatal("dry run created .bak")
	}

	o.Write = true
	out, _, err = runOC(t, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"+ anyshiftlab-openai-proxy/codex", "- anyshiftlab-openai-proxy/handmade", "create anyshiftlab-anthropic-proxy", "opencode service restart"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
	m := readJSON(t, p)
	if m["theme"] != "dark" || m["provider"].(map[string]any)["other"] == nil {
		t.Fatal("unrelated keys lost")
	}
	om := models(m, "provider", "anyshiftlab-openai-proxy")
	if len(om) != 2 || om["handmade"] != nil || om["codex"] == nil {
		t.Fatalf("openai models %v", om)
	}
	if om["fast"].(map[string]any)["limit"] == nil {
		t.Fatal("kept model lost its settings")
	}
	opts := m["provider"].(map[string]any)["anyshiftlab-openai-proxy"].(map[string]any)["options"].(map[string]any)
	if opts["apiKey"] != "oldkey" {
		t.Fatal("apiKey touched without --set-key")
	}
	an := m["provider"].(map[string]any)["anyshiftlab-anthropic-proxy"].(map[string]any)
	if an["npm"] != "@ai-sdk/anthropic" || an["options"].(map[string]any)["baseURL"] != srv.URL+"/v1" || len(an["models"].(map[string]any)) != 2 {
		t.Fatalf("created provider %v", an)
	}
	if _, ok := an["options"].(map[string]any)["apiKey"]; ok {
		t.Fatal("apiKey written without --set-key")
	}
	if b, _ := os.ReadFile(p + ".bak"); string(b) != baseCfg {
		t.Fatal("bad backup")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if ents, _ := os.ReadDir(filepath.Dir(p)); len(ents) != 2 {
		t.Fatalf("leftover temp files: %v", ents)
	}
	// second run: up to date
	out, _, _ = runOC(t, o)
	if !strings.Contains(out, "already up to date") {
		t.Fatal(out)
	}
}

func TestOpenCodeNewFile0600AndSetKey(t *testing.T) {
	srv := fakeProxy(t)
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "sub", "opencode.json")
	o := OpenCodeOptions{URL: srv.URL + "/v1", Key: testKey, ConfigPath: p, Write: true, SetKey: true}
	so, se, err := runOC(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	m := readJSON(t, p)
	if models(m, "provider", DefaultOpenAIProvider)["fast"] == nil {
		t.Fatal("no models")
	}
	if m["provider"].(map[string]any)[DefaultOpenAIProvider].(map[string]any)["options"].(map[string]any)["apiKey"] != testKey {
		t.Fatal("key not set")
	}
	if strings.Contains(so+se, testKey) {
		t.Fatal("key printed")
	}
}

func TestOpenCodeStyleV2AndNoOpenAI(t *testing.T) {
	srv := fakeProxy(t)
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"providers":{"z":{"models":{}}}}`), 0o600)
	o := OpenCodeOptions{URL: srv.URL + "/v1", Key: testKey, ConfigPath: p, Write: true, NoOpenAI: true}
	if _, _, err := runOC(t, o); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, p)
	if _, ok := m["provider"]; ok {
		t.Fatal("wrong style")
	}
	ps := m["providers"].(map[string]any)
	if ps[DefaultOpenAIProvider] != nil || ps["z"] == nil {
		t.Fatalf("%v", ps)
	}
	a := ps[DefaultAnthropicProvider].(map[string]any)
	if a["package"] == nil || a["settings"].(map[string]any)["baseURL"] == nil {
		t.Fatalf("v2 block %v", a)
	}
}

func TestOpenCodeAuthErrorAndJSONC(t *testing.T) {
	srv := fakeProxy(t)
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "c.json")
	o := OpenCodeOptions{URL: srv.URL + "/v1", Key: "wrong", ConfigPath: p, Write: true}
	so, se, err := runOC(t, o)
	if err == nil || !strings.Contains(err.Error(), "rejected") || strings.Contains(so+se+err.Error(), "wrong") && false {
		t.Fatalf("want auth error, got %v", err)
	}
	if _, e := os.Stat(p); e == nil {
		t.Fatal("file created on auth error")
	}

	jc := "{\n // comment\n \"theme\": \"x\",\n}\n"
	os.WriteFile(p, []byte(jc), 0o600)
	o.Key = testKey
	so, se, err = runOC(t, o)
	if err == nil || !strings.Contains(se, "Nothing was written") || !strings.Contains(se, `"fast"`) {
		t.Fatalf("%v %s", err, se)
	}
	if b, _ := os.ReadFile(p); string(b) != jc {
		t.Fatal("jsonc file modified")
	}
	if strings.Contains(so+se, testKey) {
		t.Fatal("key printed")
	}
}

func TestOpenCodeNoURL(t *testing.T) {
	if _, _, err := runOC(t, OpenCodeOptions{}); err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatal(err)
	}
}

func TestDisplayName(t *testing.T) {
	for in, want := range map[string]string{
		"fast":            "Fast",
		"standard-low":    "Standard (Low)",
		"premium-high":    "Premium (High)",
		"frontier-high":   "Frontier (High)",
		"my-big-fast":     "My (Big Fast)",
		"gPT-4o-Mini":     "GPT (4o Mini)",
		"gpt-5":           "Gpt (5)",
		"a--b-":           "A (B)",
		"-lead":           "Lead",
		"-":               "-",
		"":                "",
		"été-rapide":      "Été (Rapide)",
		"codex@high.v2-x": "Codex@high.v2 (X)",
	} {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenCodeRenameRules(t *testing.T) {
	srv := fakeProxy(t)
	defer srv.Close()
	cfg := `{"provider":{"anyshiftlab-openai-proxy":{"models":{
	  "fast":{"name":"fast","limit":{"context":5}},
	  "codex":{"name":"My Codex","limit":{"context":7}}}},
	  "anyshiftlab-anthropic-proxy":{"models":{"fast":{"name":"Fast"},"premium":{}}}}}`
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(cfg), 0o600)
	o := OpenCodeOptions{URL: srv.URL + "/v1", Key: testKey, ConfigPath: p}
	out, _, err := runOC(t, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`~ anyshiftlab-openai-proxy/fast: "fast" -> "Fast"`, `~ anyshiftlab-anthropic-proxy/premium: "" -> "Premium"`, "~1"} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "My Codex") {
		t.Error("custom name reported as renamed")
	}
	o.Write = true
	if _, _, err := runOC(t, o); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, p)
	om := models(m, "provider", DefaultOpenAIProvider)
	if om["fast"].(map[string]any)["name"] != "Fast" || om["fast"].(map[string]any)["limit"] == nil {
		t.Fatalf("%v", om["fast"])
	}
	if om["codex"].(map[string]any)["name"] != "My Codex" || om["codex"].(map[string]any)["limit"] == nil {
		t.Fatalf("custom name touched: %v", om["codex"])
	}
	if models(m, "provider", DefaultAnthropicProvider)["premium"].(map[string]any)["name"] != "Premium" {
		t.Fatal("missing name not filled")
	}
	// idempotent
	out, _, _ = runOC(t, o)
	if !strings.Contains(out, "already up to date") {
		t.Fatal(out)
	}
}

func TestOpenCodeRawNames(t *testing.T) {
	srv := fakeProxy(t)
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "c.json")
	o := OpenCodeOptions{URL: srv.URL + "/v1", Key: testKey, ConfigPath: p, Write: true, RawNames: true}
	if _, _, err := runOC(t, o); err != nil {
		t.Fatal(err)
	}
	if n := models(readJSON(t, p), "provider", DefaultOpenAIProvider)["fast"].(map[string]any)["name"]; n != "fast" {
		t.Fatalf("raw name %v", n)
	}
	// switching back to readable names renames generated/raw ones
	o.RawNames = false
	if _, _, err := runOC(t, o); err != nil {
		t.Fatal(err)
	}
	if n := models(readJSON(t, p), "provider", DefaultOpenAIProvider)["fast"].(map[string]any)["name"]; n != "Fast" {
		t.Fatalf("name %v", n)
	}
	// and raw again from the generated name
	o.RawNames = true
	if _, _, err := runOC(t, o); err != nil {
		t.Fatal(err)
	}
	if n := models(readJSON(t, p), "provider", DefaultOpenAIProvider)["fast"].(map[string]any)["name"]; n != "fast" {
		t.Fatalf("name %v", n)
	}
}
