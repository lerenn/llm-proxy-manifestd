// Command codex exposes the OpenAI chat-completions API in front of the
// official `codex app-server` (JSON-RPC over stdio), one process and one fresh
// thread per request. It is meant to run as a sidecar of llm-proxy-manifestd,
// one per subscription login.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/common"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envDuration(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			return p
		}
		log.Printf("ignoring invalid %s=%q", k, v)
	}
	return d
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ready" {
		os.Exit(common.Probe(env("ADAPTER_LISTEN", "127.0.0.1:9100")))
	}
	timeout := flag.Duration("shutdown-timeout", envDuration("ADAPTER_SHUTDOWN_TIMEOUT", 10*time.Minute), "max time to let in-flight requests finish on SIGTERM/SIGINT")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	srv := &Server{
		Bin:       env("CODEX_BIN", "codex"),
		CodexHome: env("CODEX_HOME", "/data/codex"),
	}
	addr := env("ADAPTER_LISTEN", "127.0.0.1:9100")
	log.Printf("adapter-codex listening on %s (codex home %s)", addr, srv.CodexHome)
	hs := &http.Server{Addr: addr, Handler: common.Gate(common.RequireToken(os.Getenv("ADAPTER_TOKEN"), srv.Handler())), ReadHeaderTimeout: 10 * time.Second}
	if err := common.Serve(ctx, hs, *timeout); err != nil {
		log.Fatal(err)
	}
}
