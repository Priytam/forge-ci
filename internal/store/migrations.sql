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
INSERT INTO protected_environments (repo, name, required_approvals)
SELECT '', 'production', 1
WHERE NOT EXISTS (SELECT 1 FROM protected_environments WHERE repo='' AND name='production');

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
    provider       TEXT PRIMARY KEY CHECK (provider IN ('google','microsoft','github')),
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

-- First-class repo registry: the connection to the real VCS repo. token is
-- used to build authenticated clone URLs for runners (never returned by the
-- API, never logged). Plaintext at rest for now — same caveat as variables.
CREATE TABLE IF NOT EXISTS repo_registry (
    repo           TEXT PRIMARY KEY,
    provider       TEXT NOT NULL CHECK (provider IN ('github','bitbucket','other')),
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
