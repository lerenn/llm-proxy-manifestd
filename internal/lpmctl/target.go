// Package lpmctl implements the lpmctl command: a user-side Kubernetes CLI
// that inspects and operates an llm-proxy-manifestd Helm release (client keys,
// provider login status, interactive provider login) through the user's
// kubeconfig. It never runs inside the cluster.
package lpmctl

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// ChartName is the chart name, also the app.kubernetes.io/name label.
	ChartName = "llm-proxy-manifestd"
	// ProxyContainer is the name of the proxy container in the pod.
	ProxyContainer = ChartName

	labelName     = "app.kubernetes.io/name"
	labelInstance = "app.kubernetes.io/instance"

	configVolume     = "config"
	clientKeysVolume = "client-keys"
	configKey        = "config.yaml"
	adapterPrefix    = "adapter-"
)

// Target is a resolved release in a namespace.
type Target struct {
	Client    kubernetes.Interface
	Namespace string
	Release   string
}

// Resolve returns the target for release in namespace. An empty release is
// auto-detected from the pods labelled app.kubernetes.io/name=llm-proxy-manifestd:
// it succeeds only if exactly one release (app.kubernetes.io/instance) exists.
func Resolve(ctx context.Context, c kubernetes.Interface, namespace, release string) (*Target, error) {
	t := &Target{Client: c, Namespace: namespace, Release: release}
	if release != "" {
		return t, nil
	}
	pods, err := c.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelName + "=" + ChartName})
	if err != nil {
		return nil, fmt.Errorf("list pods in namespace %q: %w", namespace, err)
	}
	seen := map[string]bool{}
	for _, p := range pods.Items {
		if r := p.Labels[labelInstance]; r != "" {
			seen[r] = true
		}
	}
	releases := make([]string, 0, len(seen))
	for r := range seen {
		releases = append(releases, r)
	}
	sort.Strings(releases)
	switch len(releases) {
	case 0:
		return nil, fmt.Errorf("no %s release found in namespace %q (no pod labelled %s=%s); use -n/--namespace or --release", ChartName, namespace, labelName, ChartName)
	case 1:
		t.Release = releases[0]
		return t, nil
	default:
		return nil, fmt.Errorf("several %s releases in namespace %q (%s): pick one with --release", ChartName, namespace, strings.Join(releases, ", "))
	}
}

// Fullname mirrors the chart's llm-proxy-manifestd.fullname helper (without
// nameOverride/fullnameOverride): the release name if it already contains the
// chart name, otherwise "<release>-llm-proxy-manifestd".
func (t *Target) Fullname() string {
	n := t.Release
	if !strings.Contains(n, ChartName) {
		n = n + "-" + ChartName
	}
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// Pod returns the release's pod: a running, non-terminating pod if any (the
// newest one), else the newest pod.
func (t *Target) Pod(ctx context.Context) (*corev1.Pod, error) {
	sel := fmt.Sprintf("%s=%s,%s=%s", labelName, ChartName, labelInstance, t.Release)
	pods, err := t.Client.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list pods of release %q: %w", t.Release, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pod found for release %q in namespace %q", t.Release, t.Namespace)
	}
	items := pods.Items
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := usable(&items[i]), usable(&items[j])
		if ri != rj {
			return ri
		}
		return items[j].CreationTimestamp.Before(&items[i].CreationTimestamp)
	})
	return &items[0], nil
}

func usable(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil
}

// podVolumeSource returns the volume called name in the pod, if any.
func podVolume(p *corev1.Pod, name string) *corev1.Volume {
	if p == nil {
		return nil
	}
	for i := range p.Spec.Volumes {
		if p.Spec.Volumes[i].Name == name {
			return &p.Spec.Volumes[i]
		}
	}
	return nil
}

// ClientKeysSecretName returns the Secret holding the client keys: the one
// mounted by the pod (which honours clientsExistingSecret), else the chart
// default "<fullname>-client-keys".
func (t *Target) ClientKeysSecretName(pod *corev1.Pod) string {
	if v := podVolume(pod, clientKeysVolume); v != nil && v.Secret != nil && v.Secret.SecretName != "" {
		return v.Secret.SecretName
	}
	return t.Fullname() + "-client-keys"
}

// ConfigMapName returns the ConfigMap holding config.yaml: the one mounted by
// the pod, else the chart default "<fullname>".
func (t *Target) ConfigMapName(pod *corev1.Pod) string {
	if v := podVolume(pod, configVolume); v != nil && v.ConfigMap != nil && v.ConfigMap.Name != "" {
		return v.ConfigMap.Name
	}
	return t.Fullname()
}

