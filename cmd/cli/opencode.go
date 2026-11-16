package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lerenn/llm-proxy-manifestd/internal/lpmctl"
)

func newOpenCodeCmd(g *globals) *cobra.Command {
	o := lpmctl.OpenCodeOptions{}
	var client, keyEnv string
	c := &cobra.Command{
		Use:     "opencode",
		Aliases: []string{"opencode-models"},
		Short:   "Generate opencode's model lists from the proxy's live /v1/models",
		Long: "opencode cannot discover the models of a custom provider, so this command reads the\n" +
			"proxy's GET /v1/models (OpenAI format and Anthropic format) and rewrites the `models`\n" +
			"map of the two opencode providers. The proxy stays the source of truth.\n\n" +
			"Default is a dry run; --write applies it (atomic, <config>.bak saved first, file mode\n" +
			"kept). Only the `models` of the two named providers are replaced: models added by hand\n" +
			"to them are removed, models removed from the proxy disappear; settings of models that\n" +
			"stay are kept. Everything else in the file is preserved. A provider block is created\n" +
			"if missing. The config must be plain JSON (comments/trailing commas: nothing is\n" +
			"written, the snippet is printed). Both `provider` and `providers` config styles are\n" +
			"detected; `provider` is used for a new file.\n\n" +
			"The client key comes from the cluster Secret (--client, optional when the release has\n" +
			"one client) or from an environment variable (--key-env). It is never printed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.URL == "" {
				o.URL = os.Getenv("LPMCTL_PROXY_URL")
			}
			if o.URL == "" {
				return fmt.Errorf("no proxy URL: pass --url (base URL including /v1) or set LPMCTL_PROXY_URL")
			}
			if o.ConfigPath == "" {
				p, err := lpmctl.DefaultOpenCodeConfigPath()
				if err != nil {
					return err
				}
				o.ConfigPath = p
			}
			if keyEnv != "" {
				o.Key = strings.TrimSpace(os.Getenv(keyEnv))
				if o.Key == "" {
					return fmt.Errorf("environment variable %s is empty or unset", keyEnv)
				}
			} else {
				t, _, err := g.connect(cmd.Context())
				if err != nil {
					return err
				}
				if o.Key, err = t.ClientKey(cmd.Context(), client); err != nil {
					return err
				}
			}
			o.Stdout, o.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			return lpmctl.OpenCode(cmd.Context(), o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.URL, "url", "", "proxy base URL including /v1 (default: $LPMCTL_PROXY_URL)")
	f.StringVar(&client, "client", "", "client whose key to use (default: the only client of the release)")
	f.StringVar(&keyEnv, "key-env", "", "read the key from this environment variable instead of the cluster")
	f.StringVar(&o.ConfigPath, "config", "", "opencode config file (default: $OPENCODE_CONFIG or ~/.config/opencode/opencode.json)")
	f.StringVar(&o.AnthropicProvider, "anthropic-provider", lpmctl.DefaultAnthropicProvider, "opencode provider id for the Anthropic endpoints")
	f.StringVar(&o.OpenAIProvider, "openai-provider", lpmctl.DefaultOpenAIProvider, "opencode provider id for the OpenAI endpoints")
	f.BoolVar(&o.Write, "write", false, "apply the change (default: dry run)")
	f.BoolVar(&o.SetKey, "set-key", false, "also write the client key as the providers' apiKey")
	f.BoolVar(&o.NoAnthropic, "no-anthropic", false, "skip the Anthropic provider")
	f.BoolVar(&o.NoOpenAI, "no-openai", false, "skip the OpenAI provider")
	return c
}
