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
	Protocol string `yaml:"protocol"`
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
