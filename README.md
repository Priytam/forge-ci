# Forge CI

A GitLab-style CI/CD engine: YAML pipelines compiled to a job DAG, a
Postgres-backed scheduler, pull-based runners with pluggable executors, an
approval gate for protected environments, and a React dashboard.

Monorepo with three deployables:

| Deployable | Path | What it is |
|---|---|---|
| `forge-server` | `cmd/server` | Control plane: REST API, YAML→DAG compiler, scheduler, approvals |
| `forge-runner` | `cmd/runner` | Pull-based build agent; `shell` and `docker` executors |
| web | `web/` | React dashboard (pipeline list, stage view, live logs, approve/reject) |

## Architecture

```
 POST /pipelines (webhook/UI)                       ┌──────────────┐
 ──────────► forge-server ── compiler ── DAG ──────►│   Postgres   │
             │        ▲                             │ (single SoT) │
             │  scheduler tick: promote/block/      └──────────────┘
             │  cancel/expire/fail-stale (idempotent SQL)
             ▼
     runner protocol (HTTP long-poll)
 ◄── acquire (FOR UPDATE SKIP LOCKED) ── forge-runner ×N
 ──► logs (streamed chunks) / heartbeat / complete       │
                                                shell | docker executor
```

Job state machine:
`created → (blocked →) pending → running → success | failed | canceled`

- **created → pending** when all `needs` succeeded; **→ blocked** instead when
  the job targets a protected environment (approval required).
- **blocked → pending** once `required_approvals` approved votes are recorded;
  **→ failed** on any rejection or approval timeout.
- **running → failed** automatically when a runner stops heartbeating.
- Jobs whose dependencies fail are canceled.

Approval policy lives in the `protected_environments` table — server-side, so
editing pipeline YAML in a PR cannot weaken the gate. Votes are append-only in
`job_approvals` (one vote per approver per job).

## Pipeline DSL

```yaml
auto_cancel: true            # optional (default true): a newer pipeline for the
                             # same repo+ref cancels this one's non-terminal jobs
default:
  timeout: 1h                # per-job TTL for jobs without their own
  retry: 0                   # per-job retry count for jobs without their own
stages: [build, test, deploy]
jobs:
  build-app:
    stage: build
    image: alpine:3          # used by the docker executor
    script:
      - echo "compiling..."
  unit-tests:
    stage: test              # no explicit needs -> depends on previous stage
    retry: 2                 # retry on failure up to N times (0..10)
    script: [echo testing]
  deploy-dev:
    stage: deploy
    except: [main]           # ref-aware DAG: skipped on main
    script: [echo dev deploy]
  deploy-prod:
    stage: deploy
    needs: [unit-tests]      # explicit needs must be in an earlier stage
    environment: production  # protected -> requires approval
    only: [main, release-*]  # ref-aware DAG: glob-matched against the ref
    variables: {REGION: ap-south-1}
    script: [echo deploying]
```

`only`/`except` are glob patterns matched against the pipeline ref at compile
time, so the same YAML yields different DAGs for dev branches vs main —
excluded jobs never enter the pipeline. Implicit needs skip over stages left
empty for a ref; an explicit `needs` on a ref-excluded job is a compile error.

`retry: N` (0..10, job-level overrides `default.retry`) requeues a failed job
for another attempt instead of failing the pipeline. Cancellations, approval
rejections, and **timeout kills (exit 124) are not retried** — a job that keeps
hitting its `timeout` fails without consuming retries. Logs from every attempt
are preserved with an `attempt N/M` separator.

`auto_cancel` (default `true`) is GitLab-style redundant-pipeline cancellation:
creating a new pipeline for the same repo **and ref** (via API or webhook)
cancels older non-terminal pipelines for that same repo+ref. Different refs are
never affected. Set `auto_cancel: false` to let redundant pipelines run.

### Caching (GitLab-style)

Jobs can restore a cache before the script and save it after, sharing warmed
dependency directories **across pipelines** for the same repo:

