# Requirement: AWS CodeCommit as a source provider

**Status:** proposed · **Priority:** P0 (blocks Tablespace onboarding) · **Owner:** _tbd_

## Why

Forge CI today integrates with source control via **GitHub and Bitbucket only**
(`internal/api/webhooks.go`, `internal/vcs/fetch.go`, `internal/vcs/status.go`).
The adopting org hosts all repositories in **AWS CodeCommit**, which uses
**EventBridge** for events (not webhooks) and **IAM SigV4** for git auth (not an
HTTPS token). A generic `provider: "other"` exists but only clones by token and
supports neither triggering, in-repo config, nor status — so it is not enough.

This requirement adds a first-class `codecommit` provider so CodeCommit repos
get the same flow GitHub repos get today: push/PR triggering, `.forge-ci.yml`
fetched at the event SHA, source clone into the job, and status feedback.

## Scope — four pieces

### 1. Trigger ingestion (EventBridge → pipeline)
- Add `POST /api/v1/webhooks/codecommit`, auth-exempt like the other webhook
  routes (`internal/api/security.go:202`, `auth.go:102`), fed by an EventBridge
  **API Destination** (the deployment side wires the rule; out of scope here).
- Parse the CodeCommit EventBridge detail and map to `{repo, ref, sha}`:
  - `Reference Changes` (`referenceCreated` / `referenceUpdated`) → push, using
    `detail.repositoryName`, `detail.referenceName`, `detail.commitId`.
  - `Pull Request State Change` (`pullRequestCreated` / `SourceUpdated`) →
    `CI_PIPELINE_SOURCE=merge_request`, build the PR source commit, populate the
    `CI_MERGE_REQUEST_*` context already used by the GitHub PR path.
- Dedupe redelivered events by event `id` (mirror the existing webhook dedupe).

### 2. Config fetch (`.forge-ci.yml` at the SHA)
- Add `case "codecommit"` in `internal/vcs/fetch.go` that reads the config file
  via the CodeCommit **GetFile** API (SigV4-signed, AWS SDK for Go v2) at the
  event commit, honouring the existing `config_source` toggle and the fall-back
  to registered config.

### 3. Provider registration
- Add `"codecommit"` to the allowed-provider switch and clone-URL derivation in
  `internal/api/webhooks.go:98`. Registered clone URL form:
  `codecommit::<region>://<repo>` (git-remote-codecommit) **or** the
  `https://git-codecommit.<region>.amazonaws.com/v1/repos/<repo>` GRC URL.
- `region` (and optional `profile`/`role_arn`) become part of the repo registry
  record for CodeCommit repos.

### 4. Clone auth — IAM SigV4 (the non-obvious piece)
- Forge clones today with an HTTPS **token** (`https://<token>@host/...`).
  CodeCommit has no token; the clone must be **IAM-signed**.
- The runner must clone using its AWS identity via one of:
  - **`git-remote-codecommit`** (`codecommit::` remote) with the runner's AWS
    credentials on PATH, **or**
  - the CodeCommit credential-helper / SigV4 GRC URL.
- **Decision required:** where the runner's AWS identity comes from —
  **(recommended)** Forge's existing OIDC keyless path
  (`FORGE_OIDC_TOKEN` → STS `AssumeRoleWithWebIdentity` → role with
  `codecommit:GitPull`), which keeps runners key-less; **or** an instance role
  on the runner host. Provider should work with either; default to OIDC.

## Explicit non-goal
- **Native commit-status checks are impossible on CodeCommit** (no status API).
  `internal/vcs/status.go` gets a `codecommit` case that posts a PR **comment**
  via `PostCommentForPullRequest` instead. Document that CodeCommit repos get a
  comment, not a green/red check. Do not fake a status.

## Acceptance criteria
1. A push to a registered CodeCommit repo (via the EventBridge destination)
   creates and runs a pipeline, with `.forge-ci.yml` read from that commit.
2. A CodeCommit PR event builds the PR source commit with `merge_request`
   source and the `CI_MERGE_REQUEST_*` context populated.
3. The job clones the CodeCommit source at the pipeline SHA using IAM auth, with
   no static git credentials stored.
4. Redelivered EventBridge events do not create duplicate pipelines.
5. On completion, a comment is posted to the originating PR (when the run came
   from a PR); non-PR runs post nothing and error nowhere.
6. GitHub and Bitbucket behaviour is unchanged (regression-tested).

## Out of scope (tracked separately)
- SCIM provisioning; RBAC roles above login; KMS/Vault secret backend + key
  rotation; session idle timeout; child/multi-project pipelines. See
  `docs/requirements/enterprise-readiness.md`.
