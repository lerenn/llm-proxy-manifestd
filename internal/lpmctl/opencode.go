package lpmctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Default opencode provider ids and display names.
const (
	DefaultAnthropicProvider = "anyshiftlab-anthropic-proxy"
	DefaultOpenAIProvider    = "anyshiftlab-openai-proxy"

	anthropicVersion = "2023-06-01"
)

// OpenCodeOptions configures OpenCode.
type OpenCodeOptions struct {
	URL               string // proxy base URL, including /v1
	Key               string // client API key; never printed
	ConfigPath        string
	AnthropicProvider string
	OpenAIProvider    string
	Write             bool // false = dry run
	SetKey            bool // also write the apiKey option
	NoAnthropic       bool
	NoOpenAI          bool
	HTTP              *http.Client
	Stdout, Stderr    io.Writer
}

// DefaultOpenCodeConfigPath returns the config file opencode reads:
// $OPENCODE_CONFIG, else $XDG_CONFIG_HOME/opencode/opencode.json, else
// ~/.config/opencode/opencode.json (opencode.jsonc if only that one exists).
func DefaultOpenCodeConfigPath() (string, error) {
	if p := os.Getenv("OPENCODE_CONFIG"); p != "" {
		return p, nil
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(h, ".config")
	}
	p := filepath.Join(base, "opencode", "opencode.json")
	if _, err := os.Stat(p); err != nil {
		if _, err2 := os.Stat(p + "c"); err2 == nil {
			return p + "c", nil
		}
	}
	return p, nil
}

// ClientKey returns the key of one client from the cluster Secret. An empty
// name is accepted only when the Secret holds exactly one client.
func (t *Target) ClientKey(ctx context.Context, name string) (string, error) {
	secret, keys, err := t.ClientKeys(ctx)
	if err != nil {
		return "", err
	}
	if name == "" {
		if len(keys) != 1 {
			names := make([]string, 0, len(keys))
			for n := range keys {
				names = append(names, n)
			}
			sort.Strings(names)
			return "", fmt.Errorf("secret %q holds %d clients (%s): pick one with --client", secret, len(keys), strings.Join(names, ", "))
		}
		for _, k := range keys {
			return k, nil
		}
	}
	k, ok := keys[name]
	if !ok {
		return "", fmt.Errorf("no client %q in secret %q", name, secret)
	}
	return k, nil
}

