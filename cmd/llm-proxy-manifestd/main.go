// Command llm-proxy-manifestd is a headless, manifest-driven reverse proxy
// for OpenAI-compatible endpoints with per-pool concurrency limits.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lerenn/llm-proxy-manifestd/internal/config"
	"github.com/lerenn/llm-proxy-manifestd/internal/proxy"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "/etc/llm-proxy-manifestd/config.yaml", "path to the YAML manifest")
	shutdownTimeout := flag.Duration("shutdown-timeout", 10*time.Minute, "max time to let in-flight requests finish on shutdown")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	loader := config.NewLoader(*cfgPath, logger)
	if err := loader.Reload(); err != nil {
		return fmt.Errorf("initial config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		if err := loader.Watch(ctx); err != nil {
			logger.Error("config watcher stopped", "error", err)
		}
	}()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := loader.Reload(); err != nil {
					logger.Error("config reload rejected, keeping previous config", "error", err)
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              loader.State().Config.Server.Listen,
		Handler:           proxy.New(loader.State, logger),
		ReadHeaderTimeout: 15 * time.Second,
		// No Read/Write timeouts: streams can be long-lived.
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr, "version", version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down; waiting for in-flight requests", "timeout", shutdownTimeout.String())
	sctx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
