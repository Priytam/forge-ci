# Forge CI vs GitLab CI — feature comparison & roadmap

Positioning: Forge is a **standalone CI/CD engine** (like Buildkite or Drone),
not a VCS. It integrates with GitHub/Bitbucket via webhooks rather than owning
the repo, keeps GitLab's UX vocabulary (pipelines → stages → jobs, pill graph,
settings sections), and aims to absorb ArgoCD-style environment/deployment
views over time.

Legend: ✅ shipped · 🟡 partial · ❌ not yet (roadmap).

## Pipeline authoring

| Feature | GitLab CI | Forge | Notes / roadmap |
|---|---|---|---|
| YAML pipeline (stages, jobs, script, image) | ✅ | ✅ | |
| DAG via `needs` | ✅ | ✅ | explicit must point to earlier stage |
| Ref-conditional jobs (`only/except`, `rules:`) | ✅ rules engine | ✅ `only/except` globs + `rules:` engine | `rules: if/when` with a safe expr evaluator; `changes/exists` are documented no-ops (no checkout at compile time) — see docs/pipeline-dsl.md |
| `variables` at job level | ✅ | ✅ | |
| Artifacts (`paths`) | ✅ + expiry, reports | 🟡 paths only | add `expire_in`, junit test reports |
| Caching (keyed, per-lockfile) | ✅ | ❌ | biggest perceived-speed feature — high priority |
| Templates: `include`, `extends`, anchors | ✅ | 🟡 `include` (per-repo templates) + `extends` | anchors N/A; remote/URL includes out of scope (no network fetch) — see docs/pipeline-dsl.md |
| Matrix builds (`parallel:`) | ✅ | ✅ | `parallel: N` and `parallel: {matrix}` with `needs` fan-in |
| Manual jobs (`when: manual`) | ✅ | ✅ | `rules`/job `when: manual` → gated `blocked`; `POST /jobs/{id}/play` releases; composes with env approval |
| Allowed failures (`allow_failure`) | ✅ | ✅ | job-level or per-rule; dependents proceed, pipeline not failed |
| Timeouts (per job + pipeline default) | ✅ | ✅ | runner group-kill + server backstop + queue timeout |
| Retry policy | ✅ | ✅ | `retry: N` (0..10) + `default.retry`, attempt tracking; timeouts/cancels not retried |
| Child/multi-project pipelines, triggers | ✅ | ❌ | |
| Scheduled pipelines (cron) | ✅ | ✅ | 5-field cron (UTC); replica-safe compare-and-set claim, catch-up = fire once then advance; `CI_PIPELINE_SOURCE == "schedule"` — see docs/schedules.md |

## Execution

| Feature | GitLab CI | Forge | Notes |
|---|---|---|---|
| Pull-based runners, self-registration | ✅ | ✅ | no inbound ports |
| Executors | shell, docker, k8s, custom, VM autoscaling | ✅ shell, docker, kubernetes (ephemeral pod per job) | VM autoscaling not planned |
| Artifact passing to dependent jobs | ✅ | ✅ | restored from `needs` before script |
| Runner concurrency (one manager, N jobs) | ✅ | ✅ | `--concurrency` |
| Tag-based routing | ✅ | ✅ | |
| Pause/drain runners | ✅ | ✅ | |
| Job logs: live streaming | ✅ | ✅ | SSE live tail + Redis buffer + object-storage archive |
| Services (sidecar containers, e.g. postgres for tests) | ✅ | ✅ | docker (per-job network) + kubernetes (pod containers + hostAliases); shell rejects; up to 5/job |
| Git clone of the source into the job | ✅ (owns repo) | ✅ | shallow clone at the pipeline SHA via the registry token |
| Interruptible/auto-cancel superseded pipelines | ✅ | ✅ | `auto_cancel` (default true), per repo+ref |

## Security & governance

