package lpmctl

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const ns = "llm"

func labels(release string) map[string]string {
	return map[string]string{labelName: ChartName, labelInstance: release}
}

func sidecar(name, typ string) corev1.Container {
	return corev1.Container{
		Name: name,
		Env:  []corev1.EnvVar{{Name: "ADAPTER_TYPE", Value: typ}},
	}
}

// releasePod is a pod as rendered by the chart for release "proxy": a claude
// and a codex sidecar, an anthropic-api provider, client keys from a custom
// Secret.
func releasePod(name, release string, phase corev1.PodPhase, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels(release), CreationTimestamp: metav1.NewTime(created)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: ProxyContainer, Env: []corev1.EnvVar{
					{Name: "PROVIDER_MY_ANTHROPIC_API_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "provider-keys"}, Key: "anthropic"}}},
					{Name: "OTHER", Value: "x"},
				}},
				sidecar("claude-sub", "claude"),
				sidecar("codex-sub", "codex"),
			},
			Volumes: []corev1.Volume{
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: release + "-llm-proxy-manifestd"}}}},
				{Name: "client-keys", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "my-client-keys"}}},
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

const manifest = `server: {listen: ":8080"}
pools:
  my-anthropic: {upstream_url: https://api.anthropic.com, protocol: anthropic, api_key: "${PROVIDER_MY_ANTHROPIC_API_KEY}"}
  claude-sub: {upstream_url: "http://localhost:9100", protocol: anthropic}
  nevermetered-anthropic: {upstream_url: https://nm.example, protocol: anthropic, api_key: "${K}", max_concurrency: 1, concurrency_group: nevermetered}
  nevermetered-openai: {upstream_url: https://nm.example, protocol: openai, api_key: "${K}", max_concurrency: 1, concurrency_group: nevermetered}
clients:
  opencode: {}
  ci: {priority: 10, endpoints: [fast, standard]}
  bare:
endpoints: {}
unknown_future_key: true
`

func objects() []runtime.Object {
	return []runtime.Object{
		releasePod("proxy-old", "proxy", corev1.PodRunning, time.Unix(100, 0)),
		releasePod("proxy-new", "proxy", corev1.PodRunning, time.Unix(200, 0)),
		releasePod("proxy-pending", "proxy", corev1.PodPending, time.Unix(300, 0)),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "proxy-llm-proxy-manifestd", Namespace: ns},
			Data: map[string]string{"config.yaml": manifest}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "my-client-keys", Namespace: ns},
			Data: map[string][]byte{"opencode": []byte("llmp_open"), "ci": []byte("llmp_ci")}},
	}
}

type fakeExec struct {
	calls  []ExecRequest
	result map[string]error // by container
}

func (f *fakeExec) Exec(_ context.Context, r ExecRequest) error {
	f.calls = append(f.calls, r)
	return f.result[r.Container]
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	c := fake.NewSimpleClientset(objects()...)
	tg, err := Resolve(ctx, c, ns, "")
	if err != nil || tg.Release != "proxy" {
		t.Fatalf("auto-detect: %v %+v", err, tg)
	}
	if _, err := Resolve(ctx, c, "empty", ""); err == nil || !strings.Contains(err.Error(), "no llm-proxy-manifestd release") {
		t.Fatalf("empty namespace: %v", err)
	}
	c = fake.NewSimpleClientset(append(objects(), releasePod("other-x", "other", corev1.PodRunning, time.Now()))...)
	if _, err := Resolve(ctx, c, ns, ""); err == nil || !strings.Contains(err.Error(), "other, proxy") {
		t.Fatalf("ambiguous: %v", err)
	}
	if tg, err := Resolve(ctx, c, ns, "other"); err != nil || tg.Release != "other" {
		t.Fatalf("explicit: %v", err)
	}
}

