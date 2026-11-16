// Command checker validates a llm-proxy-manifestd manifest using the exact
// loading code of the proxy. Non-fatal warnings (for example an endpoint name
// defined for only one protocol) go to stderr and do not change the exit code. It is meant for init containers, Helm hooks and
// CI steps.
//
// Environment variables referenced as ${VAR} are expanded exactly as the
// proxy does, so unset variables expand to the empty string and do not make
// validation fail: secrets need not be present. Error messages never echo
// secret values.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
)

var version = "dev"

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run returns the process exit code: 0 valid, 1 invalid, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("checker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("f", "", "path to the YAML manifest (required)")
	fs.StringVar(path, "config", "", "alias of -f")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	if *path == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: checker -f <config.yaml>")
		return 2
	}
	cfg, err := config.LoadFile(*path)
	if err != nil {
		fmt.Fprintf(stderr, "invalid config %s: %v\n", *path, err)
		return 1
	}
	warnMissingKeys(cfg, stderr)
	for _, w := range cfg.Warnings() {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	fmt.Fprintf(stdout, "config ok: %d pools, %d anthropic endpoints, %d openai endpoints\n",
		len(cfg.Pools), len(cfg.Endpoints[config.ProtocolAnthropic]), len(cfg.Endpoints[config.ProtocolOpenAI]))
	return 0
}

// warnMissingKeys prints a warning for every client without a key file in
// server.client_keys_dir. It only warns: the directory (a mounted Secret) may
// not exist where the checker runs. Key contents are never read or printed.
func warnMissingKeys(cfg *config.Config, w io.Writer) {
	dir := cfg.Server.ClientKeysDir
	if len(cfg.Clients) == 0 {
		return
	}
	if _, err := os.Stat(dir); err != nil {
		fmt.Fprintf(w, "warning: client keys directory %s is not readable here; cannot check that every client has a key\n", dir)
		return
	}
	names := make([]string, 0, len(cfg.Clients))
	for n := range cfg.Clients {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if st, err := os.Stat(filepath.Join(dir, n)); err != nil || st.Size() == 0 {
			fmt.Fprintf(w, "warning: client %q has no key in %s; it will be rejected\n", n, dir)
		}
	}
}
