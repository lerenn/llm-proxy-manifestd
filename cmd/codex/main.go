// Command codex exposes the OpenAI chat-completions API in front of the
// official `codex app-server` (JSON-RPC over stdio), one process and one fresh
// thread per request. It is meant to run as a sidecar of llm-proxy-manifestd,
// one per subscription login.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/adapter/common"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ready" {
		os.Exit(common.Probe(env("ADAPTER_LISTEN", "127.0.0.1:9100")))
	}
	srv := &Server{
		Bin:       env("CODEX_BIN", "codex"),
		CodexHome: env("CODEX_HOME", "/data/codex"),
	}
	addr := env("ADAPTER_LISTEN", "127.0.0.1:9100")
	log.Printf("adapter-codex listening on %s (codex home %s)", addr, srv.CodexHome)
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.ListenAndServe())
}
