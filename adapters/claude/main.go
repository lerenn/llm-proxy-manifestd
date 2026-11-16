// Command adapter-claude exposes an OpenAI-compatible API in front of the
// official `claude` CLI, one process per request. It is meant to run as a
// sidecar of llm-proxy-manifestd, one per subscription account.
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	srv := &Server{
		Bin:       env("CLAUDE_BIN", "claude"),
		ConfigDir: env("CLAUDE_CONFIG_DIR", "/data/claude"),
	}
	addr := env("ADAPTER_LISTEN", "127.0.0.1:9100")
	log.Printf("adapter-claude listening on %s (config dir %s)", addr, srv.ConfigDir)
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(hs.ListenAndServe())
}
