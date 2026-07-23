# High-volume log architecture

Forge separates a job's **live log** (written continuously while the job runs,
read for the live tail) from its **archived log** (one immutable object per
finished job in the blob store). This keeps Postgres off the hot path: in the
production configuration log bodies never touch the database — only a pointer to
the archived object does.

Two swappable backends implement the same live-buffer interface
(`internal/logstore`):

| Backend | Live buffer | Live tail (SSE) | Finished-job read | Postgres holds |
|---|---|---|---|---|
| `redis` (prod) | capped Redis String per job | Redis pub/sub fan-out | blob object | a pointer only |
| `postgres` (dev) | `job_logs` rows (unchanged) | poll-and-push (1s) | `job_logs` | the log bodies |

```
                 ┌─────────── forge-server ───────────┐
 runner  POST    │  pushLogs: mask (carry-over tail)   │
 log chunk  ───► │        │                            │
                 │        ▼                            │      ┌───────────┐
                 │   logstore.Append ──► APPEND ───────┼────► │   Redis   │
                 │        │              PUBLISH ───────┼────► │ log:{id}: │
                 │        │                            │      │  buf / ch │
                 │        │  (cap → truncation notice) │      └─────┬─────┘
                 │        ▼                            │            │ pub/sub
 GET .../logs    │   FullText / ReadDelta ◄───GETRANGE─┼────────────┤
 GET .../stream  │   streamLogs ◄────── subscribe ─────┼────────────┘
                 │        │                            │
 job completes   │   Archive: Snapshot ─► blob.Put ────┼────► ┌───────────┐
 (terminal)      │        └─► SetLogPointer(jobs)       │      │ blob store│
                 │            Evict(buffer, 60s TTL)   │      │ logs/job- │
                 └─────────────────────────────────────┘      │  <id>.log │
                              │ pointer (log_object_key)       └───────────┘
                              ▼
                        ┌──────────┐
                        │ Postgres │  jobs.log_object_key / log_total_bytes /
                        │          │  log_truncated   (NO bodies in redis mode)
                        └──────────┘
```

## Write path (ingest)

1. The runner batches stdout/stderr and POSTs chunks to
   `POST /api/v1/runner/jobs/{id}/logs` (`maxLogChunk` = 1 MiB per POST).
2. `pushLogs` masks the chunk. Masked variable values can straddle chunk
   boundaries, so a **per-job carry-over tail** (`maskBuf`) holds trailing bytes
   that could still be the prefix of a value completed by the next chunk. This is
   unchanged from the correctness pass.
3. The masked bytes go to `logstore.Append`, which appends to the live buffer and
   **publishes** a wake to the job's channel. A cumulative byte cap
   (`MAX_JOB_LOG_BYTES`) is enforced atomically (a Lua script in the redis
   backend): once crossed it writes a single truncation notice, sets a truncated
   flag, and drops further output — byte-identical to the legacy Postgres path
   (`store.TruncationNotice`).
