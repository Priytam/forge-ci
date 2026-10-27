// Package agent runs a Forge Live provider and ships what it reports to
// Forge. It dials out (Forge never reaches into a cluster or holds its
// credentials), sends one batch at a time and waits for the answer — while
// it waits, changes keep coalescing in the tracker, and when too much is
// waiting the provider blocks: that is the whole backpressure story.
package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Config is one agent.
type Config struct {
	Provider live.Provider
	URL      string // Forge's base URL
	Token    string // the target's agent token
	Version  string
	Client   *http.Client
	// Window is the coalescing window (default live.DefaultWindow).
	Window time.Duration
	// MaxBatch is the most bytes of resources one batch carries (default 2 MiB).
	MaxBatch int
	// Heartbeat: an empty batch is sent after this long without one, so
	// Forge knows the agent is alive (default 30s).
	Heartbeat time.Duration
}

// errResync: Forge asked for a fresh epoch.
var errResync = errors.New("forge asked for a fresh report")

// Run reports until ctx ends. Each epoch is one run of the provider; a new
// one starts when the provider asks for it, when Forge does, or after the
// provider fails.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Window <= 0 {
		cfg.Window = live.DefaultWindow
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 2 << 20
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 30 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: time.Minute}
	}
	a := &agent{cfg: cfg, started: time.Now()}
	for failures := 0; ctx.Err() == nil; {
		err := a.epoch(ctx)
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, live.ErrRestart), errors.Is(err, errResync):
			slog.Info("live: starting a fresh report", "reason", err)
			failures = 0
		default:
			failures++
			wait := backoff(failures)
			slog.Error("live: provider stopped", "provider", cfg.Provider.Name(), "err", err, "retry_in", wait)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}
	return nil
}

type agent struct {
	cfg     Config
	started time.Time
	sent    atomic.Int64
}

func backoff(n int) time.Duration {
	return min(time.Second<<min(n, 6), time.Minute)
}

func (a *agent) epoch(ctx context.Context) error {
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	epoch := hex.EncodeToString(id)
	tr := live.NewTracker(a.cfg.Window)
	pctx, cancel := context.WithCancel(ctx)
	watchErr := make(chan error, 1)
	go func() { watchErr <- a.cfg.Provider.Watch(pctx, tr) }()
	stopped := false
	defer func() {
		cancel()
		tr.Close()
		if !stopped {
			<-watchErr
		}
	}()

	tick := time.NewTicker(a.cfg.Window / 5)
	defer tick.Stop()
	var seq int64
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-watchErr:
			stopped = true
			if err == nil {
				err = errors.New("provider stopped")
			}
			return err
		case <-tick.C:
		}
		deltas, done := tr.Take(a.cfg.MaxBatch)
		if len(deltas) == 0 && !done && time.Since(last) < a.cfg.Heartbeat {
			continue
		}
		seq++
		b := live.Batch{Epoch: epoch, Seq: seq, Provider: a.cfg.Provider.Name(), Capabilities: a.cfg.Provider.Capabilities(),
			Version: a.cfg.Version, Synced: done, Deltas: deltas, Stats: a.stats(tr)}
		body, err := encode(b)
		if err != nil {
			return err
		}
		// The same batch until Forge has it: a retry after a lost answer is
		// recognised by its (epoch, seq) and not applied twice.
		for attempt := 1; ; attempt++ {
			ack, err := a.post(ctx, body)
			if err == nil {
				if ack.Resync {
					return errResync
				}
				break
			}
			if ctx.Err() != nil {
				return nil
			}
			wait := backoff(attempt)
			slog.Warn("live: sending to Forge failed", "seq", seq, "err", err, "retry_in", wait)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
		}
		last = time.Now()
		if done {
			objects, apps := tr.Counts()
			slog.Info("live: initial report sent", "epoch", epoch, "apps", apps, "objects", objects, "batches", seq)
		}
	}
}

func encode(b live.Batch) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a *agent) post(ctx context.Context, body []byte) (live.Ack, error) {
	var ack live.Ack
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.cfg.URL, "/")+"/api/v1/live/agent/batches", bytes.NewReader(body))
	if err != nil {
		return ack, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		return ack, err
	}
	defer resp.Body.Close()
	a.sent.Add(int64(len(body)))
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return ack, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return ack, json.NewDecoder(resp.Body).Decode(&ack)
}

func (a *agent) stats(tr *live.Tracker) live.AgentStats {
	objects, apps := tr.Counts()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	st := live.AgentStats{
		Objects: objects, Apps: apps, HeapBytes: m.HeapInuse, BytesSent: a.sent.Load(),
		CPUSeconds: float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6,
		Uptime:     time.Since(a.started).Seconds(),
	}
	if c, ok := a.cfg.Provider.(interface{ Calls() int64 }); ok {
		st.ProviderCalls = c.Calls()
	}
	return st
}
