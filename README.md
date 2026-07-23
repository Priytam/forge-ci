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

## Docs

- [VCS integration (GitHub/Bitbucket webhooks)](docs/vcs-integration.md)
- [Registering runners (VM/systemd, Docker, Kubernetes)](docs/runners.md)
- [Artifact storage (local, MinIO, S3, GCS)](docs/artifact-storage.md)
- [Roles, membership & approval rules](docs/rbac-approvals.md)
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

- No authn — RBAC is enforced server-side but identity is client-asserted;
  front with OIDC before real use.
- No git clone step yet: scripts operate on an empty workspace.
- Logs live in Postgres; masked values split across log chunks can escape
  redaction.
- Variable values are plaintext in the DB (envelope encryption/Vault next).
- No caching, `rules:`, includes, matrix, retries, or scheduled pipelines yet.
