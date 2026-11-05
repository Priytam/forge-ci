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

### URL, version and digest

A deployment also records **where** it is reachable and **what** was deployed,
whatever the target — a Kubernetes cluster, a VM fleet, a serverless function,
a static site. Forge doesn't need to know; the job says:

```yaml
jobs:
  build:
    stage: build
    script:
      - docker buildx build --push -t "$REGISTRY/shop:$CI_COMMIT_SHORT_SHA" --metadata-file meta.json .
      - echo "DEPLOY_VERSION=1.4.$CI_PIPELINE_ID" >> build.env
      - echo "DEPLOY_DIGEST=$(jq -r '."containerimage.digest"' meta.json)" >> build.env
    artifacts:
      reports:
        dotenv: build.env          # KEY=VALUE, passed to the jobs that need this one
  deploy-staging:
    stage: deploy
    needs: [build]
    environment:
      name: staging
      url: https://staging.$APP_DOMAIN   # $VARS expanded when the deployment is recorded
    script:
      - ./deploy.sh "$REGISTRY/shop@$DEPLOY_DIGEST"   # deploy by digest, not by tag
```

| Field | From (first that is set) |
|---|---|
| `url` | the job's `DEPLOY_URL` (or `CI_ENVIRONMENT_URL`) dotenv variable, then `environment: url:` with variables expanded. Only `http(s)://` links are kept. |
| `version` | `DEPLOY_VERSION`, else the short commit sha |
| `digest` | `DEPLOY_DIGEST` — an image digest, artifact checksum, package version id… anything immutable |

`DEPLOY_*` can come from the deploy job's own dotenv or be inherited from a
job it needs (the build above). The environments page shows the version, a
link to the URL, and the digest on each card and in the history.

**Dotenv rules:** `KEY=VALUE` lines (`export` and quotes allowed, `#`
comments), at most 100 variables of 4 KiB each; `CI_*` and `FORGE_*` are
reserved and refused so a job can't spoof Forge's own variables. Variables
flow to every job that `needs:` the reporting job (oldest first; a later
job's value wins), before repo variables and the job's own `variables:`.

## Promotion — ship the build you tested

`POST /api/v1/environments/{repo}/{env}/promote` with
`{"from_environment": "staging"}` (its current deployment) or
`{"deployment_id": 123}` redeploys an earlier deployment's **build** to `env`
— the **Promote →** button on each environment card does this. Forge:

1. takes the source deployment's pipeline — its exact config, commit and ref;
2. compiles it and keeps **only the jobs that deploy to `env`**;
3. points their `needs:` on anything else (build, image push, tests) at the
   source pipeline's **successful jobs**, so they download the same artifacts
   and inherit the same dotenv — `DEPLOY_VERSION`, `DEPLOY_DIGEST`, image
   references. Nothing is rebuilt.

The promotion is an ordinary pipeline (`source: promotion`,
`CI_PIPELINE_SOURCE=promotion` at run time) and goes through everything a
deploy does: manual gates, approvals, branch rules, freezes, resource groups.
Its jobs also see `CI_PROMOTION=true`, `CI_PROMOTED_FROM_ENVIRONMENT`,
`CI_PROMOTED_FROM_DEPLOYMENT_ID` and `CI_PROMOTED_FROM_PIPELINE_ID`. Rules are
evaluated as they were for the source pipeline, so a deploy job that existed
then exists now. The resulting deployment records `promoted_from`, and
promoting again (staging → production → dr) still reuses the **original**
build.

Errors are explicit: promoting to the same environment (use rollback), a
config with no job for the target, or a deploy job that needs a job which
didn't succeed in the source pipeline. Developers (`operate` on the repo) may
promote; protected environments still need their approvals.

## Cancelling superseded deploys

Tick **Cancel superseded** on an environment (Admin → Environments &
branches) and a deploy to it that is still **waiting** — queued, awaiting
approval, behind a freeze or a resource group — is canceled once a deploy from
a **newer pipeline** of the same repo has started or succeeded there. The job
shows `superseded: pipeline #N has a newer deploy to production`, and its
dependents are canceled with it. A deploy that is already running is never
touched. Off by default; the starter registry turns it on for production.

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

## Org-wide views (many repos × dev / staging / prod)

- **Environments** (`/environments`, `GET /api/v1/environment-board`): one
  row per repo, one column per environment in promotion order (dev → staging
  → prod; unknown names after, alphabetically). Each cell shows the live
  deployment and, when the newest attempt differs, a badge: running, approval,
  manual, failed or canceled; plus `frozen` during a deploy freeze. Filters:
  `?q` (repo name), `?env` (only that column and repos active in it),
  `?attention=1` (latest attempt failed, waiting or canceled — in `env` when
  set). 50 repos per page; latest attempts look back 30 days.
- **Jobs** (`/jobs`, `GET /api/v1/jobs`): every job across pipelines, newest
  first, filtered on the server by `repo`, `ref`, `status`, `runner`,
  `environment` and `name` (contains). Quick presets: Active, Waiting, Failed.
- **Environment tabs** on the Pipelines, Jobs and Environments pages, and a
  **List & filter** view inside a pipeline (`?group=list`) for pipelines with
  many jobs.

## Environments & branches (admin, per server)

**Admin ▾ → Environments & branches** (`GET`/`PUT /api/v1/environment-registry`)
defines this server's real environments. Each company running Forge sets up its
own; nothing is shared between servers.

| Field | Meaning |
|---|---|
| name | The real name, used everywhere in the UI (`production`) |
| order | Column/tab order (promotion path: dev → staging → production) |
| tier | `dev`, `staging`, `prod` or `other` — colour and meaning |
| aliases | Other names pipelines may write for it (`prod`, `prd`) |
| branches allowed | Glob patterns (`main`, `release/*`; `*` matches anything including `/`). Empty = any branch |
| unlisted names | `allow` (use as written) or `deny` (job fails — catches typos like `prdo`) |

How it is applied:

- **New pipelines** (API, webhook, schedule, retry): each job's `environment:`
  is mapped to the real name. If the pipeline's branch is not allowed for that
  environment, the job is created as **failed without running**, with the
  reason on the job page, the job lists and the board ("refused"). Dependents
  then don't run either. Pipeline files cannot override this.
- **Saving** renames aliases already stored in jobs, deployments, approval
  rules, freezes and scoped variables to the real name, in one transaction. If a
  repo has rows under both an alias and the real name (e.g. approval rules for
  `prod` and `production`), the save is refused with the repo named; remove one
  and save again.
- Admin forms (approval rules, freezes, variable scopes) accept aliases and
  store the real name.
- **No environments listed = no policy**: names are used as written and any
  branch may deploy anywhere — exactly the behaviour before this feature.

### First-time setup checklist (each company)

1. Admin ▾ → Environments & branches → *Start from dev / staging / production*.
2. Adjust names to what your teams call them; put every spelling in use
   under aliases (check the tabs on the Pipelines page for stray names).
3. Set allowed branches — typically `production` and `staging`:
   `main, release/*`; `dev`: empty (any branch).
4. Save. Fix any conflict it reports, then save again.
5. Once pipelines use registered names, switch *unlisted environments* to
   **Refuse** to catch typos.
6. Add approval rules for production (Repo → Settings) if not already set.
