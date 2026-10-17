# Forge CI vs GitLab CI — feature comparison & roadmap

Positioning: Forge is a **standalone CI/CD engine** (like Buildkite or Drone),
not a VCS. It integrates with GitHub/Bitbucket via webhooks (and AWS CodeCommit
via EventBridge — see [codecommit.md](codecommit.md)) rather than owning
the repo, keeps GitLab's UX vocabulary (pipelines → stages → jobs, pill graph,
settings sections), and aims to absorb ArgoCD-style environment/deployment
views over time.

Legend: ✅ shipped · 🟡 partial · ❌ not yet (roadmap).

## Pipeline authoring

| Feature | GitLab CI | Forge | Notes / roadmap |
|---|---|---|---|
| YAML pipeline (stages, jobs, script, image) | ✅ | ✅ | |
| DAG via `needs` | ✅ | ✅ | explicit must point to earlier stage |
| Ref-conditional jobs (`only/except`, `rules:`) | ✅ rules engine | ✅ `only/except` globs + `rules:` engine | `rules: if/when/changes` with a safe expr evaluator; `changes:` compares against the last green pipeline on the branch (or the merge base); `exists:` is a documented no-op — see docs/pipeline-dsl.md |
| `variables` at job level | ✅ | ✅ | |
| Artifacts (`paths`) | ✅ + expiry, reports | ✅ `paths` + `expire_in` + `reports.junit` | per-artifact TTL (server-side GC, independent of `RETENTION_DAYS`); JUnit XML parsed into a per-job pass/fail summary at `GET /jobs/{id}/report` — see docs/pipeline-dsl.md |
| Caching (keyed, per-lockfile) | ✅ | ✅ | `cache: {key, paths, policy}`, literal or content-addressed `key.files`, shared across pipelines — see docs/pipeline-dsl.md |
| Templates: `include`, `extends`, anchors | ✅ | ✅ `include` from repo templates, the versioned template catalog (`name@version`), another repo (`project:`/`ref:`/`file:`, any git host) and `remote:` https URLs (SSRF-guarded, `integrity:` pin), with typed `spec: inputs:`; `extends` | YAML anchors work as plain YAML — see docs/pipeline-dsl.md |
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
| Secrets storage | encrypted at rest, Vault integration | ✅ (envelope AES-GCM) | FORGE_SECRET_KEY envelope-encrypts variables/tokens/SSO/App keys at rest (`enc:v1:`); key rotation still roadmap |
| External secret managers (`secrets:`) | ✅ Vault, AWS, GCP, Azure (Premium) | ✅ | runner fetches with the job's OIDC identity: Vault KV v2, AWS Secrets Manager, GCP Secret Manager, Azure Key Vault; masked; `file:` for key material — see [secrets.md](secrets.md) |
| Registry login without stored passwords | ✅ (own registry) | ✅ | `registry_login:` mints ECR / Artifact Registry / ACR logins from the job identity; `DOCKER_AUTH_CONFIG` for any other registry; used to pull the job image and inside the job |
| Artifact/cache/log storage backends | object storage | ✅ | local, any S3-compatible (AWS, GCS, MinIO, Ceph, R2), Azure Blob — see [artifact-storage.md](artifact-storage.md) |
| Notifications | Slack, email, webhooks, Teams (integrations) | ✅ | pluggable notifiers: Slack, Microsoft Teams, signed JSON webhook, email; routes per event/repo; approve/reject from the email with signed links — see [notifications.md](notifications.md) |
| Protected environments + approvers | ✅ (Premium) | ✅ | roles: admin/owner/developer |
| Separation of duties (no self-approval) | ✅ | ✅ | pinned to `triggered_by` |
| Approval audit trail | ✅ | ✅ | append-only `job_approvals` |
| Built-in security scan templates (SAST/dependency/container/secrets) | ✅ (CI templates) | ✅ | one-line `include: [{template: security/sast}]` — semgrep/trivy/gitleaks, `allow_failure` by default; built-ins resolve as a fallback after per-repo templates — see docs/pipeline-dsl.md |
| AuthN (SSO/OIDC) + real RBAC identity | ✅ | ✅ | any OpenID Connect IdP (Okta, Keycloak, Auth0, Entra, Authentik, Dex…), LDAP/AD, Google/Microsoft/GitHub — see docs/sso.md |
| IdP group → role sync | ✅ (SAML/OIDC group sync, Premium) | ✅ | groups refreshed each sign-in; platform admin or roles on repo patterns; used for approvals too |
| SAML SSO | ✅ | 🟡 | via an OIDC bridge today; native SP designed in docs/design/saml.md |
| Configurable session lifetime | ✅ | ✅ | 1h–30d, admin setting or SESSION_TTL |
| OIDC / keyless cloud auth (AWS STS, GCP WIF) | ✅ (ID tokens) | ✅ | short-lived per-job ID token (`FORGE_OIDC_TOKEN`/`CI_JOB_JWT`), public discovery+JWKS, no static cloud keys — see [oidc.md](oidc.md) |
| Audit log (all setting changes) | ✅ | ✅ | append-only `audit_log` of every mutating admin action + denials; `GET /api/v1/audit-log` — see docs/sso.md |