4. On a **Redis write error** the server logs it, increments
   `forge_log_redis_unavailable_total`, and still returns `202` so the runner is
   never blocked (that window's bytes are lost; the job is not).

## Read path

`GET /api/v1/jobs/{id}/logs` serves two shapes, chosen by `?offset`:

- **no `?offset`** → the full log as `text/plain` (back-compat: the current web
  client GETs `.../logs` and renders the whole body). Byte-identical to the
  legacy output.
- **`?offset=N`** → JSON `{bytes, next_offset, eof}` carrying only the bytes from
  `N` to the current end. `eof` is `true` when the job is terminal and the reader
  has drained to the end.

Resolution: a **running** job (or one terminal-but-not-yet-archived) reads from
the live buffer (redis `GETRANGE`, efficient by offset); a **finished** job in
redis mode reads from the archived blob object; the **postgres** backend always
reads `job_logs` (`string_agg` + read-path masking backstop). Full-text reads
apply the masking backstop; incremental slices rely on ingest-time masking (a
slice must never be re-masked — it could cut through a value).

### Offset semantics

Offsets are **byte offsets into the masked log**. `next_offset` returned by one
read is the `offset` to pass to the next — this makes polling and the SSE replay
resumable and idempotent. Offsets are stable because the buffer is append-only
and masking happens before bytes are stored.

## SSE live tail

`GET /api/v1/jobs/{id}/logs/stream?offset=N` → `text/event-stream`. It replays
from `?offset` (default 0), then live-tails. Each frame is a `log` event with
`{bytes, next_offset, eof}`; a final `eof` event is sent when the job reaches a
terminal state and the reader has drained. It exits on client disconnect
(`r.Context().Done()`), flushes per event, and is a GET (CSRF-exempt; still
requires a session when SSO is enforced).

- **redis backend**: subscribes to the job's pub/sub channel. Each publish is a
  *wake signal only* — the handler then pulls the actual delta by offset, so a
  dropped or late notification can never corrupt or duplicate the stream.
- **postgres backend**: no pub/sub, so the handler **polls** by offset every
  ~1s (poll-and-push). The endpoint therefore exists in both modes.

**Fan-out**: N concurrent SSE clients on one job each hold an independent
subscription and do independent offset reads — no shared cursor, no
head-of-line coupling. Verified with the live subscriber gauge
(`forge_log_sse_subscribers`) and multiple concurrent `curl -N` clients.

## Archive (finished jobs) + Postgres off the hot path

When a job reaches a terminal state, `Archive` (redis backend only):

1. snapshots the live buffer,
2. writes it to the blob store as one object, key `logs/job-<id>.log` (skipped
   when the job produced no output),
3. records the pointer on the job row (`log_object_key`, `log_total_bytes`,
   `log_truncated`),
4. **evicts** the buffer with a short TTL (60s) rather than deleting it, so a
   reader mid-transition is not cut off — reads prefer the object once the
   pointer is set.

Archival is triggered from two places, and is idempotent (a job whose pointer is
set is skipped):

- the runner-complete path (`POST /api/v1/runner/jobs/{id}/complete`), for
  timeliness;
- a scheduler **safety-net sweep** (every 30s) that archives terminal jobs which
  died *without* a runner complete call — stale/overdue/canceled-while-pending —
  whose buffers would otherwise expire unflushed.

In redis mode `job_logs` never accumulates bodies (the retry "attempt N/M"
separator is routed to the live buffer, not the table). In postgres mode
everything is unchanged.

## Backend resolution

`LOG_BACKEND` selects the backend; `REDIS_URL` supplies the connection:

| `LOG_BACKEND` | `REDIS_URL` | Result |
|---|---|---|
| _(unset)_ | set + reachable | **redis** |
| _(unset)_ | unset | **postgres** |
| `redis` | set + reachable | **redis** |
| `redis` | unset / unreachable | **postgres** (loud warning, no crash) |
| `postgres` | any | **postgres** |

A `LOG_BACKEND=redis` that cannot `PING` Redis at startup **degrades to
postgres** with a warning rather than crashing — logs stay durable in the DB and
SSE falls back to polling. Redis client timeouts are short (3s) so a slow/absent
Redis surfaces fast instead of hanging a log POST.

## Degradation when Redis is down

- **At startup**: fall back to the postgres backend (above).
- **Mid-flight write failure**: the log POST still returns `202` (runner never
  hangs); the error is logged and counted. That window's bytes are lost, the job
  is not.
- **Reads / SSE**: in postgres mode SSE polls; in redis mode a read error
  surfaces as a 500 to the UI, which retries.

## Retention of log objects

The hourly retention GC (`RETENTION_DAYS`) deletes archived log **objects** from
the blob store alongside artifact blobs, before cascade-deleting the pipeline
rows (`store.ExpiredLogObjectKeys`). Empty-key pointers (jobs that produced no
output) are skipped. This extends the existing artifact-blob retention path.

## Multi-instance notes

- **SSE across replicas**: Redis pub/sub broadcasts to every subscriber
  regardless of which server replica published the append, so live tail works
  with any number of server replicas behind a load balancer.
- **Masking carry-over is per-replica**: the `maskBuf` chunk-boundary tail lives
  in the server process. If a runner's consecutive log POSTs for one job are
  load-balanced across *different* replicas, a secret split exactly on a POST
  boundary could evade the per-chunk pass — the read-path masking backstop
  (applied to full-text reads) is the second line of defence. Pinning a runner's
  connection (or a single-replica ingest tier) avoids the window entirely.
- **Archive races**: archival is idempotent and the safety-net sweep re-archives
  anything a crashed replica missed; the short eviction TTL keeps in-flight
  readers alive across the pointer flip.

## Observability

`GET /api/v1/metrics` exposes Prometheus-format counters (no external client
library): `forge_log_bytes_ingested_total`, `forge_log_truncations_total`,
`forge_log_archive_flush_total`, `forge_log_archive_flush_errors_total`,
`forge_log_archive_flush_millis_total`, `forge_log_redis_unavailable_total`, the
`forge_log_sse_subscribers` gauge, and `forge_log_backend_info{backend=...}`.
The endpoint is auth-exempt (scrapeable without a session).
