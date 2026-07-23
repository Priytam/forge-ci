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
  deploy-prod:
    stage: deploy
    needs: [unit-tests]      # explicit needs must be in an earlier stage
    environment: production  # protected -> requires approval
    variables: {REGION: ap-south-1}
    script: [echo deploying]
```

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

- `POST /api/v1/pipelines` `{repo, ref, sha, config}` — compile YAML and enqueue
- `GET  /api/v1/pipelines` / `GET /api/v1/pipelines/{id}`
- `GET  /api/v1/jobs/{id}/logs` (plain text)
- `POST /api/v1/jobs/{id}/approvals` `{approver, verdict: approved|rejected, comment}`

Runner protocol:

- `POST /api/v1/runner/acquire` — long-poll, atomic claim via `SKIP LOCKED`
- `POST /api/v1/runner/jobs/{id}/logs` — text chunks
- `POST /api/v1/runner/jobs/{id}/heartbeat`
- `POST /api/v1/runner/jobs/{id}/complete` `{status, exit_code}`

## Deliberate MVP simplifications

- No authn/authz — put OIDC + RBAC in front before real use; approver identity
  is currently client-asserted.
- Logs live in Postgres — move to object storage with a Redis live-tail.
- No git clone step, artifacts, or caches yet — the executor interface and
  schema are where they'd attach.
- Single approval count per environment (no approver allowlists / self-approval
  block yet; the `job_approvals` schema already supports adding them).
- No retries, `rules:`, matrix builds, or includes in the DSL.
