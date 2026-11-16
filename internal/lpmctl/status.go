package lpmctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// Login states printed by `lpmctl status`.
const (
	StateLoggedIn    = "logged-in"
	StateNotLoggedIn = "not logged-in"
	StateAPIKey      = "api-key"
)

// readyTimeout bounds each sidecar's ready check.
const readyTimeout = 15 * time.Second

// ProviderStatus is one row of `lpmctl status`.
type ProviderStatus struct {
	Provider
	State string
}

// SidecarState runs the sidecar's ready check (`adapter-<type> ready`) in its
// container: exit 0 means logged in, a non-zero exit not logged in.
func SidecarState(ctx context.Context, ex Executor, namespace, pod string, p Provider) string {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	err := ex.Exec(ctx, ExecRequest{
		Namespace: namespace, Pod: pod, Container: p.Name,
		Command: p.ReadyCommand,
		Stdout:  io.Discard, Stderr: io.Discard,
	})
	var ee *ExitError
	switch {
	case err == nil:
		return StateLoggedIn
	case errors.As(err, &ee):
		return StateNotLoggedIn
	default:
		return "unknown (" + oneLine(err.Error()) + ")"
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Status implements `lpmctl status`: the login state of every provider and
// the configured clients. Warnings (unreadable ConfigMap) go to errw.
func Status(ctx context.Context, t *Target, ex Executor, w, errw io.Writer) error {
	pod, err := t.Pod(ctx)
	if err != nil {
		return err
	}
	cmName := t.ConfigMapName(pod)
	cfg, cfgErr := t.LoadManifest(ctx, cmName)
	if cfgErr != nil {
		fmt.Fprintf(errw, "warning: %v\n", cfgErr)
	}

	var rows []ProviderStatus
	for _, p := range SidecarProviders(pod) {
		rows = append(rows, ProviderStatus{Provider: p, State: SidecarState(ctx, ex, t.Namespace, pod.Name, p)})
	}
	if groups := cfg.APIGroups(); len(groups) > 0 {
		for _, g := range groups {
			mc := "unlimited"
			if g.MaxConcurrency > 0 {
				mc = fmt.Sprint(g.MaxConcurrency)
			}
			rows = append(rows, ProviderStatus{
				Provider: Provider{Name: g.Name, Type: "api"},
				State:    fmt.Sprintf("%s, max_concurrency %s (no login needed)", strings.Join(g.Protocols, ","), mc),
			})
		}
	} else {
		// ConfigMap unreadable: fall back to the proxy container's env.
		for _, p := range APIProviders(pod, cfg) {
			rows = append(rows, ProviderStatus{Provider: p, State: StateAPIKey + " (no login needed)"})
		}
	}

	fmt.Fprintf(w, "Release %s in namespace %s, pod %s (%s)\n\n", t.Release, t.Namespace, pod.Name, pod.Status.Phase)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if len(rows) == 0 {
		fmt.Fprintln(tw, "No providers configured.")
	} else {
		fmt.Fprintln(tw, "PROVIDER\tTYPE\tSTATUS")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Type, r.State)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	if cfg == nil {
		fmt.Fprintf(w, "Clients: unknown (configmap %s unreadable).\n", cmName)
		return nil
	}
	names := cfg.ClientNames()
	if len(names) == 0 {
		fmt.Fprintln(w, "No clients configured (client keys disabled).")
		return nil
	}
	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CLIENT\tPRIORITY\tENDPOINTS")
	for _, n := range names {
		prio, eps := "-", "*"
		if c := cfg.Clients[n]; c != nil {
			if c.Priority != nil {
				prio = fmt.Sprint(*c.Priority)
			}
			if len(c.Endpoints) > 0 {
				eps = strings.Join(c.Endpoints, ",")
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", n, prio, eps)
	}
	return tw.Flush()
}
