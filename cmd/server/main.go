// forge-server: control plane — API, webhook-style pipeline trigger,
// compiler, and scheduler in one deployable.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/priytamjeepandey/forge-ci/internal/api"
	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/scheduler"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	addr := envOr("LISTEN_ADDR", ":8080")
	dsn := envOr("DATABASE_URL", "postgres://forge:forge@localhost:5433/forge?sslmode=disable")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(ctx, dsn)
	if err != nil {
		slog.Error("store init failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	blobs, err := blob.FromEnv()
	if err != nil {
		slog.Error("artifact store init failed", "err", err)
		os.Exit(1)
	}
	slog.Info("artifact store configured", "backend", blobs.Kind())

	go scheduler.New(st, blobs).Run(ctx)

	srv := &http.Server{Addr: addr, Handler: api.New(st, blobs)}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	slog.Info("forge-server listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}
