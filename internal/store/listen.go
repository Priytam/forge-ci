package store

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// The server's LISTENs (code-review changes, Live hub, Live ops) share one
// dedicated connection opened outside the pool. Holding a pooled connection
// per LISTEN used to leave a 4-connection pool with one connection for every
// API request, which stalled all routes under a burst of pipelines.

// listenChannels are LISTENed on every listener connection. The set is small
// and fixed, so a channel nobody subscribes to costs nothing but a dropped
// notification.
var listenChannels = []string{changesChannel, liveChannel, opsChannel}

// listenerAppName marks the listener's backend in pg_stat_activity.
const listenerAppName = "forge-listener"

// listenSubBuffer is how many notifications a slow subscriber may lag behind
// before further ones are dropped for it.
const listenSubBuffer = 1024

type listenSub struct {
	channel string
	ch      chan string
}

// notifier owns the listener connection. It runs while anyone is subscribed
// and reconnects (with backoff) when the connection drops.
type notifier struct {
	connect func(ctx context.Context) (*pgx.Conn, error)

	mu     sync.Mutex
	subs   map[*listenSub]struct{}
	cancel context.CancelFunc
	done   chan struct{}
	closed bool
}

func newNotifier(connect func(ctx context.Context) (*pgx.Conn, error)) *notifier {
	return &notifier{connect: connect, subs: map[*listenSub]struct{}{}}
}

func (n *notifier) add(channel string) *listenSub {
	s := &listenSub{channel: channel, ch: make(chan string, listenSubBuffer)}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.subs[s] = struct{}{}
	if n.cancel == nil && !n.closed {
		ctx, cancel := context.WithCancel(context.Background())
		n.cancel, n.done = cancel, make(chan struct{})
		go n.run(ctx, n.done)
	}
	return s
}

func (n *notifier) remove(s *listenSub) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.subs, s)
	if len(n.subs) == 0 && n.cancel != nil {
		n.cancel()
		n.cancel = nil
	}
}

// close stops the listener for good and waits for its connection to close.
func (n *notifier) close() {
	n.mu.Lock()
	n.closed = true
	cancel, done := n.cancel, n.done
	n.cancel = nil
	n.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (n *notifier) dispatch(channel, payload string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for s := range n.subs {
		if s.channel != channel {
			continue
		}
		select {
		case s.ch <- payload:
		default:
			slog.Warn("postgres listener: subscriber is behind, dropping a notification", "channel", channel)
		}
	}
}

func (n *notifier) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	backoff := time.Second
	for ctx.Err() == nil {
		listening, err := n.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if listening {
			backoff = time.Second
		}
		slog.Warn("postgres listener: connection lost, reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// session opens one connection, LISTENs and dispatches until it fails.
// listening reports whether it got as far as LISTEN.
func (n *notifier) session(ctx context.Context) (listening bool, err error) {
	conn, err := n.connect(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	for _, ch := range listenChannels {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return false, err
		}
	}
	for {
		msg, err := conn.WaitForNotification(ctx)
		if err != nil {
			return true, err
		}
		n.dispatch(msg.Channel, msg.Payload)
	}
}

// listen calls fn with each notification on channel until ctx ends. fn runs
// on the caller's goroutine, so a slow handler delays only itself.
func (s *Store) listen(ctx context.Context, channel string, fn func(payload string)) error {
	sub := s.notify.add(channel)
	defer s.notify.remove(sub)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case p := <-sub.ch:
			fn(p)
		}
	}
}

// listenerConnect opens the dedicated listener connection with the pool's
// settings (same DSN, TLS and credentials) but outside its MaxConns.
func (s *Store) listenerConnect(ctx context.Context) (*pgx.Conn, error) {
	cfg := s.pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = listenerAppName
	return pgx.ConnectConfig(ctx, cfg)
}