## Integration (standalone-CI specific)

| Feature | GitLab (owns repo) | Forge | Notes |
|---|---|---|---|
| Push webhooks GitHub/Bitbucket | n/a (native) | ✅ | HMAC verified (GitHub) |
| AWS CodeCommit as a source provider | ❌ | ✅ | EventBridge push/PR triggers, `.forge-ci.yml` via GetFile at the event sha, IAM SigV4 clone keyed on per-job OIDC (no static credentials anywhere) — see [codecommit.md](codecommit.md) |
| Pipeline config source | in-repo `.gitlab-ci.yml` | ✅ | in-repo `.forge-ci.yml` fetched via the provider contents API at the event sha (config rides in the commit/PR), with automatic fall-back to the registered config; per-repo `config_source` toggle (`repo` default / `registered`) — see [vcs-integration.md](vcs-integration.md#config-from-the-repo-forge-ciyml) |
| Commit status write-back (✓/✗ on commit, PR checks) | native | ✅ | GitHub commit-status API + Bitbucket build status, context `forge-ci`, `target_url` → pipeline page; async idempotent posting (needs a token with commit-status scope) — see [vcs-integration.md](vcs-integration.md#commit-status-write-back). **CodeCommit has no status API at all**: its runs report as a pull-request comment instead, and push runs report nothing — see [codecommit.md](codecommit.md) |
| PR/MR-triggered pipelines | ✅ | ✅ | GitHub `pull_request` (opened/synchronize/reopened) + Bitbucket `pullrequest:created`/`:updated` build the PR HEAD sha with `CI_PIPELINE_SOURCE=merge_request` and `CI_MERGE_REQUEST_*` context; commit status lands on the PR head, and a new push/sync auto-cancels the older PR pipeline — see [vcs-integration.md](vcs-integration.md#prmr-triggered-pipelines) |

## UI (GitLab-themed)

| Feature | GitLab | Forge | Notes |
|---|---|---|---|
| Repo overview cards (multi-repo home) | ~ (groups) | ✅ | Forge-specific, since it's cross-VCS |
| Pipeline list + per-stage mini circles | ✅ | ✅ | |
| Pipeline graph: stage & job-dependency grouping, pill cards, curved edges | ✅ | ✅ | |
| Live log viewer | ✅ | ✅ | |
| Settings → CI/CD sections (variables, runners, artifacts, members) | ✅ | ✅ | |
| Runner autoscaling | ✅ (docker-machine / k8s executor / fleeting) | ✅ | autoscaled pools with pluggable providers: Kubernetes Deployment, Docker host, exec (any cloud via CLI/Terraform); queue-driven, drain-aware scale-down — see [autoscaling.md](autoscaling.md); or KEDA on the queue metrics |
| Environments/deployments board | ✅ | ✅ | board + history + drift + rollback + freezes, deploy URL/version/digest per deployment ([docs/environments.md](environments.md)) |
| Promote a build between environments | 🟡 (manual pattern) | ✅ | one click/API call redeploys the source deployment's exact commit, artifacts and digest; only the target's deploy jobs run; gates apply ([environments.md](environments.md#promotion--ship-the-build-you-tested)) |
| Prevent outdated deployments | ✅ | ✅ | per environment `cancel_superseded` |
| `artifacts:reports:dotenv` | ✅ | ✅ | variables flow to needing jobs; `DEPLOY_*` fill the deployment record |
| Test coverage (regex + Cobertura) | ✅ | ✅ | per job, pipeline mean, repo trend on the Tests page |

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
