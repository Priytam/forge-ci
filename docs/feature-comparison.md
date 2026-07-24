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
| Ref-conditional jobs (`only/except`, `rules:`) | ✅ rules engine | 🟡 `only/except` globs | `rules: if/changes/exists` engine next |
| `variables` at job level | ✅ | ✅ | |
| Artifacts (`paths`) | ✅ + expiry, reports | 🟡 paths only | add `expire_in`, junit test reports |
| Caching (keyed, per-lockfile) | ✅ | ❌ | biggest perceived-speed feature — high priority |
| Templates: `include`, `extends`, anchors | ✅ | ❌ | needed for org-wide standards |
| Matrix builds (`parallel:`) | ✅ | ❌ | |
| Manual jobs (`when: manual`) | ✅ | 🟡 | approvals cover the gated case; plain manual next |
| Timeouts (per job + pipeline default) | ✅ | ✅ | runner group-kill + server backstop + queue timeout |
| Retry policy | ✅ | ✅ | `retry: N` (0..10) + `default.retry`, attempt tracking; timeouts/cancels not retried |
| Child/multi-project pipelines, triggers | ✅ | ❌ | |
| Scheduled pipelines (cron) | ✅ | ❌ | easy: scheduler already ticks |

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
| Services (sidecar containers, e.g. postgres for tests) | ✅ | ❌ | |
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
| AuthN (SSO/OIDC) + real RBAC identity | ✅ | ❌ | identity is client-asserted today — REQUIRED before real use |
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
| Environments/deployments board | ✅ | ❌ | see ArgoCD section |

## ArgoCD-inspired direction (CD depth)

ArgoCD's value is the **environment lens**: what's deployed where, is it
healthy, what's the diff. Forge already has the two primitives that matter —
`environment:` on jobs and the approval gate. Roadmap to an ArgoCD-flavored CD
view in GitLab's visual language:

1. **Environments board** — one card per environment per repo: currently
   deployed SHA (last successful `environment:` job), who approved, when,
   history of deployments with one-click rollback (re-run old pipeline's
   deploy job). *(Mostly derivable from existing data.)*
2. **Deployment freezes** — freeze windows per environment (GitLab has this;
   maps cleanly onto the scheduler's blocked logic).
3. **Sync/health status** — ArgoCD's killer feature needs a k8s connection:
   after a deploy job, poll the cluster (Deployment rollout status) and show
   Healthy/Progressing/Degraded on the environment card.
4. **Drift awareness** — "deployed SHA ≠ latest main" indicator per env
   (pure DB query), the lightweight cousin of ArgoCD's OutOfSync.

## Suggested build order (impact-ranked)

1. OIDC authn + map RBAC to real identities (unblocks everything trust-related)
2. ~~Git clone step + commit-status write-back~~ ✅ shipped (makes it a *real* CI for GitHub/Bitbucket)
3. Caching + k8s executor (speed and scale)
4. Environments board with deploy history/rollback (ArgoCD lens, GitLab skin)
5. `rules:`/`include`/matrix (authoring power) · WebSocket logs · scheduled pipelines