func TestFullname(t *testing.T) {
	for rel, want := range map[string]string{
		"proxy":                       "proxy-llm-proxy-manifestd",
		"llm-proxy-manifestd":         "llm-proxy-manifestd",
		"my-llm-proxy-manifestd-prod": "my-llm-proxy-manifestd-prod",
	} {
		if got := (&Target{Release: rel}).Fullname(); got != want {
			t.Errorf("%s: got %s want %s", rel, got, want)
		}
	}
}

func TestPodPrefersNewestRunning(t *testing.T) {
	tg := &Target{Client: fake.NewSimpleClientset(objects()...), Namespace: ns, Release: "proxy"}
	p, err := tg.Pod(context.Background())
	if err != nil || p.Name != "proxy-new" {
		t.Fatalf("got %v %v", p, err)
	}
}

func TestProviders(t *testing.T) {
	pod := releasePod("p", "proxy", corev1.PodRunning, time.Now())
	sc := SidecarProviders(pod)
	if len(sc) != 2 || sc[0].Name != "claude-sub" || sc[0].Type != "claude" || sc[1].Type != "codex" ||
		!reflect.DeepEqual(sc[1].ReadyCommand, []string{"/usr/local/bin/adapter-codex", "ready"}) {
		t.Fatalf("sidecars: %+v", sc)
	}
	m, _ := ParseManifest([]byte(manifest))
	api := APIProviders(pod, m)
	if len(api) != 1 || api[0].Name != "my-anthropic" || api[0].Type != "anthropic-api" || api[0].SecretName != "provider-keys" {
		t.Fatalf("api: %+v", api)
	}
	if api := APIProviders(pod, nil); api[0].Type != "openai-api" {
		t.Fatalf("api without config defaults to openai-api: %+v", api)
	}
}

func TestKeysTable(t *testing.T) {
	tg := &Target{Client: fake.NewSimpleClientset(objects()...), Namespace: ns, Release: "proxy"}
	var b bytes.Buffer
	if err := Keys(context.Background(), tg, &b, "", false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "NAME") || !strings.Contains(lines[1], "ci") ||
		!strings.Contains(lines[1], "llmp_ci") || !strings.Contains(lines[2], "llmp_open") {
		t.Fatalf("table:\n%s", b.String())
	}

	b.Reset()
	if err := Keys(context.Background(), tg, &b, "", true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "llmp_") || !strings.Contains(b.String(), maskedKey) {
		t.Fatalf("hide leaked keys:\n%s", b.String())
	}
}

func TestKeysSingleIsPipeFriendly(t *testing.T) {
	tg := &Target{Client: fake.NewSimpleClientset(objects()...), Namespace: ns, Release: "proxy"}
	var b bytes.Buffer
	if err := Keys(context.Background(), tg, &b, "opencode", false); err != nil {
		t.Fatal(err)
	}
	if b.String() != "llmp_open\n" {
		t.Fatalf("got %q", b.String())
	}
	b.Reset()
	err := Keys(context.Background(), tg, &b, "nope", false)
	if err == nil || b.Len() != 0 || strings.Contains(err.Error(), "llmp_") {
		t.Fatalf("unknown client: %v %q", err, b.String())
	}
}

func TestKeysDefaultSecretName(t *testing.T) {
	// No pod: fall back to <fullname>-client-keys; missing Secret is explained.
	c := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "proxy-llm-proxy-manifestd-client-keys", Namespace: ns},
		Data: map[string][]byte{"a": []byte("k")}})
	tg := &Target{Client: c, Namespace: ns, Release: "proxy"}
	var b bytes.Buffer
	if err := Keys(context.Background(), tg, &b, "a", false); err != nil || b.String() != "k\n" {
		t.Fatalf("%v %q", err, b.String())
	}
	tg.Release = "missing"
	if err := Keys(context.Background(), tg, &b, "", false); err == nil || !strings.Contains(err.Error(), "config.clients") {
		t.Fatalf("missing secret: %v", err)
	}
}

