-- Forge CI schema. Idempotent: safe to run at every server start.

CREATE TABLE IF NOT EXISTS pipelines (
    id          BIGSERIAL PRIMARY KEY,
    repo        TEXT        NOT NULL,
    ref         TEXT        NOT NULL,
    sha         TEXT        NOT NULL,
    config_yaml TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS jobs (
    id           BIGSERIAL PRIMARY KEY,
    pipeline_id  BIGINT      NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    stage        TEXT        NOT NULL,
    stage_idx    INT         NOT NULL,
    image        TEXT,
    script       TEXT        NOT NULL,
    env          JSONB       NOT NULL DEFAULT '{}',
    environment  TEXT,
    status       TEXT        NOT NULL DEFAULT 'created'
        CHECK (status IN ('created','blocked','pending','running','success','failed','canceled')),
    blocked_at   TIMESTAMPTZ,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    runner_id    TEXT,
    exit_code    INT,
    UNIQUE (pipeline_id, name)
);
CREATE INDEX IF NOT EXISTS jobs_status_idx ON jobs (status);
CREATE INDEX IF NOT EXISTS jobs_pipeline_idx ON jobs (pipeline_id);

CREATE TABLE IF NOT EXISTS job_needs (
    job_id       BIGINT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    needs_job_id BIGINT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    PRIMARY KEY (job_id, needs_job_id)
);

CREATE TABLE IF NOT EXISTS job_logs (
    id         BIGSERIAL PRIMARY KEY,
    job_id     BIGINT      NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    chunk      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS job_logs_job_idx ON job_logs (job_id, id);

-- Server-side approval policy: lives outside pipeline YAML so an MR editing
-- CI config cannot weaken the gate. repo='' is the global default; a row
-- with a specific repo overrides it for that repo.
CREATE TABLE IF NOT EXISTS protected_environments (
    id                     BIGSERIAL PRIMARY KEY,
    name                   TEXT NOT NULL,
    required_approvals     INT  NOT NULL DEFAULT 1,
    approval_timeout_hours INT  NOT NULL DEFAULT 24
);
ALTER TABLE protected_environments ADD COLUMN IF NOT EXISTS repo TEXT NOT NULL DEFAULT '';
ALTER TABLE protected_environments
    ADD COLUMN IF NOT EXISTS approver_roles TEXT[] NOT NULL DEFAULT '{admin,owner}';
ALTER TABLE protected_environments
    ADD COLUMN IF NOT EXISTS allow_self_approval BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE protected_environments DROP CONSTRAINT IF EXISTS protected_environments_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS protected_env_repo_name_idx
    ON protected_environments (repo, name);
-- platform_settings is defined here as well as below (same idempotent
-- definition) so this seed can honour an admin having deleted the default
-- rule: deleting it records 'default_production_rule_removed'.
CREATE TABLE IF NOT EXISTS platform_settings (
    key        TEXT        PRIMARY KEY,
    value      JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO protected_environments (repo, name, required_approvals)
SELECT '', 'production', 1
WHERE NOT EXISTS (SELECT 1 FROM protected_environments WHERE repo='' AND name='production')
  AND NOT EXISTS (SELECT 1 FROM platform_settings WHERE key='default_production_rule_removed');

-- Repo membership: who is what on a repo. Enforced at the approval endpoint.
-- When SSO is enforced the approver identity comes from the session (see
-- docs/sso.md); in open bootstrap mode it is client-asserted.
CREATE TABLE IF NOT EXISTS repo_members (
    id       BIGSERIAL PRIMARY KEY,
    repo     TEXT NOT NULL,
    username TEXT NOT NULL,
    role     TEXT NOT NULL CHECK (role IN ('admin','owner','developer')),
    UNIQUE (repo, username)
);

ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS triggered_by TEXT NOT NULL DEFAULT '';
-- Trigger source: api | push | webhook | merge_request | schedule.
ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'api';

-- Registered pipeline YAML per repo (Forge doesn't host the repo; webhooks
-- carry only repo/ref/sha, the config lives here).
CREATE TABLE IF NOT EXISTS repo_configs (
    repo        TEXT PRIMARY KEY,
    config_yaml TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- SSO provider configuration, editable in-product (Admin -> SSO). Secrets are
-- write-only through the API. While NO provider is enabled the API runs in
-- open bootstrap mode; enabling any provider turns on session enforcement.
CREATE TABLE IF NOT EXISTS sso_providers (
    provider       TEXT PRIMARY KEY CHECK (provider IN ('google','microsoft','github','oidc','ldap')),
    enabled        BOOLEAN     NOT NULL DEFAULT FALSE,
    client_id      TEXT        NOT NULL DEFAULT '',
    client_secret  TEXT        NOT NULL DEFAULT '',
    tenant         TEXT        NOT NULL DEFAULT 'common',  -- microsoft only
    allowed_domain TEXT        NOT NULL DEFAULT '',        -- e.g. meesho.com
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Server-side login sessions (cookie carries only the random token).
CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT PRIMARY KEY,
    email      TEXT        NOT NULL,
    name       TEXT        NOT NULL DEFAULT '',
    provider   TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions (expires_at);

-- Append-only pipeline-config history. Every save is a new version; revert
-- copies an old version forward as a new one. History is never rewritten, so
-- any change can be traced and undone. repo_configs stays the "current"
-- pointer used by webhooks.
CREATE TABLE IF NOT EXISTS repo_config_versions (
    id          BIGSERIAL PRIMARY KEY,
    repo        TEXT        NOT NULL,
    version     INT         NOT NULL,
    config_yaml TEXT        NOT NULL,
    author      TEXT        NOT NULL DEFAULT '',
    message     TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo, version)
);
-- Seed v1 from any config saved before versioning existed.
INSERT INTO repo_config_versions (repo, version, config_yaml, author, message)
SELECT rc.repo, 1, rc.config_yaml, '', 'pre-versioning config'
FROM repo_configs rc
WHERE NOT EXISTS (SELECT 1 FROM repo_config_versions v WHERE v.repo = rc.repo);

-- Which registered config version a pipeline ran (NULL = one-off custom
-- config supplied at run time; never persisted to the registry).
ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS config_version INT;

-- config-from-repo: which source this pipeline's config_yaml came from.
--   'registered' = a registered config version (stamped in config_version), or a
--                  one-off custom config supplied to the manual API.
--   'repo'       = the in-repo .forge-ci.yml fetched at the pipeline sha
--                  (config_version is NULL — it is not a registry version).
-- config_yaml always stores the exact YAML that was compiled, so retries/audit
-- see precisely what ran. Defaults to 'registered' so existing rows and the
-- manual one-off path are unaffected.
ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS config_source TEXT NOT NULL DEFAULT 'registered';

-- fail_fast (top-level `fail_fast: true` in the pipeline YAML; default FALSE,
-- so existing pipelines are unaffected and GitLab-compatible). When TRUE, the
-- scheduler's fail_fast_cancel transition cancels the pipeline's other
-- non-terminal jobs as soon as any job reaches a genuine (non-allow_failure,
-- retries-exhausted) 'failed' state — stopping in-flight and not-yet-started
-- siblings, not just downstream dependents.
ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS fail_fast BOOLEAN NOT NULL DEFAULT FALSE;

-- Pull-request identity for a merge_request pipeline. Needed because CodeCommit
-- has NO commit-status API: Forge reports a CodeCommit result by commenting on
-- the originating pull request, and PostCommentForPullRequest requires the pull
-- request id plus the before/after commits — none of which are recoverable from
-- (repo, ref, sha) after the fact. mr_iid is TEXT because CodeCommit pull
-- request ids are strings. mr_base_sha is the destination-branch commit the PR
-- targets ("before"); the pipeline's own sha is the "after".
-- Both stay '' for push pipelines and for providers that report via a real
-- status API, and an empty mr_iid is exactly what marks a run as "nothing to
-- comment on".
-- What the run is, and who wrote it — as distinct from who STARTED it
-- (triggered_by). The two differ exactly where it matters: a merge, a rebase, a
-- bot push, or a re-run of an old commit all have an actor who is not the
-- author. Recorded at ingest from the provider event, because Forge does not
-- host the repo and cannot look a commit up later.
--   commit_author  who wrote the code; '' when the event does not say
--                  (CodeCommit's EventBridge payload carries no author).
--   commit_message what the run is about: the commit message for a push, the
--                  pull-request title for a PR run. Capped at ingest — the UI
--                  shows the subject line, and an unbounded body has no reader.
ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS commit_author  TEXT NOT NULL DEFAULT '';
ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS commit_message TEXT NOT NULL DEFAULT '';

ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS mr_iid      TEXT NOT NULL DEFAULT '';
ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS mr_base_sha TEXT NOT NULL DEFAULT '';

-- First-class repo registry: the connection to the real VCS repo. token is
-- used to build authenticated clone URLs for runners (never returned by the
-- API, never logged). Plaintext at rest for now — same caveat as variables.
CREATE TABLE IF NOT EXISTS repo_registry (
    repo           TEXT PRIMARY KEY,
    provider       TEXT NOT NULL CHECK (provider IN ('github','bitbucket','codecommit','other')),
    clone_url      TEXT NOT NULL,
    token          TEXT NOT NULL DEFAULT '',
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- GitHub App authentication (alternative to the static `token` PAT above). A
-- connection authenticates via EITHER a static token OR a GitHub App; when the
-- App columns are set they take precedence and Forge mints a short-lived
-- installation access token on demand (see internal/githubapp). The columns
-- live on repo_registry so resolution stays a single primary-key lookup and the
-- PAT path is unchanged (these default to '' and are simply ignored). The
-- private key is a secret encrypted at rest exactly like `token`
-- (enc:v1: prefix, re-encrypted by MigrateSecrets on start); it is write-only
-- over the API and never returned. app_id and installation_id are not secret.
ALTER TABLE repo_registry ADD COLUMN IF NOT EXISTS github_app_id              TEXT NOT NULL DEFAULT '';
ALTER TABLE repo_registry ADD COLUMN IF NOT EXISTS github_app_private_key     TEXT NOT NULL DEFAULT '';
ALTER TABLE repo_registry ADD COLUMN IF NOT EXISTS github_app_installation_id TEXT NOT NULL DEFAULT '';

-- AWS CodeCommit connections. CodeCommit has no token: every call (GetFile for
-- config-from-repo, PostCommentForPullRequest for status, and the runner's git
-- clone) is SigV4-signed with an AWS identity, so NOTHING SECRET is stored for a
-- CodeCommit repo — these three columns are all non-secret and are returned by
-- the API, unlike token / github_app_private_key.
--   aws_region   the repository's region; also encoded in clone_url, which stays
--                the fall-back source for connections registered by URL alone.
--   aws_profile  optional named profile for the CONTROL PLANE's shared AWS config.
--   aws_role_arn optional role the control plane assumes before calling
--                CodeCommit, so one Forge deployment can serve repositories in
--                several accounts. The RUNNER's role is separate and keyless
--                (per-job OIDC -> AssumeRoleWithWebIdentity); see docs/codecommit.md.
-- The provider CHECK is widened rather than replaced: DROP IF EXISTS then ADD
-- keeps the migration idempotent on re-run, and existing rows are unaffected
-- because the new value only widens the allowed set.
ALTER TABLE repo_registry DROP CONSTRAINT IF EXISTS repo_registry_provider_check;
ALTER TABLE repo_registry ADD CONSTRAINT repo_registry_provider_check
    CHECK (provider IN ('github','gitlab','bitbucket','gitea','codecommit','git','other'));
ALTER TABLE repo_registry ADD COLUMN IF NOT EXISTS aws_region   TEXT NOT NULL DEFAULT '';
ALTER TABLE repo_registry ADD COLUMN IF NOT EXISTS aws_profile  TEXT NOT NULL DEFAULT '';
ALTER TABLE repo_registry ADD COLUMN IF NOT EXISTS aws_role_arn TEXT NOT NULL DEFAULT '';

-- config-from-repo: where a repo's pipeline config comes from.
--   'repo'       = prefer the in-repo .forge-ci.yml fetched (via the provider
--                  contents API, at the event sha, with the connection's
--                  token/App) and fall back to the registered config when the
--                  file is absent.
--   'registered' = only ever use the registered config (the repo file is never
--                  fetched, even if it exists).
-- Default 'repo' so pipeline config rides in the commit/PR (the DX win); a repo
-- with no in-repo file transparently falls back to its registered config, so
-- existing connections keep working. config_path overrides the fetched path
-- ('' = the default .forge-ci.yml).
ALTER TABLE repo_registry ADD COLUMN IF NOT EXISTS config_source TEXT NOT NULL DEFAULT 'repo'
    CHECK (config_source IN ('repo','registered'));
ALTER TABLE repo_registry ADD COLUMN IF NOT EXISTS config_path   TEXT NOT NULL DEFAULT '';

-- Per-repo defaults. Runners are deployed independently (global fleet,
-- selected by tags); a repo picks its runner group here. Jobs without an
-- explicit tags: list inherit these.
CREATE TABLE IF NOT EXISTS repo_settings (
    repo                TEXT PRIMARY KEY,
    default_runner_tags TEXT[] NOT NULL DEFAULT '{}',
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Job routing tags and artifact declarations.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
-- Per-job execution timeout; 0 = use the server default.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS timeout_seconds INT NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS artifact_paths JSONB NOT NULL DEFAULT '[]';
-- Per-job cache directives (GitLab-style cache: block). cache_key is the literal
-- key or the prefix; cache_key_files are hashed by the runner into a
-- content-addressed key; cache_policy is pull|push|pull-push ('' = no cache).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cache_paths     JSONB NOT NULL DEFAULT '[]';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cache_key       TEXT  NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cache_key_files JSONB NOT NULL DEFAULT '[]';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cache_policy    TEXT  NOT NULL DEFAULT '';
-- Per-job sidecar service containers (GitLab-style services: list). Each entry
-- is {image, alias, env, cmd}; the runner starts them alongside the job and the
-- script reaches them by alias. '[]' = no services. docker/kubernetes only.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS services        JSONB NOT NULL DEFAULT '[]';

-- Container network for a docker job. '' means the executor default, which is
-- BRIDGE: a job that cannot resolve DNS cannot install dependencies, fetch rule
-- sets or download a vulnerability database, so an isolated default silently
-- broke every network-dependent tool (including the shipped security/* scans).
-- 'none' is the explicit opt-out for jobs that genuinely want no egress.
-- A job declaring services is unaffected: it always gets its own per-job
-- network (forge-net-<id>) so it can resolve the service aliases.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS network         TEXT NOT NULL DEFAULT '';

-- Runner registry: runners self-register on their first acquire and update
-- last_contact_at on every poll. Paused runners receive no jobs.
CREATE TABLE IF NOT EXISTS runners (
    id              TEXT PRIMARY KEY,
    executor        TEXT        NOT NULL DEFAULT '',
    tags            TEXT[]      NOT NULL DEFAULT '{}',
    description     TEXT        NOT NULL DEFAULT '',
    paused          BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_contact_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Artifact archives uploaded by runners after successful jobs. Blob lives on
-- the server filesystem (object storage in production); this is metadata.
CREATE TABLE IF NOT EXISTS artifacts (
    id         BIGSERIAL PRIMARY KEY,
    job_id     BIGINT      NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    size_bytes BIGINT      NOT NULL,
    path       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS artifacts_job_idx ON artifacts (job_id);

-- Per-artifact expiry (from artifacts.expire_in). The server stamps this at
-- upload as now() + jobs.artifact_expire_seconds when that is > 0; NULL means no
-- explicit expiry (the artifact relies on the RETENTION_DAYS sweep). The GC
-- sweep deletes rows whose expires_at < now() INDEPENDENTLY of RETENTION_DAYS,
-- so a short expire_in expires promptly even when retention is large or off.
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS artifacts_expires_idx ON artifacts (expires_at)
    WHERE expires_at IS NOT NULL;

-- artifact_expire_seconds carries a job's artifacts.expire_in to the upload
-- handler; report_junit carries artifacts.reports.junit globs to the runner.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS artifact_expire_seconds INT   NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS report_junit            JSONB NOT NULL DEFAULT '[]';

-- Per-job test report parsed server-side from uploaded JUnit XML (one row per
-- job; a re-run's re-upload replaces it). failures is a JSON array of
-- {name, classname, type, message} for the failed/errored cases.
CREATE TABLE IF NOT EXISTS job_reports (
    job_id           BIGINT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
    total            INT              NOT NULL,
    passed           INT              NOT NULL,
    failed           INT              NOT NULL,
    skipped          INT              NOT NULL,
    duration_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
    failures         JSONB            NOT NULL DEFAULT '[]',
    created_at       TIMESTAMPTZ      NOT NULL DEFAULT now()
);

-- Per-test-case history: one row per (job, test case), populated by the same
-- report_junit upload that writes job_reports (see internal/api/junit.go). This
-- is append-only ACROSS jobs — unlike job_reports, which is an aggregate
-- overwritten on re-upload — so a case's pass/fail trend can be read back
-- across every run Forge has seen (internal/store ListTestCaseHistory). A
-- re-upload of the SAME job's report still replaces that job's own rows
-- (SaveTestCaseResults deletes job_id's rows before inserting), matching
-- job_reports' re-upload semantics without duplicating history.
CREATE TABLE IF NOT EXISTS test_case_results (
    id               BIGSERIAL PRIMARY KEY,
    job_id           BIGINT           NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    name             TEXT             NOT NULL,
    classname        TEXT             NOT NULL DEFAULT '',
    status           TEXT             NOT NULL CHECK (status IN ('passed','failed','skipped')),
    duration_seconds DOUBLE PRECISION NOT NULL DEFAULT 0,
    message          TEXT             NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ      NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS test_case_results_job_idx ON test_case_results (job_id);
-- Drives ListTestCaseHistory's per-repo, per-case "most recent runs" query.
CREATE INDEX IF NOT EXISTS test_case_results_name_idx
    ON test_case_results (classname, name, created_at DESC);

-- Cache entries: the runner restores/saves per-job caches through the server.
-- Unlike artifacts (per-job, per-pipeline), a cache is SHARED across pipelines
-- for the same repo+key, so it survives the retention sweep of the pipeline that
-- last wrote it. blob_key points at the tar.gz in the blob store; (repo, key) is
-- unique so a save overwrites the previous cache for that key. updated_at drives
-- age-based retention GC (independent of pipelines).
CREATE TABLE IF NOT EXISTS cache_entries (
    id         BIGSERIAL   PRIMARY KEY,
    repo       TEXT        NOT NULL,
    cache_key  TEXT        NOT NULL,
    blob_key   TEXT        NOT NULL,
    size_bytes BIGINT      NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo, cache_key)
);
CREATE INDEX IF NOT EXISTS cache_entries_updated_idx ON cache_entries (updated_at);

-- CI/CD variables, scoped to a repo (GitLab Settings -> CI/CD -> Variables).
-- protected: only injected when the pipeline ref matches a protected ref.
-- masked: value is redacted from job logs at ingestion.
-- environment_scope: '*' (all) or a glob matched against the job's environment.
CREATE TABLE IF NOT EXISTS repo_variables (
    id                BIGSERIAL PRIMARY KEY,
    repo              TEXT        NOT NULL,
    key               TEXT        NOT NULL,
    value             TEXT        NOT NULL,
    protected         BOOLEAN     NOT NULL DEFAULT FALSE,
    masked            BOOLEAN     NOT NULL DEFAULT FALSE,
    environment_scope TEXT        NOT NULL DEFAULT '*',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo, key, environment_scope)
);

-- Ref patterns considered "protected" (for protected variables).
-- repo = '' applies to every repo.
CREATE TABLE IF NOT EXISTS protected_refs (
    id      BIGSERIAL PRIMARY KEY,
    repo    TEXT NOT NULL DEFAULT '',
    pattern TEXT NOT NULL,
    UNIQUE (repo, pattern)
);
INSERT INTO protected_refs (repo, pattern) VALUES ('', 'main')
ON CONFLICT DO NOTHING;

-- Runner authentication tokens. Runners present `Authorization: Bearer <token>`
-- on every /runner/* call (and artifact upload) when RUNNER_AUTH=on. token is
-- the raw secret (PK); the API never returns it after creation — lists show
-- the last 4 chars only. id gives a stable handle for revoke-by-id.
CREATE TABLE IF NOT EXISTS runner_tokens (
    id           BIGSERIAL   UNIQUE,
    token        TEXT        PRIMARY KEY,
    description  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked      BOOLEAN     NOT NULL DEFAULT FALSE
);

-- Per-job log-byte cap bookkeeping: once a job's cumulative log bytes exceed
-- MAX_JOB_LOG_BYTES the server writes a single truncation notice and sets this
-- flag so further chunks are dropped without re-noticing.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS log_truncated BOOLEAN NOT NULL DEFAULT FALSE;

-- High-volume log archive pointer (LOG_BACKEND=redis). When a job reaches a
-- terminal state its full log is flushed from the Redis live buffer to the blob
-- store as one object; the pointer below records where it landed so finished
-- jobs are read straight from object storage and never accumulate bodies in
-- Postgres. NULL log_object_key means "not archived yet" (still in Redis, or
-- the postgres backend which keeps bodies in job_logs). An empty-string key
-- means "archived, but the job produced no output" (nothing was written to the
-- blob store). log_total_bytes is the archived byte length.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS log_object_key  TEXT;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS log_total_bytes BIGINT NOT NULL DEFAULT 0;
-- Serves the scheduler's "archive terminal-but-unarchived jobs" safety-net scan.
CREATE INDEX IF NOT EXISTS jobs_log_unarchived_idx
    ON jobs (finished_at) WHERE log_object_key IS NULL AND finished_at IS NOT NULL;

-- Append-only audit trail of approval decisions.
CREATE TABLE IF NOT EXISTS job_approvals (
    id         BIGSERIAL PRIMARY KEY,
    job_id     BIGINT      NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    approver   TEXT        NOT NULL,
    verdict    TEXT        NOT NULL CHECK (verdict IN ('approved','rejected')),
    comment    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (job_id, approver)
);

-- Cancellation: a running job cannot be stopped synchronously (the runner owns
-- the process). Setting cancel_requested asks the runner — via the heartbeat
-- response — to kill the job and report status='canceled'. Crash-safe and
-- idempotent: the flag is a durable request, re-read on every heartbeat.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS cancel_requested BOOLEAN NOT NULL DEFAULT FALSE;

-- Job retries: attempt is the current (1-based) try; max_attempts = retry+1.
-- On a non-timeout, non-cancel failure with attempt < max_attempts the job is
-- requeued (pending) for another try instead of failing the pipeline.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS attempt      INT NOT NULL DEFAULT 1;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS max_attempts INT NOT NULL DEFAULT 1;

-- Manual gate (rules when: manual, or job-level when: manual). A manual job is
-- created in the 'blocked' state and does not run until it is explicitly played
-- (POST /api/v1/jobs/{id}/play), which returns it to 'created' so the normal
-- scheduler flow (needs / protected-environment) applies. It is distinct from
-- the environment-approval 'blocked' state: the manual flag lets the scheduler
-- and the approval endpoint tell the two apart.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS manual BOOLEAN NOT NULL DEFAULT FALSE;

-- allow_failure: when TRUE, this job failing does NOT block its dependents and
-- is not counted as a pipeline failure for status derivation (GitLab semantics).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS allow_failure BOOLEAN NOT NULL DEFAULT FALSE;

-- Reusable pipeline fragments referenced by top-level include: [{template: N}].
-- Forge hosts no repo file tree and webhooks carry no file contents, so
-- includes resolve against these per-repo registered templates by name rather
-- than a filesystem path. repo scopes the template; name is unique per repo.
CREATE TABLE IF NOT EXISTS repo_templates (
    repo       TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    yaml       TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (repo, name)
);

-- Webhook delivery dedup: providers redeliver the same event (retries, manual
-- redelivery). Recording each delivery id makes pipeline creation idempotent
-- per (provider, delivery_id). Old rows are GC'd by the retention sweep.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    provider    TEXT        NOT NULL,
    delivery_id TEXT        NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, delivery_id)
);
CREATE INDEX IF NOT EXISTS webhook_deliveries_received_idx ON webhook_deliveries (received_at);

-- Pagination / list-scan support: the pipelines list is filtered by repo and
-- ordered newest-first (id DESC). This composite index serves both the
-- all-repos and repo-filtered list queries without a full scan.
CREATE INDEX IF NOT EXISTS pipelines_repo_id_idx ON pipelines (repo, id DESC);
CREATE INDEX IF NOT EXISTS pipelines_created_idx ON pipelines (created_at);

-- Commit-status write-back dedup: one row per (pipeline, posted status). The
-- scheduler posts the pipeline's status back to the origin VCS (GitHub commit
-- status / Bitbucket build status) as it progresses; recording each posted
-- (pipeline_id, status) makes posting idempotent so the 1s scheduler tick never
-- re-posts the same state. A row is a durable claim: it is inserted BEFORE the
-- HTTP call so concurrent ticks/instances can't double-post, and deleted again
-- only when a transient delivery failure should be retried on a later tick.
-- Cascades away with the pipeline (retention GC).
CREATE TABLE IF NOT EXISTS pipeline_status_posts (
    pipeline_id BIGINT      NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    status      TEXT        NOT NULL,
    posted_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (pipeline_id, status)
);

-- Append-only audit trail of mutating admin/settings actions (and denied
-- attempts). Written after a successful mutation, and on a 401/403 for an admin
-- route. NEVER stores secret values, tokens or client secrets — detail carries
-- only safe metadata. Rows are only ever INSERTed and SELECTed in normal paths;
-- the retention sweep (RETENTION_DAYS) is the sole path that deletes, pruning
-- rows older than the window. actor is the authenticated session email when SSO
-- is enforced, else a fixed label ('bootstrap'/'webhook'/'runner'). result is
-- 'ok' | 'denied' | 'error'. repo is duplicated out of detail for cheap
-- filtering on GET /api/v1/audit-log?repo=.
CREATE TABLE IF NOT EXISTS audit_log (
    id         BIGSERIAL   PRIMARY KEY,
    ts         TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor      TEXT        NOT NULL DEFAULT '',
    action     TEXT        NOT NULL,
    target     TEXT        NOT NULL DEFAULT '',
    repo       TEXT        NOT NULL DEFAULT '',
    detail     JSONB       NOT NULL DEFAULT '{}',
    source_ip  TEXT        NOT NULL DEFAULT '',
    result     TEXT        NOT NULL DEFAULT 'ok'
);
-- Newest-first listing, optionally filtered by repo or actor.
CREATE INDEX IF NOT EXISTS audit_log_id_idx    ON audit_log (id DESC);
CREATE INDEX IF NOT EXISTS audit_log_ts_idx    ON audit_log (ts);
CREATE INDEX IF NOT EXISTS audit_log_repo_idx  ON audit_log (repo);
CREATE INDEX IF NOT EXISTS audit_log_actor_idx ON audit_log (actor);

-- ArgoCD-style deployment history. One row is recorded — transactionally, in
-- CompleteJob, at the moment an environment-targeting job flips to 'success' —
-- so the deployment record and the job's terminal state commit together
-- (crash-safe). UNIQUE(job_id) makes recording idempotent: a duplicate/late
-- runner report (or a re-run of CompleteJob) inserts nothing (ON CONFLICT DO
-- NOTHING), so a given job's success is deployed at most once. deployed_by is
-- the environment's approver when the job was approval-gated, else the pipeline's
-- triggered_by. Rows cascade away with the pipeline under the retention sweep.
CREATE TABLE IF NOT EXISTS deployments (
    id          BIGSERIAL   PRIMARY KEY,
    repo        TEXT        NOT NULL,
    environment TEXT        NOT NULL,
    sha         TEXT        NOT NULL,
    ref         TEXT        NOT NULL,
    pipeline_id BIGINT      NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    job_id      BIGINT      NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    deployed_by TEXT        NOT NULL DEFAULT '',
    deployed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status      TEXT        NOT NULL DEFAULT 'success',
    UNIQUE (job_id)
);
-- Serves the board (latest-per-env) and the per-env history listing.
CREATE INDEX IF NOT EXISTS deployments_repo_env_id_idx ON deployments (repo, environment, id DESC);

-- Cron-scheduled pipelines. A schedule fires a pipeline for (repo, ref) on a
-- 5-field cron cadence (evaluated in UTC). The scheduler finds enabled schedules
-- whose next_run_at <= now(), then atomically CLAIMs each with a compare-and-set
-- on next_run_at (advancing it to the next future slot) so two replicas can't
-- double-fire and a missed window fires once rather than storm-firing (catch-up:
-- fire once, advance to the next future slot, no backfill). Schedules are config,
-- not run data — the retention sweep NEVER deletes them (only the pipelines they
-- create are subject to RETENTION_DAYS). next_run_at is NOT NULL: it is computed
-- from the cron expression at create/update time and always points at the next
-- fire time.
CREATE TABLE IF NOT EXISTS pipeline_schedules (
    id          BIGSERIAL   PRIMARY KEY,
    repo        TEXT        NOT NULL,
    ref         TEXT        NOT NULL,
    cron        TEXT        NOT NULL,
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_run_at TIMESTAMPTZ,
    next_run_at TIMESTAMPTZ NOT NULL
);
-- Serves the scheduler's due-schedule scan (enabled, next_run_at <= now).
CREATE INDEX IF NOT EXISTS pipeline_schedules_due_idx
    ON pipeline_schedules (next_run_at) WHERE enabled;

-- OIDC signing key for keyless cloud auth. Forge signs a short-lived ID token
-- per job (see internal/oidc) that jobs exchange for AWS/GCP credentials with no
-- static cloud keys. The RSA signing key must be STABLE across restarts so the
-- published JWKS (and its kid) don't change under a cloud provider's feet. When
-- OIDC_PRIVATE_KEY (PEM) is set in the env it wins and this table is unused;
-- otherwise Forge generates an RSA-2048 key on first start and persists it here,
-- encrypted at rest with FORGE_SECRET_KEY exactly like other secrets (enc:v1:
-- prefix; re-encrypted by MigrateSecrets on start). Only one row is ever kept:
-- the oldest by created_at is the active key (single-key model for v1).
CREATE TABLE IF NOT EXISTS oidc_keys (
    kid                 TEXT        PRIMARY KEY,
    private_key_pem_enc TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Deployment freeze windows. While now() is within [starts_at, ends_at) for a
-- matching (repo, environment) — repo='' is a global freeze — an environment-
-- targeting job is HELD in 'created' by PromoteReadyJobs and not promoted to
-- pending/blocked until the window passes (self-releasing on a later tick).
CREATE TABLE IF NOT EXISTS deploy_freezes (
    id          BIGSERIAL   PRIMARY KEY,
    repo        TEXT        NOT NULL DEFAULT '',
    environment TEXT        NOT NULL,
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ NOT NULL,
    reason      TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deploy_freezes_lookup_idx ON deploy_freezes (environment, repo);

-- Personal API tokens. A token acts on behalf of the user who created it: a
-- request with `Authorization: Bearer fci_…` is treated exactly like that
-- user's SSO session (same identity, same admin/repo rights). Only a SHA-256
-- hash of the token is stored; the raw value is shown once at creation.
-- expires_at NULL means the token never expires (allowed only while the
-- api_token_policy setting permits it — the policy is re-checked on every use,
-- so turning it off disables existing non-expiring tokens immediately).
CREATE TABLE IF NOT EXISTS api_tokens (
    id           BIGSERIAL   PRIMARY KEY,
    token_hash   TEXT        NOT NULL UNIQUE,
    token_suffix TEXT        NOT NULL,
    owner        TEXT        NOT NULL,
    owner_name   TEXT        NOT NULL DEFAULT '',
    description  TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked      BOOLEAN     NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS api_tokens_owner_idx ON api_tokens (lower(owner));

-- Platform-wide settings edited from the admin UI, one JSON value per key
-- (e.g. 'api_token_policy'). A missing key means "use the built-in default".
CREATE TABLE IF NOT EXISTS platform_settings (
    key        TEXT        PRIMARY KEY,
    value      JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Saved pipeline-list views: a named set of filters (repos, ref, status,
-- runner, environment) a user can return to. owner is the session email (''
-- in open bootstrap mode); shared views are visible to everyone in the org.
CREATE TABLE IF NOT EXISTS pipeline_views (
    id         BIGSERIAL   PRIMARY KEY,
    owner      TEXT        NOT NULL DEFAULT '',
    name       TEXT        NOT NULL,
    filters    JSONB       NOT NULL DEFAULT '{}',
    shared     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner, name)
);

-- Server-side pipeline-list filters by runner and environment look jobs up by
-- these columns (see Store.ListPipelinesFiltered).
CREATE INDEX IF NOT EXISTS jobs_runner_idx ON jobs (runner_id, pipeline_id) WHERE runner_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS jobs_environment_idx ON jobs (environment, pipeline_id) WHERE environment IS NOT NULL;
CREATE INDEX IF NOT EXISTS pipelines_ref_idx ON pipelines (ref, id DESC);

-- Stored pipeline status. A pipeline's status is derived from its jobs (see
-- deriveStatus in store.go; the CASE below must stay in step with it), but
-- deriving it at read time means scanning every job of every pipeline for
-- repo counts and status filters. Instead, like most CI servers, the status
-- is materialized on pipelines and kept current by statement-level triggers
-- on jobs, so every writer (scheduler, runner API, retries, cancels) updates
-- it without having to remember to.
CREATE OR REPLACE FUNCTION forge_pipeline_status(pid BIGINT) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN count(*) = 0 THEN 'created'
        WHEN bool_or(j.status = 'failed' AND NOT j.allow_failure) THEN 'failed'
        WHEN bool_or(j.status = 'canceled') THEN 'canceled'
        WHEN bool_or(j.status IN ('running','pending')) THEN 'running'
        WHEN bool_or(j.status = 'blocked') THEN 'blocked'
        WHEN bool_or(j.status = 'created') THEN 'running'
        ELSE 'success' END
    FROM jobs j WHERE j.pipeline_id = pid
$$;

-- Recompute the status of pids. The rows are locked (in id order) before the
-- recompute, which runs as a separate statement: under READ COMMITTED it then
-- takes a fresh snapshot, so when two transactions finish jobs of the same
-- pipeline concurrently the second one sees the first's job and neither
-- leaves a stale status behind.
CREATE OR REPLACE FUNCTION forge_sync_pipeline_status(pids BIGINT[]) RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    IF cardinality(pids) = 0 THEN
        RETURN;
    END IF;
    PERFORM 1 FROM pipelines WHERE id = ANY(pids) ORDER BY id FOR UPDATE;
    UPDATE pipelines p SET status = s.status
    FROM (SELECT id, forge_pipeline_status(id) AS status
          FROM pipelines WHERE id = ANY(pids)) s
    WHERE p.id = s.id AND p.status IS DISTINCT FROM s.status;
END
$$;

-- Insert/delete: every touched pipeline. Update: only pipelines whose jobs
-- changed status or allow_failure (heartbeats and log bookkeeping don't count).
CREATE OR REPLACE FUNCTION forge_jobs_sync_status() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        PERFORM forge_sync_pipeline_status(ARRAY(
            SELECT DISTINCT n.pipeline_id FROM changed n JOIN old_rows o ON o.id = n.id
            WHERE n.status IS DISTINCT FROM o.status
               OR n.allow_failure IS DISTINCT FROM o.allow_failure
               OR n.pipeline_id IS DISTINCT FROM o.pipeline_id));
    ELSE
        PERFORM forge_sync_pipeline_status(ARRAY(SELECT DISTINCT pipeline_id FROM changed));
    END IF;
    RETURN NULL;
END
$$;

ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS status TEXT;

CREATE OR REPLACE TRIGGER jobs_sync_status_ins AFTER INSERT ON jobs
    REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION forge_jobs_sync_status();
CREATE OR REPLACE TRIGGER jobs_sync_status_upd AFTER UPDATE ON jobs
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION forge_jobs_sync_status();
CREATE OR REPLACE TRIGGER jobs_sync_status_del AFTER DELETE ON jobs
    REFERENCING OLD TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION forge_jobs_sync_status();

-- Backfill once (a no-op on later boots), then new pipelines start 'created'
-- until their jobs are inserted.
UPDATE pipelines SET status = forge_pipeline_status(id) WHERE status IS NULL;
ALTER TABLE pipelines ALTER COLUMN status SET DEFAULT 'created';
ALTER TABLE pipelines ALTER COLUMN status SET NOT NULL;

-- Repo cards count runs per repo and status and find the last run (an
-- index-only scan); the
-- pipeline list filters by status across repos.
CREATE INDEX IF NOT EXISTS pipelines_repo_status_idx ON pipelines (repo, status, id);
CREATE INDEX IF NOT EXISTS pipelines_status_idx ON pipelines (status, id DESC);

-- Environment registry (admin-managed, per server). Each row is one real
-- environment: its canonical name, display order, tier (colour/meaning),
-- aliases that mean the same environment (e.g. prd, prod → production), and
-- the branches allowed to deploy to it (glob patterns, '*' matches anything;
-- empty = any branch). An empty registry means "no policy": names are used as
-- pipelines write them and any branch may deploy anywhere, as before.
CREATE TABLE IF NOT EXISTS environment_registry (
    name         TEXT    PRIMARY KEY,
    position     INT     NOT NULL DEFAULT 0,
    tier         TEXT    NOT NULL DEFAULT 'other'
        CHECK (tier IN ('dev','staging','prod','other')),
    aliases      TEXT[]  NOT NULL DEFAULT '{}',
    allowed_refs TEXT[]  NOT NULL DEFAULT '{}',
    description  TEXT    NOT NULL DEFAULT ''
);

-- Why a job ended without running (e.g. its branch may not deploy to its
-- environment). Shown on the job and pipeline pages.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS status_reason TEXT NOT NULL DEFAULT '';

-- Approval notifications: when a job starts waiting for environment approval
-- the scheduler notifies the eligible approvers once (Slack and/or email, per
-- the 'notifications' platform setting) and stamps this column. NULL = not
-- notified yet; jobs blocked long ago are never back-filled.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS approval_notified_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS jobs_approval_unnotified_idx
    ON jobs (blocked_at) WHERE status = 'blocked' AND NOT manual AND approval_notified_at IS NULL;

-- resource_group: at most one job per (repo, resource_group) runs at a time
-- (e.g. one production deploy). '' = no group. Enforced at acquire.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS resource_group TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS jobs_resource_group_active_idx
    ON jobs (resource_group) WHERE resource_group <> '' AND status = 'running';

-- Retry a single job within its pipeline: the retried job (and the downstream
-- jobs that did not succeed) get NEW rows — fresh log, fresh attempt — and
-- the old rows stay as history with superseded = TRUE and retry_of linking
-- new → old. Only live (non-superseded) rows count for a pipeline's jobs, so
-- the per-pipeline name uniqueness moves to a partial index.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS superseded BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS retry_of BIGINT REFERENCES jobs(id) ON DELETE SET NULL;
ALTER TABLE jobs DROP CONSTRAINT IF EXISTS jobs_pipeline_id_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS jobs_pipeline_name_live_idx ON jobs (pipeline_id, name) WHERE NOT superseded;

CREATE OR REPLACE FUNCTION forge_pipeline_status(pid BIGINT) RETURNS TEXT
LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN count(*) = 0 THEN 'created'
        WHEN bool_or(j.status = 'failed' AND NOT j.allow_failure) THEN 'failed'
        WHEN bool_or(j.status = 'canceled') THEN 'canceled'
        WHEN bool_or(j.status IN ('running','pending')) THEN 'running'
        WHEN bool_or(j.status = 'blocked') THEN 'blocked'
        WHEN bool_or(j.status = 'created') THEN 'running'
        ELSE 'success' END
    FROM jobs j WHERE j.pipeline_id = pid AND NOT j.superseded
$$;

-- Approver groups: named lists of people (emails), admin-managed. An approval
-- rule can require "N approvals, at least M from group X"; members of X may
-- vote on that environment even without the repo role.
CREATE TABLE IF NOT EXISTS approver_groups (
    name        TEXT        PRIMARY KEY,
    description TEXT        NOT NULL DEFAULT '',
    members     TEXT[]      NOT NULL DEFAULT '{}',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE protected_environments ADD COLUMN IF NOT EXISTS required_group TEXT NOT NULL DEFAULT '';
ALTER TABLE protected_environments ADD COLUMN IF NOT EXISTS required_group_approvals INT NOT NULL DEFAULT 0;

-- Service accounts: non-human identities for automation, owned by the org and
-- managed by admins. Identity string is 'svc:<name>' — add it as a repo
-- member to grant repo access. Their API tokens live in api_tokens with
-- service_account_id set; a disabled account's tokens stop working at once.
CREATE TABLE IF NOT EXISTS service_accounts (
    id          BIGSERIAL   PRIMARY KEY,
    name        TEXT        NOT NULL UNIQUE,
    description TEXT        NOT NULL DEFAULT '',
    is_admin    BOOLEAN     NOT NULL DEFAULT FALSE,
    disabled    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS service_account_id BIGINT
    REFERENCES service_accounts(id) ON DELETE CASCADE;

-- Queue visibility: when a job last entered the queue (status → pending), by
-- whichever path (promotion, approval, retry requeue, drain requeue). Feeds
-- the runner load page and the forge_queue_* metrics.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS queued_at TIMESTAMPTZ;
CREATE OR REPLACE FUNCTION forge_jobs_stamp_queued() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'pending' AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM 'pending') THEN
        NEW.queued_at := now();
    END IF;
    RETURN NEW;
END
$$;
CREATE OR REPLACE TRIGGER jobs_stamp_queued BEFORE INSERT OR UPDATE OF status ON jobs
    FOR EACH ROW EXECUTE FUNCTION forge_jobs_stamp_queued();
CREATE INDEX IF NOT EXISTS jobs_pending_queue_idx ON jobs (queued_at) WHERE status = 'pending';

-- Runner cache affinity (opt-in per job: runner_affinity: true). A job's
-- affinity_key ('' = none) names the work whose local caches it benefits
-- from (repo + job name). After a successful run, runner_affinity remembers
-- which runner did it; the next such job prefers that runner while it is
-- online, and any runner may take it once it has waited the affinity window.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS affinity_key TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS runner_affinity (
    key        TEXT        PRIMARY KEY,
    runner_id  TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Template catalog: org-wide, versioned pipeline templates any repo can
-- include as `template: name@version` (or `template: name` for the newest).
-- Versions are immutable once published; a fix is a new version.
CREATE TABLE IF NOT EXISTS template_catalog (
    name         TEXT        NOT NULL,
    version      TEXT        NOT NULL,
    yaml         TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    published_by TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (name, version)
);

-- Deploy records and promotion. A job's environment may carry a URL
-- (environment: {name, url}); a job may write a dotenv report whose variables
-- flow to the jobs that need it and whose DEPLOY_URL / DEPLOY_VERSION /
-- DEPLOY_DIGEST fill the deployment record. A promotion pipeline redeploys an
-- earlier deployment's build to another environment (pipelines.promoted_from
-- = that deployment), and its deployment remembers where it came from.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS environment_url TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS report_dotenv   TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS dotenv          JSONB NOT NULL DEFAULT '{}';
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS url     TEXT NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS version TEXT NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS digest  TEXT NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN IF NOT EXISTS promoted_from BIGINT REFERENCES deployments(id) ON DELETE SET NULL;
ALTER TABLE pipelines ADD COLUMN IF NOT EXISTS promoted_from BIGINT REFERENCES deployments(id) ON DELETE SET NULL;

-- Coverage: from a coverage: /regex/ over the job log, or a Cobertura report.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS coverage_regex  TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS coverage_report TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS coverage        NUMERIC(6,2);

-- Per environment: cancel a waiting deploy once a newer pipeline has started
-- or finished deploying there (opt-in; see Store.CancelSupersededDeploys).
ALTER TABLE environment_registry ADD COLUMN IF NOT EXISTS cancel_superseded BOOLEAN NOT NULL DEFAULT FALSE;

-- Identity: any OpenID Connect IdP ('oidc') and LDAP / Active Directory
-- ('ldap') next to the built-in Google/Microsoft/GitHub buttons. The provider
-- CHECK only WIDENS (existing rows stay valid); it is re-stated on every start
-- so a database created before this change accepts the new providers.
-- settings holds provider-specific, non-secret config (OIDC issuer, groups
-- claim; LDAP URL, base DN, filters); client_secret holds the OIDC client
-- secret or the LDAP bind password, encrypted.
ALTER TABLE sso_providers DROP CONSTRAINT IF EXISTS sso_providers_provider_check;
ALTER TABLE sso_providers ADD CONSTRAINT sso_providers_provider_check
    CHECK (provider IN ('google','microsoft','github','oidc','ldap'));
ALTER TABLE sso_providers ADD COLUMN IF NOT EXISTS settings JSONB NOT NULL DEFAULT '{}';

-- The groups an identity provider last reported for a user, refreshed at
-- every sign-in. Role resolution reads it by email, so group-granted roles
-- apply to approvals and notifications too, not only to the signed-in request.
CREATE TABLE IF NOT EXISTS user_groups (
    email      TEXT        PRIMARY KEY,   -- lower-cased
    provider   TEXT        NOT NULL,
    groups     TEXT[]      NOT NULL DEFAULT '{}',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- IdP group → Forge role. A mapping either makes the group's members platform
-- admins, or gives them a role on every repo matching repo_pattern ('*' = all,
-- 'acme/*' = a namespace). provider '' matches a group from any provider.
CREATE TABLE IF NOT EXISTS idp_group_mappings (
    id             BIGSERIAL   PRIMARY KEY,
    provider       TEXT        NOT NULL DEFAULT '',
    idp_group      TEXT        NOT NULL,
    platform_admin BOOLEAN     NOT NULL DEFAULT FALSE,
    repo_pattern   TEXT        NOT NULL DEFAULT '',
    role           TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (platform_admin OR (repo_pattern <> '' AND role IN ('admin','owner','developer'))),
    UNIQUE (provider, idp_group, platform_admin, repo_pattern)
);

-- External secrets and registry logins a job asks the runner for (batch 10).
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS secrets         JSONB  NOT NULL DEFAULT '{}';
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS registry_logins TEXT[] NOT NULL DEFAULT '{}';

-- Notification routes (batch 11): where events go. kind is a notifier
-- (slack | teams | webhook | email | ...), target its address (webhook URL
-- or comma-separated emails; "approvers" = each eligible approver), secret
-- the HMAC key for webhook signatures. Both encrypted at rest. events lists
-- the event kinds the route takes; repo_pattern limits it ('*' = every repo).
CREATE TABLE IF NOT EXISTS notification_routes (
    id           BIGSERIAL   PRIMARY KEY,
    name         TEXT        NOT NULL,
    kind         TEXT        NOT NULL,
    target       TEXT        NOT NULL DEFAULT '',
    secret       TEXT        NOT NULL DEFAULT '',
    events       TEXT[]      NOT NULL DEFAULT '{}',
    repo_pattern TEXT        NOT NULL DEFAULT '*',
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Pipeline-level events are claimed once per (pipeline, event), like commit
-- statuses; deploy events once per job.
CREATE TABLE IF NOT EXISTS pipeline_notifications (
    pipeline_id BIGINT      NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    event       TEXT        NOT NULL,
    sent_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (pipeline_id, event)
);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS deploy_notified_at TIMESTAMPTZ;

-- Runner autoscaling (batch 12): a pool is a set of identical runners
-- (same tags) that a provider — kubernetes, docker, exec (any cloud via a
-- command) — can grow and shrink. settings is provider config, encrypted
-- (it may hold a Kubernetes token or the runner token); the status columns
-- are what the loop last observed and did.
CREATE TABLE IF NOT EXISTS runner_pools (
    id                 BIGSERIAL   PRIMARY KEY,
    name               TEXT        NOT NULL UNIQUE,
    provider           TEXT        NOT NULL,
    tags               TEXT[]      NOT NULL DEFAULT '{}',
    min_runners        INT         NOT NULL DEFAULT 0,
    max_runners        INT         NOT NULL DEFAULT 5,
    jobs_per_runner    INT         NOT NULL DEFAULT 1,
    scale_down_after_s INT         NOT NULL DEFAULT 600,
    settings           TEXT        NOT NULL DEFAULT '',
    enabled            BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- status
    current_runners    INT         NOT NULL DEFAULT 0,
    desired_runners    INT         NOT NULL DEFAULT 0,
    pending_jobs       INT         NOT NULL DEFAULT 0,
    last_action        TEXT        NOT NULL DEFAULT '',
    last_error         TEXT        NOT NULL DEFAULT '',
    last_scaled_at     TIMESTAMPTZ,
    last_busy_at       TIMESTAMPTZ,
    checked_at         TIMESTAMPTZ
);

-- Runner operations (batch 15): version reporting, drain, self-update
-- requests; per-job retry conditions (retry: {max, when}).
ALTER TABLE runners ADD COLUMN IF NOT EXISTS version             TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS draining            BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS drain_requested_at  TIMESTAMPTZ;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS update_requested_at TIMESTAMPTZ;
ALTER TABLE jobs    ADD COLUMN IF NOT EXISTS retry_when          TEXT[] NOT NULL DEFAULT '{always}';

-- Observability (batch 16): webhook delivery log, hourly rollups for long
-- history ranges.
CREATE TABLE IF NOT EXISTS webhook_log (
    id          BIGSERIAL   PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    provider    TEXT        NOT NULL,
    repo        TEXT        NOT NULL DEFAULT '',
    event       TEXT        NOT NULL DEFAULT '',
    status      INT         NOT NULL,
    reason      TEXT        NOT NULL DEFAULT '',
    delivery_id TEXT        NOT NULL DEFAULT '',
    source_ip   TEXT        NOT NULL DEFAULT '',
    pipeline_id BIGINT
);
CREATE INDEX IF NOT EXISTS webhook_log_repo_idx ON webhook_log (repo, id DESC);
CREATE INDEX IF NOT EXISTS webhook_log_received_idx ON webhook_log (received_at);
CREATE TABLE IF NOT EXISTS job_stats_hourly (
    hour         TIMESTAMPTZ NOT NULL,
    runner_id    TEXT        NOT NULL DEFAULT '',   -- '' = all runners
    jobs_started INT         NOT NULL DEFAULT 0,
    busy_seconds INT         NOT NULL DEFAULT 0,
    wait_p50     DOUBLE PRECISION NOT NULL DEFAULT 0,
    wait_p95     DOUBLE PRECISION NOT NULL DEFAULT 0,
    dur_p50      DOUBLE PRECISION NOT NULL DEFAULT 0,
    dur_p95      DOUBLE PRECISION NOT NULL DEFAULT 0,
    finished     INT         NOT NULL DEFAULT 0,
    failed       INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, runner_id)
);
CREATE TABLE IF NOT EXISTS pipeline_stats_hourly (
    hour    TIMESTAMPTZ NOT NULL,
    repo    TEXT        NOT NULL,
    success INT         NOT NULL DEFAULT 0,
    failed  INT         NOT NULL DEFAULT 0,
    other   INT         NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, repo)
);
CREATE INDEX IF NOT EXISTS jobs_started_at_idx ON jobs (started_at) WHERE started_at IS NOT NULL;

-- Runners page (batch 8): a runner's job slots, so capacity is online ×
-- concurrency rather than a runner count.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS concurrency INT NOT NULL DEFAULT 1;
CREATE INDEX IF NOT EXISTS audit_log_target_idx ON audit_log (target);

-- Runners page: the pool that launched a runner (RUNNER_POOL), and a
-- per-minute snapshot of capacity and queue depth, so those have history.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS pool TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS runner_capacity_snapshots (
    at              TIMESTAMPTZ PRIMARY KEY,           -- the minute
    total_slots     INT    NOT NULL DEFAULT 0,         -- slots on non-stale runners
    available_slots INT    NOT NULL DEFAULT 0,         -- online, not paused or draining
    busy_slots      INT    NOT NULL DEFAULT 0,         -- available slots running a job
    paused_slots    INT    NOT NULL DEFAULT 0,
    draining_slots  INT    NOT NULL DEFAULT 0,
    offline_slots   INT    NOT NULL DEFAULT 0,
    pending         INT    NOT NULL DEFAULT 0,         -- jobs waiting for a runner
    oldest_wait_s   INT    NOT NULL DEFAULT 0,
    starved_tags    TEXT[] NOT NULL DEFAULT '{}'       -- tags with runners but no available slot
);

-- Code review (batch 7, phase A): pull/merge requests from every host in one
-- model. Webhooks keep rows current; a refresh is claimed with a lease (like
-- a job) and coalesced per change: requests while one is due or in flight
-- ride along with it. Pages read only these tables — never the host.
-- state is open | merged | closed; draft is separate (an open change can be
-- a draft). number is the host's per-repo id (or Forge's, for native changes).
CREATE TABLE IF NOT EXISTS changes (
    id                   BIGSERIAL   PRIMARY KEY,
    repo                 TEXT        NOT NULL,
    provider             TEXT        NOT NULL,
    number               TEXT        NOT NULL,
    title                TEXT        NOT NULL DEFAULT '',
    body                 TEXT        NOT NULL DEFAULT '',
    author               TEXT        NOT NULL DEFAULT '',
    source_branch        TEXT        NOT NULL DEFAULT '',
    target_branch        TEXT        NOT NULL DEFAULT '',
    state                TEXT        NOT NULL DEFAULT 'open'
        CHECK (state IN ('open','merged','closed')),
    draft                BOOLEAN     NOT NULL DEFAULT FALSE,
    head_sha             TEXT        NOT NULL DEFAULT '',
    base_sha             TEXT        NOT NULL DEFAULT '',
    merge_base_sha       TEXT        NOT NULL DEFAULT '',
    labels               TEXT[]      NOT NULL DEFAULT '{}',
    url                  TEXT        NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ,
    updated_at           TIMESTAMPTZ,          -- the host's last update
    synced_at            TIMESTAMPTZ,          -- last write from the host (NULL: placeholder)
    last_event_at        TIMESTAMPTZ,          -- last webhook for this change
    refresh_due_at       TIMESTAMPTZ,
    refresh_requested_at TIMESTAMPTZ,
    refresh_priority     INT         NOT NULL DEFAULT 0,
    refresh_lease_until  TIMESTAMPTZ,
    refresh_attempts     INT         NOT NULL DEFAULT 0,
    refresh_error        TEXT        NOT NULL DEFAULT '',
    UNIQUE (repo, number)
);
CREATE INDEX IF NOT EXISTS changes_inbox_idx ON changes (state, updated_at DESC);
CREATE INDEX IF NOT EXISTS changes_refresh_idx ON changes (refresh_due_at) WHERE refresh_due_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS pipelines_repo_sha_idx ON pipelines (repo, sha, id DESC);

-- Every push is a revision, even on hosts that don't model one, so "changes
-- since revision N" works everywhere.
CREATE TABLE IF NOT EXISTS change_revisions (
    change_id BIGINT      NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
    number    INT         NOT NULL,
    head_sha  TEXT        NOT NULL,
    base_sha  TEXT        NOT NULL DEFAULT '',
    pushed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (change_id, number),
    UNIQUE (change_id, head_sha)
);

-- Reviews: source 'host' rows mirror the host's list (replaced on each
-- refresh); 'forge' rows are verdicts given in Forge.
CREATE TABLE IF NOT EXISTS change_reviews (
    id           BIGSERIAL   PRIMARY KEY,
    change_id    BIGINT      NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
    source       TEXT        NOT NULL DEFAULT 'host',
    external_id  TEXT        NOT NULL DEFAULT '',
    reviewer     TEXT        NOT NULL,
    verdict      TEXT        NOT NULL
        CHECK (verdict IN ('approve','request_changes','comment','dismissed')),
    body         TEXT        NOT NULL DEFAULT '',
    commit_sha   TEXT        NOT NULL DEFAULT '',
    submitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (change_id, source, external_id)
);

-- Per-repo reconcile: the conditional list's validators and when to run it
-- next (15 min while the repo has open changes, hours otherwise).
CREATE TABLE IF NOT EXISTS change_sync (
    repo               TEXT        PRIMARY KEY,
    etag               TEXT        NOT NULL DEFAULT '',
    last_modified      TEXT        NOT NULL DEFAULT '',
    next_reconcile_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_reconciled_at TIMESTAMPTZ,
    last_result        TEXT        NOT NULL DEFAULT '',
    lease_until        TIMESTAMPTZ
);

-- Provider budget per credential (an App installation, a token, an IAM
-- role): a token bucket in Postgres, row-locked, refilled continuously and
-- overwritten by the host's own rate-limit headers when it sends them.
CREATE TABLE IF NOT EXISTS provider_budgets (
    credential TEXT             PRIMARY KEY,
    provider   TEXT             NOT NULL,
    capacity   INT              NOT NULL,
    tokens     DOUBLE PRECISION NOT NULL,
    window_s   INT              NOT NULL DEFAULT 3600,
    reset_at   TIMESTAMPTZ,
    updated_at TIMESTAMPTZ      NOT NULL DEFAULT now()
);

-- Provider calls per hour by provider, class and result. read_path rows are
-- calls refused while serving a page: the count must stay 0.
CREATE TABLE IF NOT EXISTS provider_call_stats (
    hour      TIMESTAMPTZ NOT NULL,
    provider  TEXT        NOT NULL,
    class     TEXT        NOT NULL,
    result    TEXT        NOT NULL,
    read_path BOOLEAN     NOT NULL DEFAULT FALSE,
    calls     BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, provider, class, result, read_path)
);

-- Code review (batch 7, phase B): merge rules, the gate, provider writes.
-- A rule applies to changes of one repo ('' = every repo) whose target
-- branch matches branch_pattern (glob). No matching rule = no gate: nothing
-- is published, so turning code review on never changes a host's checks.
CREATE TABLE IF NOT EXISTS merge_policies (
    id                         BIGSERIAL   PRIMARY KEY,
    repo                       TEXT        NOT NULL DEFAULT '',
    branch_pattern             TEXT        NOT NULL DEFAULT '*',
    required_approvals         INT         NOT NULL DEFAULT 1,
    approver_roles             TEXT[]      NOT NULL DEFAULT '{admin,owner,developer}',
    required_group             TEXT        NOT NULL DEFAULT '',
    required_group_approvals   INT         NOT NULL DEFAULT 0,
    allow_self_approval        BOOLEAN     NOT NULL DEFAULT FALSE,
    dismiss_on_push            BOOLEAN     NOT NULL DEFAULT TRUE,
    block_on_changes_requested BOOLEAN     NOT NULL DEFAULT TRUE,
    require_ci                 BOOLEAN     NOT NULL DEFAULT TRUE,
    required_jobs              TEXT[]      NOT NULL DEFAULT '{}',
    required_labels            TEXT[]      NOT NULL DEFAULT '{}',
    blocked_labels             TEXT[]      NOT NULL DEFAULT '{}',
    ticket_pattern             TEXT        NOT NULL DEFAULT '',
    freeze_windows             JSONB       NOT NULL DEFAULT '[]',
    timezone                   TEXT        NOT NULL DEFAULT 'UTC',
    updated_by                 TEXT        NOT NULL DEFAULT '',
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (repo, branch_pattern)
);

-- Who a host login is in Forge (admin-maintained until accounts can be
-- linked by OAuth): makes self-approval, approver groups and roles work for
-- reviews given on the host. login is stored lowercased.
CREATE TABLE IF NOT EXISTS host_identities (
    provider   TEXT        NOT NULL,
    login      TEXT        NOT NULL,
    email      TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, login)
);

-- The gate's verdict per change and head commit; written only when it
-- changes. published_state is what the host's forge/merge-gate shows.
CREATE TABLE IF NOT EXISTS gate_results (
    change_id       BIGINT      NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
    head_sha        TEXT        NOT NULL,
    policy_id       BIGINT,
    state           TEXT        NOT NULL CHECK (state IN ('pass','pending','fail')),
    summary         TEXT        NOT NULL DEFAULT '',
    rules           JSONB       NOT NULL DEFAULT '[]',
    evaluated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_state TEXT        NOT NULL DEFAULT '',
    published_at    TIMESTAMPTZ,
    PRIMARY KEY (change_id, head_sha)
);

-- Re-evaluate a change's gate when its inputs move (webhook, review, CI,
-- rule edit) or when a freeze window opens/closes (gate_recheck_at).
ALTER TABLE changes ADD COLUMN IF NOT EXISTS gate_dirty_at   TIMESTAMPTZ;
ALTER TABLE changes ADD COLUMN IF NOT EXISTS gate_recheck_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS changes_head_idx ON changes (repo, head_sha) WHERE state = 'open';
CREATE INDEX IF NOT EXISTS changes_gate_due_idx ON changes (gate_dirty_at) WHERE gate_dirty_at IS NOT NULL;

-- A CI job changing state marks the open changes on that commit dirty.
-- Statement-level and a no-op while no change is open.
CREATE OR REPLACE FUNCTION forge_jobs_mark_gates() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM changes WHERE state = 'open' LIMIT 1) THEN
        RETURN NULL;
    END IF;
    UPDATE changes c SET gate_dirty_at = now()
    FROM (SELECT DISTINCT p.repo, p.sha FROM changed j JOIN pipelines p ON p.id = j.pipeline_id) x
    WHERE c.state = 'open' AND c.repo = x.repo AND c.head_sha = x.sha;
    RETURN NULL;
END
$$;
CREATE OR REPLACE TRIGGER jobs_mark_gates_ins AFTER INSERT ON jobs
    REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION forge_jobs_mark_gates();
CREATE OR REPLACE TRIGGER jobs_mark_gates_upd AFTER UPDATE ON jobs
    REFERENCING NEW TABLE AS changed
    FOR EACH STATEMENT EXECUTE FUNCTION forge_jobs_mark_gates();

-- Every provider write goes through this outbox: saved at once, sent by a
-- worker under the credential's budget, retried with backoff. idem_key
-- makes a double click or a retry one row; a newer gate publish for the
-- same change supersedes an unsent older one.
CREATE TABLE IF NOT EXISTS provider_outbox (
    id              BIGSERIAL   PRIMARY KEY,
    idem_key        TEXT        NOT NULL UNIQUE,
    kind            TEXT        NOT NULL,            -- publish_gate | merge
    repo            TEXT        NOT NULL,
    change_id       BIGINT      REFERENCES changes(id) ON DELETE CASCADE,
    class           TEXT        NOT NULL,            -- user | webhook | reconcile
    payload         JSONB       NOT NULL DEFAULT '{}',
    state           TEXT        NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','done','failed','superseded')),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until     TIMESTAMPTZ,
    last_error      TEXT        NOT NULL DEFAULT '',
    result          JSONB       NOT NULL DEFAULT '{}',
    requested_by    TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    done_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS provider_outbox_due_idx ON provider_outbox (next_attempt_at) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS provider_outbox_change_idx ON provider_outbox (change_id, id DESC);

-- Code review (batch 7, phase C): diffs from runner-held git mirrors.
-- runner_tasks is a queue of small system work runners do in-process (not
-- jobs: no pipeline, no executor, no job slot). affinity_key keeps a repo's
-- mirror on the runner that holds it (runner_affinity, as for job caches).
CREATE TABLE IF NOT EXISTS runner_tasks (
    id           BIGSERIAL   PRIMARY KEY,
    kind         TEXT        NOT NULL,
    repo         TEXT        NOT NULL,
    affinity_key TEXT        NOT NULL DEFAULT '',
    dedupe_key   TEXT        NOT NULL UNIQUE,
    payload      JSONB       NOT NULL DEFAULT '{}',
    state        TEXT        NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending','running','done','failed')),
    runner_id    TEXT        NOT NULL DEFAULT '',
    attempts     INT         NOT NULL DEFAULT 0,
    queued_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    not_before   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    lease_until  TIMESTAMPTZ,
    error        TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runner_tasks_pending_idx ON runner_tasks (queued_at) WHERE state IN ('pending','running');

-- One row per diff Forge knows: (from, to, mode) as asked — mode 'change'
-- diffs from the merge base of from (target tip) and to (head), 'range'
-- diffs the two commits. The bytes live in the blob store under blob_prefix
-- (immutable: commits never change), the row says where and how big.
CREATE TABLE IF NOT EXISTS change_diffs (
    repo         TEXT        NOT NULL,
    from_sha     TEXT        NOT NULL,
    to_sha       TEXT        NOT NULL,
    mode         TEXT        NOT NULL CHECK (mode IN ('change','range')),
    state        TEXT        NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','ready','failed')),
    merge_base   TEXT        NOT NULL DEFAULT '',
    blob_prefix  TEXT        NOT NULL DEFAULT '',
    chunks       INT         NOT NULL DEFAULT 0,
    files        INT         NOT NULL DEFAULT 0,
    additions    INT         NOT NULL DEFAULT 0,
    deletions    INT         NOT NULL DEFAULT 0,
    bytes        BIGINT      NOT NULL DEFAULT 0,
    fetched      BOOLEAN     NOT NULL DEFAULT FALSE,
    compute_ms   INT         NOT NULL DEFAULT 0,
    error        TEXT        NOT NULL DEFAULT '',
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ready_at     TIMESTAMPTZ,
    last_read_at TIMESTAMPTZ,
    PRIMARY KEY (repo, from_sha, to_sha, mode)
);
CREATE INDEX IF NOT EXISTS change_diffs_ready_idx ON change_diffs (ready_at) WHERE state = 'ready';

-- Diff page opens per hour: served ready (hit), had to compute (miss),
-- still computing (pending) or failed.
CREATE TABLE IF NOT EXISTS diff_read_stats (
    hour   TIMESTAMPTZ NOT NULL,
    result TEXT        NOT NULL,
    reads  BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, result)
);

-- Code review (batch 7, phase D): threads and comments, synced both ways.
-- A thread is on a line of the diff (path set) or is the change's
-- conversation (path '', external_id 'conversation'). Host threads and
-- comments are mirrored on each refresh; Forge comments are saved at once
-- (state pending) and sent through the outbox carrying a hidden marker
-- (forge_uid), which is how the host's echo is matched instead of duplicated.
CREATE TABLE IF NOT EXISTS change_threads (
    id                  BIGSERIAL   PRIMARY KEY,
    change_id           BIGINT      NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
    external_id         TEXT        NOT NULL DEFAULT '',
    root_comment_ext    TEXT        NOT NULL DEFAULT '', -- host id of the first comment (replies go to it)
    path                TEXT        NOT NULL DEFAULT '',
    side                TEXT        NOT NULL DEFAULT 'new' CHECK (side IN ('new','old')),
    line                INT         NOT NULL DEFAULT 0,  -- 0: not on a line, or outdated
    start_line          INT         NOT NULL DEFAULT 0,
    original_line       INT         NOT NULL DEFAULT 0,
    commit_sha          TEXT        NOT NULL DEFAULT '',
    original_commit     TEXT        NOT NULL DEFAULT '',
    anchor_head         TEXT        NOT NULL DEFAULT '', -- head the line was last placed on (Forge re-anchoring)
    fingerprint         TEXT        NOT NULL DEFAULT '', -- the anchored line's text
    resolved            BOOLEAN     NOT NULL DEFAULT FALSE,
    outdated            BOOLEAN     NOT NULL DEFAULT FALSE,
    resolve_pending     BOOLEAN     NOT NULL DEFAULT FALSE, -- Forge changed resolved; the host does not know yet
    resolved_by         TEXT        NOT NULL DEFAULT '',
    source              TEXT        NOT NULL DEFAULT 'host',
    created_by          TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS change_threads_ext_idx ON change_threads (change_id, external_id) WHERE external_id <> '';
CREATE INDEX IF NOT EXISTS change_threads_change_idx ON change_threads (change_id);

CREATE TABLE IF NOT EXISTS change_comments (
    id           BIGSERIAL   PRIMARY KEY,
    thread_id    BIGINT      NOT NULL REFERENCES change_threads(id) ON DELETE CASCADE,
    external_id  TEXT        NOT NULL DEFAULT '',
    forge_uid    TEXT        NOT NULL DEFAULT '',
    author       TEXT        NOT NULL DEFAULT '',
    author_email TEXT        NOT NULL DEFAULT '',
    body         TEXT        NOT NULL DEFAULT '',
    source       TEXT        NOT NULL DEFAULT 'host',
    state        TEXT        NOT NULL DEFAULT 'sent' CHECK (state IN ('pending','sent','failed','local')),
    error        TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS change_comments_ext_idx ON change_comments (thread_id, external_id) WHERE external_id <> '';
CREATE UNIQUE INDEX IF NOT EXISTS change_comments_uid_idx ON change_comments (forge_uid) WHERE forge_uid <> '';
CREATE INDEX IF NOT EXISTS change_comments_thread_idx ON change_comments (thread_id, id);

-- Host accounts people link from Forge (OAuth): their comments, reviews and
-- merges are then made as them, with their own rate budget. The token is
-- encrypted at rest. A verified link also maps the login in host_identities.
CREATE TABLE IF NOT EXISTS user_host_tokens (
    email      TEXT        NOT NULL,
    provider   TEXT        NOT NULL,
    login      TEXT        NOT NULL,
    token      TEXT        NOT NULL,
    scopes     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (email, provider)
);
ALTER TABLE host_identities ADD COLUMN IF NOT EXISTS verified BOOLEAN NOT NULL DEFAULT FALSE;

-- Code review (batch 7, phase F): code owners, the merge queue, merges done
-- by runners.
-- A rule can require code owners' approval (CODEOWNERS read from the target
-- branch by the runner that computes the diff) and send merges through the
-- queue.
ALTER TABLE merge_policies ADD COLUMN IF NOT EXISTS require_code_owners BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE merge_policies ADD COLUMN IF NOT EXISTS merge_queue BOOLEAN NOT NULL DEFAULT FALSE;
-- What a 'change' diff touched (renames count both paths; NULL for diffs
-- stored before phase F) and the CODEOWNERS file at its target tip
-- (owners_read false: computed by a runner too old to look).
ALTER TABLE change_diffs ADD COLUMN IF NOT EXISTS paths TEXT[];
ALTER TABLE change_diffs ADD COLUMN IF NOT EXISTS owners_read BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE change_diffs ADD COLUMN IF NOT EXISTS codeowners_path TEXT NOT NULL DEFAULT '';
ALTER TABLE change_diffs ADD COLUMN IF NOT EXISTS codeowners TEXT NOT NULL DEFAULT '';
-- A task's answer (a merge's commit, or why it did not merge).
ALTER TABLE runner_tasks ADD COLUMN IF NOT EXISTS result JSONB;

-- The merge queue: per repo and target branch, one change at a time is
-- merged onto the target's tip by a runner, pushed to forge-queue/<target>,
-- tested by a Forge pipeline, and merged only if that passes — so two
-- changes that pass alone cannot break the target together.
CREATE TABLE IF NOT EXISTS merge_queue (
    id            BIGSERIAL   PRIMARY KEY,
    change_id     BIGINT      NOT NULL REFERENCES changes(id) ON DELETE CASCADE,
    repo          TEXT        NOT NULL,
    target_branch TEXT        NOT NULL,
    head_sha      TEXT        NOT NULL,
    method        TEXT        NOT NULL,
    state         TEXT        NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued','testing','merging','merged','failed','removed')),
    base_sha      TEXT        NOT NULL DEFAULT '',
    test_sha      TEXT        NOT NULL DEFAULT '',
    pipeline_id   BIGINT,
    outbox_id     BIGINT,
    error         TEXT        NOT NULL DEFAULT '',
    requested_by  TEXT        NOT NULL DEFAULT '',
    enqueued_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS merge_queue_active_change_idx ON merge_queue (change_id)
    WHERE state IN ('queued','testing','merging');
CREATE INDEX IF NOT EXISTS merge_queue_active_idx ON merge_queue (repo, target_branch, id)
    WHERE state IN ('queued','testing','merging');

-- Forge Live (batch 9). A target is where applications run — a Kubernetes
-- cluster, a Cloud Run project — reported by one forge-agent whose token is
-- scoped to it. Forge never calls a target: it stores what the agent sends.
-- Per application the indexable status lives here; the resource tree
-- (content-addressed, one blob per version) and each resource's live
-- manifest live in the blob store.
CREATE TABLE IF NOT EXISTS live_targets (
    id            BIGSERIAL   PRIMARY KEY,
    name          TEXT        NOT NULL UNIQUE,
    environment   TEXT        NOT NULL DEFAULT '',
    repo          TEXT        NOT NULL DEFAULT '',
    token_hash    TEXT        NOT NULL UNIQUE,
    token_suffix  TEXT        NOT NULL,
    provider      TEXT        NOT NULL DEFAULT '',
    capabilities  JSONB       NOT NULL DEFAULT '{}',
    agent_version TEXT        NOT NULL DEFAULT '',
    -- The agent's resume token: its current epoch (one full report) and the
    -- last batch applied in it. synced: the epoch's initial report is in.
    epoch         TEXT        NOT NULL DEFAULT '',
    seq           BIGINT      NOT NULL DEFAULT 0,
    synced        BOOLEAN     NOT NULL DEFAULT FALSE,
    stats         JSONB       NOT NULL DEFAULT '{}',
    bytes_in      BIGINT      NOT NULL DEFAULT 0,
    last_seen_at  TIMESTAMPTZ,
    created_by    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS live_apps (
    id             BIGSERIAL   PRIMARY KEY,
    target_id      BIGINT      NOT NULL REFERENCES live_targets(id) ON DELETE CASCADE,
    key            TEXT        NOT NULL,
    name           TEXT        NOT NULL,
    namespace      TEXT        NOT NULL DEFAULT '',
    app_group      TEXT        NOT NULL DEFAULT '',
    health         TEXT        NOT NULL DEFAULT 'Unknown',
    health_message TEXT        NOT NULL DEFAULT '',
    health_since   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Compared with Git from phase B on; until then Unknown.
    sync           TEXT        NOT NULL DEFAULT 'Unknown',
    resources      INT         NOT NULL DEFAULT 0,
    pods           INT         NOT NULL DEFAULT 0,
    pods_ready     INT         NOT NULL DEFAULT 0,
    images         TEXT[]      NOT NULL DEFAULT '{}',
    tree           TEXT        NOT NULL DEFAULT '', -- blob key of the current tree
    version        BIGINT      NOT NULL DEFAULT 0,
    epoch          TEXT        NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (target_id, key)
);
CREATE INDEX IF NOT EXISTS live_apps_name_idx ON live_apps (name, id);

-- What the platform said about an app's resources (Kubernetes events);
-- the newest 200 per app are kept.
CREATE TABLE IF NOT EXISTS live_app_events (
    app_id       BIGINT      NOT NULL REFERENCES live_apps(id) ON DELETE CASCADE,
    uid          TEXT        NOT NULL,
    resource_uid TEXT        NOT NULL,
    kind         TEXT        NOT NULL DEFAULT '',
    name         TEXT        NOT NULL DEFAULT '',
    type         TEXT        NOT NULL DEFAULT '',
    reason       TEXT        NOT NULL DEFAULT '',
    message      TEXT        NOT NULL DEFAULT '',
    count        INT         NOT NULL DEFAULT 1,
    first_seen   TIMESTAMPTZ NOT NULL,
    last_seen    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (app_id, uid)
);
CREATE INDEX IF NOT EXISTS live_app_events_recent_idx ON live_app_events (app_id, last_seen DESC);

-- Blobs nothing references any more (an older tree, a deleted resource's
-- manifest), deleted after a grace period so a page mid-read still finds them.
CREATE TABLE IF NOT EXISTS live_blob_gc (
    key    TEXT        PRIMARY KEY,
    due_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS live_blob_gc_due_idx ON live_blob_gc (due_at);
