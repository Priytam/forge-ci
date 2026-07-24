package logstore

import (
	"context"
	"fmt"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/redis/go-redis/v9"
)

// redisBackend keeps each running job's log in a capped Redis String, appended
// with APPEND and read by byte offset with GETRANGE, plus a per-job pub/sub
// channel that wakes live-tail (SSE) readers.
//
// Key layout (per job):
//
//	log:{id}:buf    String  — the raw (already-masked) log bytes
//	log:{id}:trunc  String  — "1" once the byte cap fired (drop further output)
//	log:{id}:ch     channel — one PUBLISH per append (a wake signal)
type redisBackend struct {
	rdb *redis.Client
}

func bufKey(id int64) string   { return fmt.Sprintf("log:{%d}:buf", id) }
func truncKey(id int64) string { return fmt.Sprintf("log:{%d}:trunc", id) }
func chanKey(id int64) string  { return fmt.Sprintf("log:{%d}:ch", id) }
func tailKey(id int64) string  { return fmt.Sprintf("log:{%d}:tail", id) }

// appendScript enforces the cumulative byte cap atomically: it appends what
// fits, writes the truncation notice once, sets the trunc flag, and refreshes
// the buffer TTL. Returns {newTotal, truncatedNow(0|1), addedBytes}.
var appendScript = redis.NewScript(`
local trunc = redis.call('GET', KEYS[2])
if trunc == '1' then
  return {redis.call('STRLEN', KEYS[1]), 0, 0}
end
local cap = tonumber(ARGV[2])
local ttl = tonumber(ARGV[4])
local cur = redis.call('STRLEN', KEYS[1])
if cap <= 0 or cur + #ARGV[1] <= cap then
  local newlen = redis.call('APPEND', KEYS[1], ARGV[1])
  if ttl > 0 then redis.call('PEXPIRE', KEYS[1], ttl) end
  return {newlen, 0, #ARGV[1]}
end
local remaining = cap - cur
local added = 0
if remaining > 0 then
  redis.call('APPEND', KEYS[1], string.sub(ARGV[1], 1, remaining))
  added = remaining
end
redis.call('APPEND', KEYS[1], ARGV[3])
added = added + #ARGV[3]
redis.call('SET', KEYS[2], '1')
if ttl > 0 then
  redis.call('PEXPIRE', KEYS[1], ttl)
  redis.call('PEXPIRE', KEYS[2], ttl)
end
return {redis.call('STRLEN', KEYS[1]), 1, added}
`)

func newRedisBackend(ctx context.Context, url string) (*redisBackend, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	// Keep the runner off the hot path: short, bounded timeouts so a slow/absent
	// Redis surfaces as an error fast instead of hanging a log POST.
	opt.DialTimeout = 3 * time.Second
	opt.ReadTimeout = 3 * time.Second
	opt.WriteTimeout = 3 * time.Second
	rdb := redis.NewClient(opt)
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &redisBackend{rdb: rdb}, nil
}

func (b *redisBackend) Kind() string { return "redis" }

func (b *redisBackend) Append(ctx context.Context, jobID int64, p []byte, capBytes int64) (AppendResult, error) {
	res, err := appendScript.Run(ctx, b.rdb,
		[]string{bufKey(jobID), truncKey(jobID)},
		p, capBytes, store.TruncationNotice(capBytes), runningTTL.Milliseconds()).Result()
	if err != nil {
		return AppendResult{}, err
	}
	vals, ok := res.([]interface{})
	if !ok || len(vals) < 3 {
		return AppendResult{}, fmt.Errorf("unexpected append reply %T", res)
	}
	total, _ := vals[0].(int64)
	truncNow, _ := vals[1].(int64)
	added, _ := vals[2].(int64)
	if added > 0 {
		// Wake any live-tail subscribers; they pull the actual delta by offset.
		_ = b.rdb.Publish(ctx, chanKey(jobID), "1").Err()
	}
	return AppendResult{Total: total, TruncatedNow: truncNow == 1}, nil
}

func (b *redisBackend) ReadFrom(ctx context.Context, jobID int64, offset int64) ([]byte, int64, error) {
	total, err := b.rdb.StrLen(ctx, bufKey(jobID)).Result()
	if err != nil {
		return nil, 0, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return nil, total, nil
	}
	// GETRANGE end index is inclusive; -1 means "to the end".
	data, err := b.rdb.GetRange(ctx, bufKey(jobID), offset, -1).Result()
	if err != nil {
		return nil, total, err
	}
	return []byte(data), total, nil
}

func (b *redisBackend) Snapshot(ctx context.Context, jobID int64) ([]byte, error) {
	data, err := b.rdb.Get(ctx, bufKey(jobID)).Result()
	if err == redis.Nil {
		return nil, nil // buffer already gone / never written
	}
	if err != nil {
		return nil, err
	}
	return []byte(data), nil
}

func (b *redisBackend) Truncated(ctx context.Context, jobID int64) (bool, error) {
	v, err := b.rdb.Get(ctx, truncKey(jobID)).Result()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

func (b *redisBackend) Evict(ctx context.Context, jobID int64, ttl time.Duration) error {
	if ttl <= 0 {
		return b.rdb.Del(ctx, bufKey(jobID), truncKey(jobID), tailKey(jobID)).Err()
	}
	pipe := b.rdb.Pipeline()
	pipe.PExpire(ctx, bufKey(jobID), ttl)
	pipe.PExpire(ctx, truncKey(jobID), ttl)
	pipe.PExpire(ctx, tailKey(jobID), ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// GetTail returns the carry-over masking tail (a Redis String); "" when unset.
func (b *redisBackend) GetTail(ctx context.Context, jobID int64) (string, error) {
	v, err := b.rdb.Get(ctx, tailKey(jobID)).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

// SetTail stores the carry-over masking tail with the running TTL; ""=delete.
func (b *redisBackend) SetTail(ctx context.Context, jobID int64, tail string) error {
	if tail == "" {
		return b.rdb.Del(ctx, tailKey(jobID)).Err()
	}
	return b.rdb.Set(ctx, tailKey(jobID), tail, runningTTL).Err()
}

// redisSubscription adapts a Redis PubSub to the Subscription interface: it
// forwards each message as a non-blocking wake on C().
type redisSubscription struct {
	ps     *redis.PubSub
	c      chan struct{}
	cancel context.CancelFunc
}

func (s *redisSubscription) C() <-chan struct{} { return s.c }

func (s *redisSubscription) Close() {
	s.cancel()
	_ = s.ps.Close()
}

func (b *redisBackend) Subscribe(ctx context.Context, jobID int64) (Subscription, bool) {
	subCtx, cancel := context.WithCancel(ctx)
	ps := b.rdb.Subscribe(subCtx, chanKey(jobID))
	sub := &redisSubscription{ps: ps, c: make(chan struct{}, 1), cancel: cancel}
	go func() {
		defer close(sub.c)
		ch := ps.Channel()
		for {
			select {
			case <-subCtx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				// Coalesce: a single pending wake is enough (reader pulls by offset).
				select {
				case sub.c <- struct{}{}:
				default:
				}
			}
		}
	}()
	return sub, true
}

func (b *redisBackend) Close() error { return b.rdb.Close() }
