package lpmctl

import (
	"context"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// maskedKey replaces a key in `lpmctl keys --hide`.
const maskedKey = "********"

// ClientKeys returns the client keys (client name -> key) from the release's
// client-keys Secret. Keys are only returned to the caller, never logged.
func (t *Target) ClientKeys(ctx context.Context) (string, map[string]string, error) {
	// The pod tells which Secret is really mounted (clientsExistingSecret);
	// without a pod, fall back to the chart's default name.
	pod, _ := t.Pod(ctx)
	name := t.ClientKeysSecretName(pod)
	s, err := t.Client.CoreV1().Secrets(t.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return name, nil, fmt.Errorf("secret %q not found in namespace %q: client keys are enabled by setting config.clients in the Helm values", name, t.Namespace)
	}
	if err != nil {
		return name, nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	keys := make(map[string]string, len(s.Data)+len(s.StringData))
	for k, v := range s.Data {
		keys[k] = string(v)
	}
	return name, keys, nil
}

// Keys implements `lpmctl keys [name]`. With a name it writes only that key
// and a newline (pipe-friendly). Without, a NAME/KEY table, masked if hide.
func Keys(ctx context.Context, t *Target, w io.Writer, name string, hide bool) error {
	secret, keys, err := t.ClientKeys(ctx)
	if err != nil {
		return err
	}
	if name != "" {
		k, ok := keys[name]
		if !ok {
			return fmt.Errorf("no client %q in secret %q", name, secret)
		}
		_, err := fmt.Fprintln(w, k)
		return err
	}
	names := make([]string, 0, len(keys))
	for n := range keys {
		names = append(names, n)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKEY")
	for _, n := range names {
		k := keys[n]
		if hide {
			k = maskedKey
		}
		fmt.Fprintf(tw, "%s\t%s\n", n, k)
	}
	return tw.Flush()
}