func TestStatus(t *testing.T) {
	tg := &Target{Client: fake.NewSimpleClientset(objects()...), Namespace: ns, Release: "proxy"}
	ex := &fakeExec{result: map[string]error{"codex-sub": &ExitError{Code: 1}}}
	var out, errw bytes.Buffer
	if err := Status(context.Background(), tg, ex, &out, &errw); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"pod proxy-new (Running)",
		"claude-sub    claude  logged-in",
		"codex-sub     codex   not logged-in",
		"my-anthropic  api     anthropic, max_concurrency unlimited (no login needed)",
		"nevermetered  api     anthropic,openai, max_concurrency 1 (no login needed)",
		"bare      -         *",
		"ci        10        fast,standard",
		"opencode  -         *",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "llmp_") || errw.Len() != 0 {
		t.Fatalf("unexpected output: %s / %s", s, errw.String())
	}
	if len(ex.calls) != 2 || ex.calls[0].Pod != "proxy-new" || ex.calls[0].Container != "claude-sub" ||
		!reflect.DeepEqual(ex.calls[0].Command, []string{"/usr/local/bin/adapter-claude", "ready"}) || ex.calls[0].TTY {
		t.Fatalf("exec calls: %+v", ex.calls)
	}
}

func TestStatusExecFailureAndMissingConfig(t *testing.T) {
	c := fake.NewSimpleClientset(releasePod("p", "proxy", corev1.PodRunning, time.Now()))
	tg := &Target{Client: c, Namespace: ns, Release: "proxy"}
	ex := &fakeExec{result: map[string]error{"claude-sub": errors.New("container not\nfound")}}
	var out, errw bytes.Buffer
	if err := Status(context.Background(), tg, ex, &out, &errw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unknown (container not found)") || !strings.Contains(out.String(), "Clients: unknown") ||
		!strings.Contains(errw.String(), "warning") {
		t.Fatalf("out:\n%s\nerr:\n%s", out.String(), errw.String())
	}
}

func TestLogin(t *testing.T) {
	ctx := context.Background()
	tg := &Target{Client: fake.NewSimpleClientset(objects()...), Namespace: ns, Release: "proxy"}

	for typ, want := range map[string][]string{
		"claude-sub": {"claude"},
		"codex-sub":  {"codex", "login", "--device-auth"},
	} {
		ex := &fakeExec{}
		var errw bytes.Buffer
		raw := false
		err := Login(ctx, tg, ex, typ, LoginOptions{TTY: true, Stderr: &errw,
			BeforeExec: func() (func(), error) { raw = true; return func() { raw = false }, nil }})
		if err != nil {
			t.Fatal(err)
		}
		r := ex.calls[0]
		if !reflect.DeepEqual(r.Command, want) || r.Container != typ || r.Pod != "proxy-new" || !r.TTY || raw {
			t.Fatalf("%s: %+v raw=%v", typ, r, raw)
		}
		if !strings.Contains(errw.String(), "Headless login") {
			t.Fatalf("%s: no hint: %s", typ, errw.String())
		}
	}

	ex := &fakeExec{}
	if err := Login(ctx, tg, ex, "claude-sub", LoginOptions{TTY: true, Command: []string{"claude", "setup-token"}}); err != nil ||
		!reflect.DeepEqual(ex.calls[0].Command, []string{"claude", "setup-token"}) {
		t.Fatalf("override: %v %+v", err, ex.calls)
	}

	if err := Login(ctx, tg, ex, "my-anthropic", LoginOptions{TTY: true}); err == nil || !strings.Contains(err.Error(), "no login needed") {
		t.Fatalf("api provider: %v", err)
	}
	if err := Login(ctx, tg, ex, "nevermetered", LoginOptions{TTY: true}); err == nil || !strings.Contains(err.Error(), "no login needed") {
		t.Fatalf("api group: %v", err)
	}
	if err := Login(ctx, tg, ex, "nope", LoginOptions{TTY: true}); err == nil || !strings.Contains(err.Error(), "claude-sub, codex-sub") {
		t.Fatalf("unknown: %v", err)
	}
	if err := Login(ctx, tg, ex, "claude-sub", LoginOptions{}); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("no tty: %v", err)
	}
}
