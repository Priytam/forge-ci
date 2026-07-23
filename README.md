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
stages: [build, test, deploy]
jobs:
  build-app:
    stage: build
    image: alpine:3          # used by the docker executor
    script:
      - echo "compiling..."
  unit-tests:
    stage: test              # no explicit needs -> depends on previous stage
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
- `GET  /api/v1/pipelines[?repo=name]` / `GET /api/v1/pipelines/{id}` — both include per-stage statuses
- `GET  /api/v1/jobs/{id}/logs` (plain text)
- `POST /api/v1/jobs/{id}/approvals` `{approver, verdict: approved|rejected, comment}`

Runner protocol:

- `POST /api/v1/runner/acquire` — long-poll, atomic claim via `SKIP LOCKED`
- `POST /api/v1/runner/jobs/{id}/logs` — text chunks
- `POST /api/v1/runner/jobs/{id}/heartbeat`
- `POST /api/v1/runner/jobs/{id}/complete` `{status, exit_code}`

## Security & operations

Hardening is env-driven on **forge-server** (all optional; defaults preserve
the dev experience):

| Env var | Purpose | Default |
|---|---|---|
| `RUNNER_AUTH` | `off` \| `on` — require a bearer token on the runner protocol + artifact upload | `off` |
| `RUNNER_TOKEN` | (runner) token sent to an `RUNNER_AUTH=on` server | — |
| `FORGE_SECRET_KEY` | base64 32-byte AES-256-GCM key; encrypts variables, VCS tokens and SSO secrets at rest | — (passthrough) |
| `ADMIN_EMAILS` | comma-separated platform-admin emails (enforced once SSO is on) | — |
| `EXTERNAL_URL` | server's public origin (SSO redirect + CSRF allow-list) | `http://localhost:8080` |
| `FRONTEND_URL` | dashboard origin (post-login redirect + CSRF allow-list) | `http://localhost:5173` |
| `MAX_JOB_LOG_BYTES` | per-job cumulative log cap; excess truncated with a notice | `10485760` (10 MiB) |
| `MAX_ARTIFACT_BYTES` | per-upload artifact cap (`413` + cleanup on overflow); `0` disables | `524288000` (500 MiB) |
| `RETENTION_DAYS` | delete pipelines + artifact blobs older than this; `0` = keep forever | `30` |

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
- [Roles, membership & approval rules](docs/rbac-approvals.md)
- [SSO setup: Google, Microsoft, GitHub](docs/sso.md)
- [Feature comparison vs GitLab CI + roadmap](docs/feature-comparison.md)

## What's here beyond the core

- **CI/CD variables** per repo — protected (only on protected refs), masked
  (redacted in job logs at ingestion), environment-scoped.
- **Runner registry** — self-registration, online/offline, tag-based job
  routing (`tags:` on jobs), pause/drain.
- **Artifacts** — `artifacts.paths` archived per job; local disk or any
  S3-compatible store (S3/MinIO/GCS).
- **RBAC approvals** — repo members (admin/owner/developer), per-repo
  protected-environment rules (required approvals, allowed roles,
  self-approval block, timeout), append-only audit trail.
- **GitHub/Bitbucket webhooks** — push → pipeline, with per-repo registered
  configs and HMAC verification (GitHub).

## Known limitations (see docs/feature-comparison.md for the full roadmap)

- Identity comes from SSO sessions; with SSO in open mode there is no authz
  (bootstrap). Enable a provider and set `ADMIN_EMAILS` before real use.
- Logs live in Postgres; masked values split across log chunks can escape
  redaction.
- Secret encryption uses a single `FORGE_SECRET_KEY` (no per-key rotation or
  external KMS/Vault yet); rotating the key requires re-encrypting rows.
- No caching, `rules:`, includes, matrix, retries, or scheduled pipelines yet.