| Feature | GitLab CI | Forge | Notes |
|---|---|---|---|
| CI variables: protected / masked / env-scoped | ✅ | ✅ | same three axes |
| Secrets storage | encrypted at rest, Vault integration | 🟡 plaintext DB | envelope-encrypt, then Vault |
| Protected environments + approvers | ✅ (Premium) | ✅ | roles: admin/owner/developer |
| Separation of duties (no self-approval) | ✅ | ✅ | pinned to `triggered_by` |
| Approval audit trail | ✅ | ✅ | append-only `job_approvals` |
| Built-in security scan templates (SAST/dependency/container/secrets) | ✅ (CI templates) | ✅ | one-line `include: [{template: security/sast}]` — semgrep/trivy/gitleaks, `allow_failure` by default; built-ins resolve as a fallback after per-repo templates — see docs/pipeline-dsl.md |
| AuthN (SSO/OIDC) + real RBAC identity | ✅ | ✅ | SSO (Google/Microsoft/GitHub) enforces session identity; RBAC maps to it — see docs/sso.md |
| OIDC / keyless cloud auth (AWS STS, GCP WIF) | ✅ (ID tokens) | ✅ | short-lived per-job ID token (`FORGE_OIDC_TOKEN`/`CI_JOB_JWT`), public discovery+JWKS, no static cloud keys — see [oidc.md](oidc.md) |
| Audit log (all setting changes) | ✅ | ❌ | |

## Integration (standalone-CI specific)

| Feature | GitLab (owns repo) | Forge | Notes |
|---|---|---|---|
| Push webhooks GitHub/Bitbucket | n/a (native) | ✅ | HMAC verified (GitHub) |
| Pipeline config source | in-repo `.gitlab-ci.yml` | 🟡 registered per repo | fetch `.forge-ci.yml` from GitHub API at push (needs token) |
| Commit status write-back (✓/✗ on commit, PR checks) | native | ✅ | GitHub commit-status API + Bitbucket build status, context `forge-ci`, `target_url` → pipeline page; async idempotent posting (needs a token with commit-status scope) — see [vcs-integration.md](vcs-integration.md#commit-status-write-back) |
| PR/MR-triggered pipelines | ✅ | ❌ | webhook already receives PR events; needs ref semantics |

## UI (GitLab-themed)

| Feature | GitLab | Forge | Notes |
|---|---|---|---|
| Repo overview cards (multi-repo home) | ~ (groups) | ✅ | Forge-specific, since it's cross-VCS |
| Pipeline list + per-stage mini circles | ✅ | ✅ | |
| Pipeline graph: stage & job-dependency grouping, pill cards, curved edges | ✅ | ✅ | |
| Live log viewer | ✅ | ✅ | |
| Settings → CI/CD sections (variables, runners, artifacts, members) | ✅ | ✅ | |
| Environments/deployments board | ✅ | ✅ | board + history + drift + rollback + freezes — API shipped ([docs/environments.md](environments.md)); web view is a later pass |

## ArgoCD-inspired direction (CD depth)

ArgoCD's value is the **environment lens**: what's deployed where, is it
healthy, what's the diff. Forge already had the two primitives that matter —
`environment:` on jobs and the approval gate — and now records deployments and
serves the board on top of them (see [docs/environments.md](environments.md)):

1. **Environments board** — ✅ shipped. One card per environment per repo:
   currently deployed SHA (last successful `environment:` job), who deployed
   (env approver, else pipeline trigger), when, deployment count, and full
   history with one-click rollback. Deployments are recorded transactionally in
   `CompleteJob` when an `environment:` job flips to success (deduped by job_id).
2. **Deployment freezes** — ✅ shipped. Per-env (or global) freeze windows; a
   deploy job targeting a frozen env is held in `created` by the scheduler until
   the window passes (self-releasing), rather than running mid-freeze.
3. **Sync/health status** — ❌ not yet. ArgoCD's killer feature needs a k8s
   connection: after a deploy job, poll the cluster (Deployment rollout status)
   and show Healthy/Progressing/Degraded on the environment card.
4. **Drift awareness** — ✅ shipped. Per-env `drift` = deployed SHA vs the live
   tip of its ref (via `git ls-remote` on the registered clone URL);
   `unknown` for unconnected repos. The lightweight cousin of ArgoCD's OutOfSync.

## Suggested build order (impact-ranked)

1. ~~OIDC authn + map RBAC to real identities~~ ✅ shipped (SSO — docs/sso.md) · ~~OIDC/keyless cloud auth (AWS STS / GCP WIF)~~ ✅ shipped (docs/oidc.md)
2. ~~Git clone step + commit-status write-back~~ ✅ shipped (makes it a *real* CI for GitHub/Bitbucket)
3. Caching + k8s executor (speed and scale)
4. ~~Environments board with deploy history/rollback~~ ✅ shipped (ArgoCD lens, GitLab skin — see docs/environments.md)
5. ~~`rules:`/`include`/matrix (authoring power)~~ ✅ shipped (see docs/pipeline-dsl.md) · ~~scheduled pipelines~~ ✅ shipped (see docs/schedules.md) · WebSocket logs