// FetchModels lists the endpoint names the proxy exposes for one protocol.
func FetchModels(ctx context.Context, hc *http.Client, baseURL, key string, anthropic bool) ([]string, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("invalid --url: %w", err)
	}
	proto := "openai"
	if anthropic {
		proto = "anthropic"
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", anthropicVersion)
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error carries only the URL, never headers.
		return nil, fmt.Errorf("GET /models (%s format): %w", proto, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("GET /models (%s format): HTTP %d: the proxy rejected the client key (check --client / --key-env)", proto, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /models (%s format): HTTP %d", proto, resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("GET /models (%s format): not a model list: %w", proto, err)
	}
	var ids []string
	for _, m := range list.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// ordered JSON object: keeps key order and untouched values verbatim.
type obj struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObj() *obj { return &obj{vals: map[string]json.RawMessage{}} }

func parseObj(raw []byte) (*obj, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	o := newObj()
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(kt.(string), v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	return o, nil
}

func (o *obj) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *obj) sub(k string) (*obj, error) {
	raw, ok := o.vals[k]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return newObj(), nil
	}
	s, err := parseObj(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not an object", k)
	}
	return s, nil
}

func (o *obj) raw() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(o.vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}

func (o *obj) setObj(k string, s *obj) { o.set(k, s.raw()) }

func jstr(s string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSpace(b.Bytes())
}

// style describes the config key names of one opencode config generation.
type style struct {
	providers, pkg, settings string
	v2                       bool
}

var (
	styleV1 = style{providers: "provider", pkg: "npm", settings: "options"}
	styleV2 = style{providers: "providers", pkg: "package", settings: "settings", v2: true}
)

func detectStyle(root *obj) style {
	if _, ok := root.vals["provider"]; ok {
		return styleV1
	}
	if _, ok := root.vals["providers"]; ok {
		return styleV2
	}
	return styleV1
}

// providerSpec is one provider to sync.
type providerSpec struct {
	id, name, npm string
	ids           []string
}

// ProviderChange is the diff of one provider's model list.
type ProviderChange struct {
	Provider             string
	Added, Removed, Same []string
	Created              bool
}

// mergeConfig returns the new file content and the per-provider changes.
func mergeConfig(existing []byte, st *style, specs []providerSpec, baseURL, key string, setKey bool) ([]byte, []ProviderChange, error) {
	root := newObj()
	if len(bytes.TrimSpace(existing)) > 0 {
		var err error
		root, err = parseObj(existing)
		if err != nil {
			return nil, nil, err
		}
	}
	s := detectStyle(root)
	if st != nil {
		*st = s
	}
	if _, ok := root.vals["$schema"]; !ok && len(root.keys) == 0 {
		root.set("$schema", jstr("https://opencode.ai/config.json"))
	}
	provs, err := root.sub(s.providers)
	if err != nil {
		return nil, nil, err
	}
	var changes []ProviderChange
	for _, sp := range specs {
		p, err := provs.sub(sp.id)
		if err != nil {
			return nil, nil, fmt.Errorf("provider %q: %w", sp.id, err)
		}
		ch := ProviderChange{Provider: sp.id}
		if _, ok := provs.vals[sp.id]; !ok {
			ch.Created = true
			pkg := sp.npm
			if s.v2 {
				pkg = "aisdk:" + pkg
			}
			p.set(s.pkg, jstr(pkg))
			p.set("name", jstr(sp.name))
		}
		opts, err := p.sub(s.settings)
		if err != nil {
			return nil, nil, fmt.Errorf("provider %q: %w", sp.id, err)
		}
		if ch.Created {
			opts.set("baseURL", jstr(baseURL))
		}
		if setKey {
			opts.set("apiKey", jstr(key))
		}
		if len(opts.keys) > 0 {
			p.setObj(s.settings, opts)
		}
		old, err := p.sub("models")
		if err != nil {
			return nil, nil, fmt.Errorf("provider %q: %w", sp.id, err)
		}
		nm := newObj()
		want := map[string]bool{}
		for _, id := range sp.ids {
			if want[id] {
				continue
			}
			want[id] = true
			if v, ok := old.vals[id]; ok { // keep a kept model's own settings
				nm.set(id, v)
				ch.Same = append(ch.Same, id)
			} else {
				nm.set(id, json.RawMessage(`{"name":`+string(jstr(id))+`}`))
				ch.Added = append(ch.Added, id)
			}
		}
		for _, id := range old.keys {
			if !want[id] {
				ch.Removed = append(ch.Removed, id)
			}
		}
		p.setObj("models", nm)
		provs.setObj(sp.id, p)
		changes = append(changes, ch)
	}
	root.setObj(s.providers, provs)
	var out bytes.Buffer
	if err := json.Indent(&out, root.raw(), "", "  "); err != nil {
		return nil, nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), changes, nil
}

// OpenCode implements `lpmctl opencode`: it generates the models maps of two
// opencode providers from the proxy's live /v1/models.
func OpenCode(ctx context.Context, o OpenCodeOptions) error {
	if o.URL == "" {
		return errors.New("no proxy URL: pass --url (base URL including /v1) or set LPMCTL_PROXY_URL")
	}
	if o.NoAnthropic && o.NoOpenAI {
		return errors.New("--no-anthropic and --no-openai together leave nothing to do")
	}
	var specs []providerSpec
	if !o.NoAnthropic {
		ids, err := FetchModels(ctx, o.HTTP, o.URL, o.Key, true)
		if err != nil {
			return err
		}
		specs = append(specs, providerSpec{o.AnthropicProvider, "Anyshift Lab Proxy (Anthropic)", "@ai-sdk/anthropic", ids})
	}
	if !o.NoOpenAI {
		ids, err := FetchModels(ctx, o.HTTP, o.URL, o.Key, false)
		if err != nil {
			return err
		}
		specs = append(specs, providerSpec{o.OpenAIProvider, "Anyshift Lab Proxy (OpenAI)", "@ai-sdk/openai-compatible", ids})
	}

	existing, err := os.ReadFile(o.ConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fileExists := err == nil
	// Snippet printed when the file cannot be edited safely (never has the key).
	snippet := func() string {
		var b strings.Builder
		for _, sp := range specs {
			m := newObj()
			for _, id := range sp.ids {
				m.set(id, json.RawMessage(`{"name":`+string(jstr(id))+`}`))
			}
			var ind bytes.Buffer
			_ = json.Indent(&ind, m.raw(), "", "  ")
			fmt.Fprintf(&b, "%q models:\n%s\n", sp.id, ind.String())
		}
		return b.String()
	}
	out, changes, err := mergeConfig(existing, nil, specs, o.URL, o.Key, o.SetKey)
	if err != nil {
		fmt.Fprintf(o.Stderr, "lpmctl: %s is not plain JSON that can be edited safely (comments, trailing commas or another problem: %v).\n"+
			"Nothing was written. Set these `models` maps by hand:\n\n%s", o.ConfigPath, err, snippet())
		return fmt.Errorf("cannot edit %s", o.ConfigPath)
	}

	for _, c := range changes {
		verb := "update"
		if c.Created {
			verb = "create"
		}
		if !o.Write {
			verb = "would " + verb
		}
		fmt.Fprintf(o.Stdout, "%s %s: +%d -%d =%d\n", verb, c.Provider, len(c.Added), len(c.Removed), len(c.Same))
		for _, id := range c.Added {
			fmt.Fprintf(o.Stdout, "  + %s/%s\n", c.Provider, id)
		}
		for _, id := range c.Removed {
			fmt.Fprintf(o.Stdout, "  - %s/%s\n", c.Provider, id)
		}
		for _, id := range c.Same {
			fmt.Fprintf(o.Stdout, "    %s/%s\n", c.Provider, id)
		}
	}
	if !o.Write {
		fmt.Fprintf(o.Stdout, "\nDry run: %s not modified. Re-run with --write to apply.\n", o.ConfigPath)
		return nil
	}
	if fileExists && bytes.Equal(existing, out) {
		fmt.Fprintf(o.Stdout, "\n%s already up to date.\n", o.ConfigPath)
		return nil
	}
	if err := writeAtomic(o.ConfigPath, out, existing, fileExists); err != nil {
		return err
	}
	fmt.Fprintf(o.Stdout, "\nWrote %s. Run `opencode service restart` (opencode caches its config in a background service), and restart any opencode session started by Paseo.\n", o.ConfigPath)
	return nil
}

// writeAtomic replaces path with data via a temp file + rename, keeping the
// existing mode (0600 for a new file) and saving <path>.bak first if absent.
func writeAtomic(path string, data, old []byte, exists bool) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	mode := os.FileMode(0o600)
	if exists {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		mode = fi.Mode().Perm()
		if _, err := os.Stat(path + ".bak"); os.IsNotExist(err) {
			if err := os.WriteFile(path+".bak", old, mode); err != nil {
				return fmt.Errorf("backup: %w", err)
			}
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".opencode-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
