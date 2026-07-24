# Environments board (ArgoCD-style CD lens)

Forge records **deployments** and exposes a per-repo **environments board** with
deployment history, drift detection, one-click rollback, and deploy freezes.
It reuses the two CD primitives Forge already had — `environment:` on jobs and
the protected-environment approval gate — and adds a deployment ledger on top.

The API is shipped and verified; the web view is a later frontend pass.

## What counts as a deployment

A **deployment** is recorded when a job that declares an `environment:` reaches
**success**. Example config:

```yaml
stages: [deploy]
jobs:
  deploy-prod:
    stage: deploy
    environment: production   # <- makes this a deployment target
    script:
      - ./deploy.sh
```

When `deploy-prod` succeeds, a row is inserted into `deployments`
(`repo, environment, sha, ref, pipeline_id, job_id, deployed_by, deployed_at,
status=success`).

### Where it's recorded, and why

Recording happens **in `store.CompleteJob`, inside the same transaction as the
job's terminal status flip** (`internal/store/store.go`) — not in a scheduler
sweep. This was a deliberate choice:

- **Transactional / crash-safe.** The deployment row and the job's `success`
  state commit together. There is no window where a job is `success` but the
  deployment is missing (or vice-versa), and a crash between the two is
  impossible — a rolled-back transaction leaves neither.
- **Idempotent.** `deployments.job_id` is `UNIQUE` and the insert is
  `ON CONFLICT (job_id) DO NOTHING`, so a job's success is recorded **at most
  once**. `CompleteJob` is itself idempotent (a late/duplicate runner report
  finds the job no longer `running` and returns `ErrNotFound`), which is a second
  layer of protection.

A scheduler sweep (à la `postStatuses`) would have needed its own dedup table
and an eventual-consistency lag; the in-transaction hook is simpler and stronger.

### `deployed_by`

`deployed_by` is the environment **approver** when the deploy job was
approval-gated (the latest `approved` verdict in `job_approvals`), otherwise the
pipeline's `triggered_by`. So a protected-env deployment is attributed to who
approved it, and an un-gated one to who triggered it.

## Board API

### `GET /api/v1/environments?repo=<repo>`

One entry per environment the repo has deployed to:

```json
[
  {
    "repo": "acme/app",
    "environment": "production",
    "current": {
      "id": 42, "sha": "dcf2ede…", "ref": "main",
      "pipeline_id": 2, "job_id": 2,
      "deployed_by": "carol@acme.com",
      "deployed_at": "2026-07-24T10:10:49Z", "status": "success"
    },
    "deployment_count": 2,
    "drift": "in_sync",
    "ref_tip_sha": "dcf2ede…",
    "frozen": false
  }
]
```

- `current` — the latest **successful** deployment to that environment.
- `deployment_count` — total successful deployments to that environment.
- `drift` — `in_sync` (deployed sha == ref tip), `drifted` (deployed sha != ref
  tip), or `unknown`. Drift is computed by resolving the current tip of the
  deployment's `ref` via `git ls-remote` on the registered clone URL
  (`store.ResolveRef`). For **unconnected** repos (nothing to resolve against)
  drift is `unknown`. `ref_tip_sha` is the resolved tip (omitted when unknown).
- `frozen` — whether an active deploy-freeze window currently covers the env.

### `GET /api/v1/environments/{repo}/{env}/deployments?limit=&offset=`

Deployment history for one environment, **newest first**, paginated. Total in
`X-Total-Count`, more-pages hint in `X-Has-More` (same convention as
`GET /api/v1/pipelines`).

> Repo names contain slashes (`owner/repo`). These sub-resource routes are served
> from a subtree handler that parses `<repo…>/<env>/<action>` from the path, so
> `Priytam/statemachine` works as the `{repo}` segment.

## Rollback

### `POST /api/v1/environments/{repo}/{env}/rollback`

Body: `{"to_pipeline_id": <id>}` **or** `{"to_sha": "<sha>"}` (pipeline id wins
when both are given). The target must be a **prior deployment** of this
repo+environment, so its `ref` is known.

Rollback does **not** flip a pointer or bypass anything. It:

1. Resolves the target `(sha, ref)` from the deployment ledger.
2. Loads the repo's **registered config** and re-compiles it at the target
   `ref`.
3. Creates a **new pipeline** at the target `sha` via the normal
   `store.CreatePipeline` path.

Because it's a normal pipeline, it goes through the **normal flow — including the
environment's approval gate**. Rolling back to a **protected** environment
(e.g. `production`) creates a pipeline whose deploy job is `blocked` awaiting
approval, exactly like any other deploy. **A rollback to prod still requires
approval.** Only after approval does it run, succeed, record a new deployment,
and flip the board's `current` back to the rolled-back SHA.

Rollback is **admin-gated** (`requireAdmin`) and **audited** (action
`environment.rollback`, with `environment`, `target`, `target_sha`, `target_ref`
and `new_pipeline_id` in the audit detail). It returns the new pipeline.

Requires a registered config (`PUT /api/v1/repo-configs`) — the rollback
re-runs the deploy from that config at the old SHA. A repo with no registered
config gets a `400`.

## Deploy freezes

Freeze windows hold deployments to an environment for a period.

- `POST /api/v1/deploy-freezes` `{repo, environment, starts_at, ends_at, reason}`
  (admin, audited). `repo: ""` makes the freeze **global**.
- `GET /api/v1/deploy-freezes[?repo=<repo>]` — list (repo-specific + global).
- `DELETE /api/v1/deploy-freezes/{id}` (admin, audited).

**Enforcement** lives in `store.PromoteReadyJobs`: a job targeting an
environment inside an active freeze window is **left in `created`** — it is not
promoted to `pending` (or to the approval `blocked` state). When the window
passes, a later scheduler tick promotes it normally. This is self-releasing,
crash-safe, and needs no new job state: a frozen deploy simply waits its turn.

The board reports an environment's active-freeze status via the `frozen` field.

## Interaction with retention

`deployments` rows reference `pipelines(id)` / `jobs(id)` with
`ON DELETE CASCADE`, so the existing retention sweep (`RETENTION_DAYS`) prunes
deployment history along with the pipelines it belongs to — no separate GC.

## Schema

See `internal/store/migrations.sql`:

- `deployments (id, repo, environment, sha, ref, pipeline_id, job_id,
  deployed_by, deployed_at, status, UNIQUE(job_id))`, indexed on
  `(repo, environment, id DESC)`.
- `deploy_freezes (id, repo, environment, starts_at, ends_at, reason,
  created_at)`.