// Provider is a provider found in the pod spec.
type Provider struct {
	Name string
	// Type is the provider type: claude, codex (sidecars) or anthropic-api,
	// openai-api (API keys, no container).
	Type string
	// Sidecar is true for CLI adapter sidecars.
	Sidecar bool
	// ReadyCommand is the sidecar's readiness command (e.g.
	// /usr/local/bin/adapter-claude ready).
	ReadyCommand []string
	// Pod is the pod running the adapter when it is NOT the proxy pod
	// (adapters.mode=separate). Empty = the proxy pod (sidecar mode).
	Pod string
	// SecretName/SecretKey locate an API provider's key (never read).
	SecretName, SecretKey string
}

// SidecarProviders returns the CLI adapter sidecars of the pod: containers
// carrying an ADAPTER_TYPE env var (claude, codex). They have no readiness
// probe (they must not gate pod readiness); the login state is checked by
// exec'ing /usr/local/bin/adapter-<type> ready. Sorted by name.
func SidecarProviders(pod *corev1.Pod) []Provider {
	var out []Provider
	for _, c := range pod.Spec.Containers {
		typ := ""
		for _, e := range c.Env {
			if e.Name == "ADAPTER_TYPE" {
				typ = e.Value
			}
		}
		if typ == "" {
			continue
		}
		out = append(out, Provider{
			Name:         c.Name,
			Type:         typ,
			Sidecar:      true,
			ReadyCommand: []string{"/usr/local/bin/" + adapterPrefix + typ, "ready"},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

const (
	labelComponent = "app.kubernetes.io/component"
	labelProvider  = "llm-proxy-manifestd/provider"
	componentAdapt = "adapter"
)

// Adapters returns every CLI adapter of the release: the sidecars of the
// proxy pod (adapters.mode=sidecar) and the adapter pods of
// adapters.mode=separate (label app.kubernetes.io/component=adapter, container
// carrying ADAPTER_TYPE), the newest usable pod per provider. Sorted by name.
func (t *Target) Adapters(ctx context.Context, proxy *corev1.Pod) ([]Provider, error) {
	var out []Provider
	for _, p := range SidecarProviders(proxy) {
		p.Pod = proxy.Name
		out = append(out, p)
	}
	sel := fmt.Sprintf("%s=%s,%s=%s", labelComponent, componentAdapt, labelInstance, t.Release)
	pods, err := t.Client.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("list adapter pods of release %q: %w", t.Release, err)
	}
	items := pods.Items
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := usable(&items[i]), usable(&items[j])
		if ri != rj {
			return ri
		}
		return items[j].CreationTimestamp.Before(&items[i].CreationTimestamp)
	})
	seen := map[string]bool{}
	for i := range items {
		pod := &items[i]
		key := pod.Labels[labelProvider]
		for _, p := range SidecarProviders(pod) {
			k := key
			if k == "" {
				k = p.Name
			}
			if seen[k] {
				continue
			}
			seen[k] = true
			p.Pod = pod.Name
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// APIProviders returns the API-key providers, found from the proxy
// container's PROVIDER_<NAME>_API_KEY env entries. Their type comes from
// the matching pool's protocol in cfg (anthropic -> anthropic-api, else
// openai-api). Sorted by name.
func APIProviders(pod *corev1.Pod, cfg *Manifest) []Provider {
	var out []Provider
	for _, c := range pod.Spec.Containers {
		if c.Name != ProxyContainer {
			continue
		}
		for _, e := range c.Env {
			if !strings.HasPrefix(e.Name, "PROVIDER_") || !strings.HasSuffix(e.Name, "_API_KEY") {
				continue
			}
			raw := strings.TrimSuffix(strings.TrimPrefix(e.Name, "PROVIDER_"), "_API_KEY")
			if raw == "" {
				continue
			}
			p := Provider{Name: strings.ReplaceAll(strings.ToLower(raw), "_", "-"), Type: "openai-api"}
			if cfg != nil {
				if pool, ok := cfg.Pools[p.Name]; ok && pool.Protocol == "anthropic" {
					p.Type = "anthropic-api"
				}
			}
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				p.SecretName = e.ValueFrom.SecretKeyRef.Name
				p.SecretKey = e.ValueFrom.SecretKeyRef.Key
			}
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
