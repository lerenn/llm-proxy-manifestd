package lpmctl

import (
	"context"
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Manifest is the subset of the proxy's config.yaml that lpmctl reads. It is
// decoded leniently (unknown keys ignored) so lpmctl keeps working across
// proxy versions.
type Manifest struct {
	Pools   map[string]ManifestPool    `yaml:"pools"`
	Clients map[string]*ManifestClient `yaml:"clients"`
}

// ManifestPool is a pool entry.
type ManifestPool struct {
	Protocol         string `yaml:"protocol"`
	APIKey           string `yaml:"api_key"`
	MaxConcurrency   int    `yaml:"max_concurrency"`
	ConcurrencyGroup string `yaml:"concurrency_group"`
}

// ManifestClient is a client entry (may be empty).
type ManifestClient struct {
	Priority  *int     `yaml:"priority"`
	Endpoints []string `yaml:"endpoints"`
}

// ClientNames returns the configured client names, sorted.
func (m *Manifest) ClientNames() []string {
	out := make([]string, 0, len(m.Clients))
	for n := range m.Clients {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ParseManifest decodes config.yaml content.
func ParseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse config.yaml: %w", err)
	}
	return &m, nil
}

// LoadManifest reads and decodes the release's ConfigMap.
func (t *Target) LoadManifest(ctx context.Context, name string) (*Manifest, error) {
	cm, err := t.Client.CoreV1().ConfigMaps(t.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get configmap %q: %w", name, err)
	}
	raw, ok := cm.Data[configKey]
	if !ok {
		return nil, fmt.Errorf("configmap %q has no %s key", name, configKey)
	}
	return ParseManifest([]byte(raw))
}

// APIGroup is an API-key provider as rendered by the chart: one pool per
// format, all sharing a concurrency group named after the provider when there
// are several. A single-format provider is one pool named after it.
type APIGroup struct {
	Name           string
	Protocols      []string // sorted
	MaxConcurrency int      // 0 = unlimited
}

// APIGroups returns the API-key providers found in the pools of cfg: pools
// with an api_key (sidecar pools have none), grouped by concurrency_group
// (pools without one stand alone). Sorted by name.
func (m *Manifest) APIGroups() []APIGroup {
	if m == nil {
		return nil
	}
	by := map[string]*APIGroup{}
	for pn, p := range m.Pools {
		if p.APIKey == "" {
			continue
		}
		name := p.ConcurrencyGroup
		if name == "" {
			name = pn
		}
		g := by[name]
		if g == nil {
			g = &APIGroup{Name: name, MaxConcurrency: p.MaxConcurrency}
			by[name] = g
		}
		proto := p.Protocol
		if proto == "" {
			proto = "openai"
		}
		g.Protocols = append(g.Protocols, proto)
	}
	out := make([]APIGroup, 0, len(by))
	for _, g := range by {
		sort.Strings(g.Protocols)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