```yaml
jobs:
  build:
    stage: build
    script: [go build ./...]
    cache:
      key:                     # literal string, OR content-addressed:
        files: [go.sum, go.mod]  # hash of these files' contents -> the key
        prefix: v1               # optional prefix on the hashed key
      paths: [vendor/, .cache/go-build]  # workspace dirs to cache
      policy: pull-push        # pull-push (default) | pull | push
```

- **`key`** — a literal string (`key: v1-deps`), or a `{files: [...], prefix}`
  map. With `files:`, the runner hashes the listed files' contents (after
  checkout) into the key, so a **changed lockfile misses cleanly** and an
  unchanged one hits. The restore falls back from the exact hashed key to the
  bare prefix, so a first build can still warm from a previous cache.
- **`paths`** — workspace paths tar'd into the cache. Required when caching.
- **`policy`** — `pull-push` (restore before, save after — default), `pull`
  (restore only), or `push` (save only).

Cache is **shared across pipelines** for the same `repo`+`key` (unlike
artifacts, which are per-job and per-pipeline). A cache miss or a cache-store
failure **never fails the job** — it logs and continues. See
[docs/pipeline-dsl.md](docs/pipeline-dsl.md#5-cache--gitlab-style-caching) for
scoping and limitations.

### Advanced authoring: rules, include, extends, parallel/matrix

On top of the core DSL above, Forge supports GitLab-style authoring power:

- **`rules:`** — per-job ordered list; first match wins; controls inclusion and
  `when` (`on_success` | `manual` | `never` | `always`) with a safe expression
  language for `if:` (`==`, `!=`, `=~`, `!~`, `&&`, `||`, `!`, `null`). Supersedes
  `only`/`except` for any job that declares it.
- **`include:`** — compose a config from reusable per-repo **templates**
  (`include: [{template: name}]`), registered via `PUT /api/v1/repo-templates`.
- **`extends:`** — inherit from one or more base jobs (hidden `.name` template
  jobs are never emitted); deep-merged, child overrides parent.
- **`parallel: N`** / **`parallel: {matrix: […]}`** — expand a job into N
  instances or one job per variable combination, with matrix `needs` fan-in.

See **[docs/pipeline-dsl.md](docs/pipeline-dsl.md)** for the full reference,
examples, and the honest `changes:` / `exists:` / remote-include limitations.

## Quickstart (local dev)

Requires Go 1.24+, Node 22+, Docker (for Postgres).

```sh
make setup      # postgres container + go deps + npm install
make server     # control plane on :8080
make runner     # a shell-executor runner (separate terminal)
make web        # vite dev server on :5173 (separate terminal)
make demo       # trigger examples/demo-pipeline.yml
```

Open http://localhost:5173 — the demo pipeline runs build → test, then blocks
on `deploy-prod` until you approve it in the UI.

Full containerized stack instead: `make up` (dashboard on :3000).

## API

Public:

- `POST /api/v1/pipelines` `{repo, ref, sha, config}` — compile YAML (for that ref) and enqueue
- `GET  /api/v1/repos` — per-repo rollup cards (counts, refs, recent statuses, last pipeline)
- `GET  /api/v1/pipelines[?repo=name][&limit=&offset=]` — newest first; array body plus
  `X-Total-Count` / `X-Has-More` headers. `limit` defaults to 50, capped at 200.
- `GET  /api/v1/pipelines/{id}` — includes per-stage statuses and jobs
- `POST /api/v1/pipelines/{id}/cancel` — cancel the whole pipeline (all non-terminal jobs); idempotent
- `POST /api/v1/jobs/{id}/cancel` — cancel a single job (created/pending/blocked → canceled; running → stopped via heartbeat); idempotent
- `POST /api/v1/jobs/{id}/play` — release a gated `when: manual` job (blocked → created); see [docs/pipeline-dsl.md](docs/pipeline-dsl.md)
- `GET  /api/v1/jobs/{id}/logs` — full log as `text/plain` (masked; back-compat), or
  `?offset=N` → JSON `{bytes, next_offset, eof}` for incremental polling
- `GET  /api/v1/jobs/{id}/logs/stream[?offset=N]` — SSE live tail (`text/event-stream`);
  `log` events then a final `eof` event on terminal state
- `POST /api/v1/jobs/{id}/approvals` `{approver, verdict: approved|rejected, comment}`
- `PUT  /api/v1/repo-templates` `{repo, name, yaml}` — register a reusable fragment for `include:` (admin)
- `GET  /api/v1/repo-templates?repo=name` — list a repo's registered template names
- `GET  /api/v1/metrics` — Prometheus-format log-tier counters (auth-exempt)

Cancel and other mutating routes use the same authz as the rest of the API:
open in bootstrap mode, admin session required once SSO is enforced.

Runner protocol:

- `POST /api/v1/runner/acquire` — long-poll, atomic claim via `SKIP LOCKED`
- `POST /api/v1/runner/jobs/{id}/logs` — text chunks (masked across chunk boundaries)
- `POST /api/v1/runner/jobs/{id}/heartbeat` — response `{cancel: bool}`; `true` tells the runner to stop the job
- `POST /api/v1/runner/jobs/{id}/complete` `{status, exit_code}` — status `success | failed | canceled | requeue`

Webhooks are deduplicated by delivery id (GitHub `X-GitHub-Delivery`, Bitbucket
`X-Request-UUID`, else a body hash): a redelivered event returns `200` without
creating a second pipeline.

## Security & operations

Hardening is env-driven on **forge-server** (all optional; defaults preserve
the dev experience):

| Env var | Purpose | Default |
|---|---|---|
| `RUNNER_AUTH` | `off` \| `on` — require a bearer token on the runner protocol + artifact upload | `off` |
| `RUNNER_TOKEN` | (runner) token sent to an `RUNNER_AUTH=on` server | — |
| `FORGE_SECRET_KEY` | base64 32-byte AES-256-GCM key; encrypts variables, VCS tokens and SSO secrets at rest | — (passthrough) |
| `ADMIN_EMAILS` | comma-separated platform-admin emails (enforced once SSO is on) | — |
| `EXTERNAL_URL` | server's public origin (SSO redirect + CSRF allow-list; commit-status `target_url` fallback) | `http://localhost:8080` |
| `FRONTEND_URL` | dashboard origin (post-login redirect + CSRF allow-list; commit-status `target_url` base) | `http://localhost:5173` |
| `COMMIT_STATUS` | `on` \| `off` — write pipeline status back to the origin VCS (GitHub commit status / Bitbucket build status). Only acts on connected repos authenticated by a token (or a GitHub App) with commit-status scope; `off` disables globally | `on` |
| `GITHUB_API_BASE` | override the GitHub API host — used for **commit-status write-back** and **GitHub App installation-token minting**. For testing against a stub; leave unset in production | `https://api.github.com` |
| `REDIS_URL` | Redis for the high-volume log tier (live buffer + pub/sub fan-out), e.g. `redis://localhost:6379/0` | — |
| `LOG_BACKEND` | `redis` \| `postgres`; empty auto-selects redis when `REDIS_URL` is set+reachable, else postgres | — (auto) |
| `MAX_JOB_LOG_BYTES` | per-job cumulative log cap; excess truncated with a notice | `10485760` (10 MiB) |
| `MAX_ARTIFACT_BYTES` | per-upload artifact cap (`413` + cleanup on overflow); `0` disables | `524288000` (500 MiB) |
| `MAX_CACHE_BYTES` | per-save cache cap (`413` + cleanup on overflow; runner skips + continues); `0` disables | `524288000` (500 MiB) |
| `RETENTION_DAYS` | delete pipelines, artifact blobs, cache blobs and webhook-dedup rows older than this; `0` = keep forever | `30` |
| `RUNNER_DRAIN_GRACE` | (runner) on SIGINT/SIGTERM, how long to let in-flight jobs finish before requeuing them | `30s` |

- **Runner auth** — see [docs/runners.md](docs/runners.md). In `on` mode with no
  tokens, the server auto-generates and logs a bootstrap token. Manage tokens
  via `POST/GET /api/v1/runner-tokens` and `.../{id}/revoke`.
- **Admin authorization & CSRF** — see [docs/sso.md](docs/sso.md). Mutating
  admin endpoints require a platform admin; cookie-authenticated mutations must
  be same-origin.
- **Secrets at rest** — set `FORGE_SECRET_KEY` to encrypt `repo_variables.value`,
  `repo_registry.token` and `sso_providers.client_secret` with envelope
  encryption (`enc:v1:` prefix). Pre-existing plaintext still reads, and any
  plaintext rows are re-encrypted on startup once a key is present. Without a key
  the server runs in plaintext passthrough and logs a loud warning if secrets
  exist. Generate a key with `head -c 32 /dev/urandom | base64`.

## Docs

- [VCS integration (GitHub/Bitbucket webhooks)](docs/vcs-integration.md)
- [Registering runners (VM/systemd, Docker, Kubernetes)](docs/runners.md)
- [Kubernetes executor: deployment, access & RBAC (incl. separate-cluster)](docs/kubernetes-deployment.md)
- [Artifact storage (local, MinIO, S3, GCS)](docs/artifact-storage.md)
- [High-volume log architecture (Redis live buffer, blob archive, SSE)](docs/log-ingestion-design.md)
- [Roles, membership & approval rules](docs/rbac-approvals.md)
- [SSO setup: Google, Microsoft, GitHub](docs/sso.md)
- [Cloud deployment (Helm chart + Terraform/OpenTofu for AWS & GCP)](docs/cloud-deployment.md)
- [Feature comparison vs GitLab CI + roadmap](docs/feature-comparison.md)

## What's here beyond the core

- **CI/CD variables** per repo — protected (only on protected refs), masked
  (redacted in job logs at ingestion), environment-scoped.
- **Runner registry** — self-registration, online/offline, tag-based job
  routing (`tags:` on jobs), pause, and graceful drain (SIGTERM finishes
  in-flight jobs within `RUNNER_DRAIN_GRACE`, then requeues the rest).
- **Cancellation & retries** — cancel a job or whole pipeline at any state
  (running jobs stopped via the heartbeat channel); per-job `retry:` and
  redundant-pipeline `auto_cancel`.
- **Artifacts** — `artifacts.paths` archived per job; local disk or any
  S3-compatible store (S3/MinIO/GCS).
- **Cache** — `cache.{key,paths,policy}` restored before / saved after the
  script, **shared across pipelines** per repo+key; literal or content-addressed
  (`key.files`) keys, capped by `MAX_CACHE_BYTES`, age-GC'd by `RETENTION_DAYS`.
  Never fails a job.
- **RBAC approvals** — repo members (admin/owner/developer), per-repo
  protected-environment rules (required approvals, allowed roles,
  self-approval block, timeout), append-only audit trail.
- **GitHub/Bitbucket webhooks** — push → pipeline, with per-repo registered
  configs and HMAC verification (GitHub).
- **Commit status write-back** — pipeline status is posted back to the origin
  VCS (GitHub commit status / Bitbucket build status, context `forge-ci`,
  `target_url` → the pipeline page) as it progresses, so the commit/PR shows
  Forge's ✓/✗. Idempotent and delivered asynchronously with bounded retries;
  needs a connection token with commit-status scope, and is toggled by
  `COMMIT_STATUS`. See [docs/vcs-integration.md](docs/vcs-integration.md#commit-status-write-back).

## Known limitations (see docs/feature-comparison.md for the full roadmap)

- Identity comes from SSO sessions; with SSO in open mode there is no authz
  (bootstrap). Enable a provider and set `ADMIN_EMAILS` before real use.
- Logs use a swappable backend (`LOG_BACKEND`): **redis** keeps each running
  job's log in a capped Redis buffer with pub/sub live-tail, then archives the
  finished log to the blob store as one object (Postgres holds only a pointer);
  **postgres** (dev/no-Redis) keeps bodies in `job_logs`. See
  [docs/log-ingestion-design.md](docs/log-ingestion-design.md). Masked values are
  redacted per chunk, across chunk boundaries (a carry-over tail per job), and
  re-masked on full-text reads as a backstop.
- Secret encryption uses a single `FORGE_SECRET_KEY` (no per-key rotation or
  external KMS/Vault yet); rotating the key requires re-encrypting rows.
- No caching, `rules:`, includes, matrix, retries, or scheduled pipelines yet.
