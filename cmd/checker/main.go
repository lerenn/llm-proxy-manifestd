// Command checker validates a llm-proxy-manifestd manifest using the exact
// loading code of the proxy. It is meant for init containers, Helm hooks and
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
	fmt.Fprintf(stdout, "config ok: %d pools, %d routes\n", len(cfg.Pools), len(cfg.Routes))
	return 0
}
