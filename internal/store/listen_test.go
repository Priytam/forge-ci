package store

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPoolConfigSizing(t *testing.T) {
	base := "postgres://forge:forge@localhost:5433/forge?sslmode=disable"

	t.Setenv("DATABASE_MAX_CONNS", "")
	cfg, err := PoolConfig(base)
	if err != nil || cfg.MaxConns != DefaultMaxConns {
		t.Fatalf("default: MaxConns=%d err=%v, want %d", cfg.MaxConns, err, DefaultMaxConns)
	}

	t.Setenv("DATABASE_MAX_CONNS", "35")
	if cfg, _ = PoolConfig(base); cfg.MaxConns != 35 {
		t.Fatalf("env: MaxConns=%d, want 35", cfg.MaxConns)
	}
	if cfg, _ = PoolConfig(base + "&pool_max_conns=7"); cfg.MaxConns != 7 {
		t.Fatalf("DSN pool_max_conns must win over env: MaxConns=%d, want 7", cfg.MaxConns)
	}

	for _, bad := range []string{"0", "-3", "lots"} {
		t.Setenv("DATABASE_MAX_CONNS", bad)
		if _, err := PoolConfig(base); err == nil {
			t.Fatalf("DATABASE_MAX_CONNS=%q accepted", bad)
		}
	}
}

// TestListenersDoNotUsePoolConnections is the regression test for the pool
// starvation outage: with a 2-connection pool and all three LISTENs running,
// both pool connections must stay free for queries, the LISTENs must share one
// connection, and notifications must survive that connection being killed.
func TestListenersDoNotUsePoolConnections(t *testing.T) {
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, appBaseDSN())
	if err != nil {
		t.Skipf("postgres not reachable (%v)", err)
	}
	defer admin.Close(ctx)
	dbName := fmt.Sprintf("forge_listen_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Skipf("cannot create throwaway db (%v)", err)
	}
	defer admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)") //nolint:errcheck
	u, _ := url.Parse(appBaseDSN())
	u.Path = "/" + dbName
	q := u.Query()
	q.Set("pool_max_conns", "2")
	u.RawQuery = q.Encode()
	st, err := New(ctx, u.String())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	got := make(chan string, 16)
	go st.ListenChanges(lctx, func(id string) { got <- "changes:" + id })          //nolint:errcheck
	go st.ListenLive(lctx, func(p string) { got <- "live:" + p })                  //nolint:errcheck
	go st.ListenLiveOps(lctx, func(id int64) { got <- fmt.Sprintf("ops:%d", id) }) //nolint:errcheck

	listenerPID := func() (pids []int32) {
		rows, err := admin.Query(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=$1 AND application_name=$2`, dbName, listenerAppName)
		if err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p int32
			_ = rows.Scan(&p)
			pids = append(pids, p)
		}
		return pids
	}
	// notifyUntil re-sends a notification until it arrives (the listener may
	// still be connecting) or the deadline passes.
	notifyUntil := func(channel, payload, want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := st.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, channel, payload); err != nil {
				t.Fatalf("pg_notify: %v", err)
			}
			timeout := time.After(300 * time.Millisecond)
		drain:
			for {
				select {
				case g := <-got:
					if g == want {
						return
					}
				case <-timeout:
					break drain
				}
			}
		}
		t.Fatalf("never received %q", want)
	}

	notifyUntil(changesChannel, "41", "changes:41")
	notifyUntil(liveChannel, `{"id":1}`, `live:{"id":1}`)
	notifyUntil(opsChannel, "9", "ops:9")

	if n := len(listenerPID()); n != 1 {
		t.Fatalf("want exactly one listener connection, got %d", n)
	}
	if acq := st.pool.Stat().AcquiredConns(); acq != 0 {
		t.Fatalf("listeners hold %d pool connections, want 0", acq)
	}
	// Both pool connections can be checked out at once while listening.
	actx, acancel := context.WithTimeout(ctx, 2*time.Second)
	defer acancel()
	c1, err := st.pool.Acquire(actx)
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	c2, err := st.pool.Acquire(actx)
	if err != nil {
		t.Fatalf("acquire 2 (pool starved by listeners?): %v", err)
	}
	c1.Release()
	c2.Release()

	// Kill the listener's backend: it must reconnect and keep delivering.
	for _, pid := range listenerPID() {
		if _, err := admin.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatalf("terminate listener: %v", err)
		}
	}
	notifyUntil(changesChannel, "42", "changes:42")

	// With no subscribers left the listener connection closes.
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for len(listenerPID()) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("listener connection still open after the last subscriber left")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
