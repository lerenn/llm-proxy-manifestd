package lpmctl

import (
	"context"
	"fmt"
	"io"
	"strings"

	"k8s.io/client-go/tools/remotecommand"
)

// loginFlow is the interactive login of one sidecar type.
type loginFlow struct {
	Command []string
	Hint    string
}

// loginFlows maps sidecar types to their CLI login. The login directory
// (CODEX_HOME) is already set in the container env and
// lives on the provider's PVC.
var loginFlows = map[string]loginFlow{
	"codex": {
		Command: []string{"codex", "login", "--device-auth"},
		Hint: `Headless login (device code): open the printed URL in a browser on any device,
sign in with your ChatGPT account and enter the one-time code shown here.
The command returns once the login is stored on the provider's volume.`,
	},
}

// LoginOptions are the terminal and overrides of `lpmctl login`.
type LoginOptions struct {
	// Command overrides the login command (args after "--").
	Command        []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	TTY            bool
	SizeQueue      remotecommand.TerminalSizeQueue
	// BeforeExec runs right before the exec starts (e.g. to switch the
	// local terminal to raw mode); its returned func runs after. Optional.
	BeforeExec func() (restore func(), err error)
}

// Login implements `lpmctl login <provider>`: an interactive exec into the
// provider's sidecar running the CLI login. API-key providers are refused.
func Login(ctx context.Context, t *Target, ex Executor, name string, o LoginOptions) error {
	pod, err := t.Pod(ctx)
	if err != nil {
		return err
	}
	var prov *Provider
	sidecars, err := t.Adapters(ctx, pod)
	if err != nil {
		return err
	}
	for i := range sidecars {
		if sidecars[i].Name == name {
			prov = &sidecars[i]
		}
	}
	if prov == nil {
		cfg, _ := t.LoadManifest(ctx, t.ConfigMapName(pod))
		for _, g := range cfg.APIGroups() {
			if g.Name == name {
				return fmt.Errorf("provider %q is an API provider (%s): no login needed, it authenticates with an API key from a Secret", name, strings.Join(g.Protocols, ","))
			}
		}
		for _, p := range APIProviders(pod, nil) {
			if p.Name == name {
				return fmt.Errorf("provider %q is an API-key provider: there is nothing to log in to, its key comes from secret %s (key %s)", name, p.SecretName, p.SecretKey)
			}
		}
		names := make([]string, 0, len(sidecars))
		for _, p := range sidecars {
			names = append(names, p.Name)
		}
		avail := "none"
		if len(names) > 0 {
			avail = strings.Join(names, ", ")
		}
		return fmt.Errorf("no subscription provider %q in pod %s (available: %s)", name, pod.Name, avail)
	}

	flow, known := loginFlows[prov.Type]
	cmd := o.Command
	if len(cmd) == 0 {
		if !known {
			return fmt.Errorf("provider %q has type %q, for which lpmctl knows no login command: pass one after --", name, prov.Type)
		}
		cmd = flow.Command
	}
	if !o.TTY {
		return fmt.Errorf("login is interactive: run lpmctl login from a terminal")
	}
	target := pod.Name
	if prov.Pod != "" {
		target = prov.Pod
	}
	if o.Stderr != nil {
		fmt.Fprintf(o.Stderr, "Logging in provider %s (type %s) in pod %s, container %s: %s\n", name, prov.Type, target, prov.Name, strings.Join(cmd, " "))
		if known && len(o.Command) == 0 {
			fmt.Fprintf(o.Stderr, "%s\n\n", flow.Hint)
		}
	}
	if o.BeforeExec != nil {
		restore, err := o.BeforeExec()
		if err != nil {
			return err
		}
		if restore != nil {
			defer restore()
		}
	}
	return ex.Exec(ctx, ExecRequest{
		Namespace: t.Namespace, Pod: target, Container: prov.Name,
		Command: cmd,
		Stdin:   o.Stdin, Stdout: o.Stdout, Stderr: o.Stderr,
		TTY:       true,
		SizeQueue: o.SizeQueue,
	})
}
