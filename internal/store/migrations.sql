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

-- Repo membership: who is what on a repo. Enforced at the approval endpoint
-- (identity is client-asserted until OIDC fronts the API — see docs).
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

-- Job routing tags and artifact declarations.
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
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
