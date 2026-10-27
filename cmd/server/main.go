// forge-server: control plane — API, webhook-style pipeline trigger,
// compiler, and scheduler in one deployable.
package main

import (
	"context"
	"fmt"
	"github.com/priytamjeepandey/forge-ci/internal/buildinfo"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/priytamjeepandey/forge-ci/internal/api"
	"github.com/priytamjeepandey/forge-ci/internal/autoscale"
	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/logstore"
	"github.com/priytamjeepandey/forge-ci/internal/review"
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
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println(buildinfo.Version)
		return
	}
	slog.Info("forge-server", "version", buildinfo.Version)
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

	// High-volume log tier: redis live buffer + blob archive, or the postgres
	// fallback. Backend is resolved from LOG_BACKEND / REDIS_URL (see logstore).
	logs := logstore.Resolve(ctx, st, blobs)
	defer logs.Close()

	go scheduler.New(st, blobs, logs).Run(ctx)
	go autoscale.Loop(ctx, st) // runner pools (no-op until one is configured)
	handler := api.New(st, blobs, logs)
	// Code review: refreshes, reconcile, gates, writes, merge queues, diff retention.
	go review.Loop(ctx, st, blobs, handler.StartQueuePipeline)
	go handler.RunLive(ctx)

	srv := &http.Server{Addr: addr, Handler: handler}
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
