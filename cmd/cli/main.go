// Command lpmctl is the user-side CLI of an llm-proxy-manifestd Helm release:
// it reads client keys, shows provider login status and runs the interactive
// provider login. It runs on the user's machine with their kubeconfig; it is
// not shipped in any image.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/lerenn/llm-proxy-manifestd/internal/lpmctl"
)

var version = "dev"

type globals struct {
	namespace, release, kubeContext, kubeconfig string
}

// connect loads the kubeconfig (KUBECONFIG / ~/.kube/config, or --kubeconfig)
// with the --context and -n overrides, and resolves the release.
func (g *globals) connect(ctx context.Context) (*lpmctl.Target, *rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if g.kubeconfig != "" {
		rules.ExplicitPath = g.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: g.kubeContext}
	if g.namespace != "" {
		overrides.Context.Namespace = g.namespace
	}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	ns, _, err := cc.Namespace()
	if err != nil {
		return nil, nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	t, err := lpmctl.Resolve(ctx, cs, ns, g.release)
	if err != nil {
		return nil, nil, err
	}
	return t, cfg, nil
}

func newRoot() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "lpmctl",
		Short:         "Operate an llm-proxy-manifestd release from your machine (keys, provider status, login)",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&g.namespace, "namespace", "n", "", "namespace of the release (default: the kubeconfig context's namespace)")
	pf.StringVar(&g.release, "release", "", "Helm release name (default: auto-detected when the namespace has exactly one)")
	pf.StringVar(&g.kubeContext, "context", "", "kubeconfig context to use")
	pf.StringVar(&g.kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")

	var hide bool
	keys := &cobra.Command{
		Use:   "keys [client]",
		Short: "Print the client API keys (NAME/KEY table), or only one client's key",
		Long: "Without argument, print a NAME/KEY table of the client keys stored in the release's\n" +
			"client-keys Secret. With a client name, print only that key followed by a newline,\n" +
			"so it can be piped or captured: export ANTHROPIC_API_KEY=$(lpmctl keys opencode)",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, _, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return lpmctl.Keys(cmd.Context(), t, cmd.OutOrStdout(), name, hide)
		},
	}
	keys.Flags().BoolVar(&hide, "hide", false, "mask the keys in the table")

	status := &cobra.Command{
		Use:   "status",
		Short: "Show each provider's login status and the configured clients",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, cfg, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			ex := &lpmctl.KubeExecutor{Config: cfg, Client: t.Client}
			return lpmctl.Status(cmd.Context(), t, ex, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}

	login := &cobra.Command{
		Use:   "login <provider> [-- command...]",
		Short: "Log a subscription provider in (interactive exec into its sidecar)",
		Long: "Open an interactive terminal in the provider's sidecar container and run its CLI\n" +
			"login (claude: `claude` then /login; codex: `codex login --device-auth`).\n" +
			"Arguments after -- replace the login command. API-key providers are refused.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, override := args[0], args[1:]
			if dash := cmd.ArgsLenAtDash(); dash >= 0 && dash != 1 {
				return fmt.Errorf("usage: lpmctl login <provider> [-- command...]")
			}
			t, cfg, err := g.connect(cmd.Context())
			if err != nil {
				return err
			}
			fd := int(os.Stdin.Fd())
			o := lpmctl.LoginOptions{
				Command: override,
				Stdin:   os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
				TTY: term.IsTerminal(fd) && term.IsTerminal(int(os.Stdout.Fd())),
				BeforeExec: func() (func(), error) {
					st, err := term.MakeRaw(fd)
					if err != nil {
						return nil, fmt.Errorf("terminal raw mode: %w", err)
					}
					return func() { _ = term.Restore(fd, st) }, nil
				},
			}
			if o.TTY {
				q := newSizeQueue(int(os.Stdout.Fd()))
				defer q.stop()
				o.SizeQueue = q
			}
			ex := &lpmctl.KubeExecutor{Config: cfg, Client: t.Client}
			return lpmctl.Login(cmd.Context(), t, ex, name, o)
		},
	}

	root.AddCommand(keys, status, login, newOpenCodeCmd(g))
	return root
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := newRoot().ExecuteContext(ctx); err != nil {
		code := 1
		var ee *lpmctl.ExitError
		if errors.As(err, &ee) {
			code = ee.Code
		} else {
			fmt.Fprintln(os.Stderr, "lpmctl:", err)
		}
		stop()
		os.Exit(code)
	}
}
