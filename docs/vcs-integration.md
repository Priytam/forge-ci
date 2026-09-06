# GitHub / Bitbucket integration

> **AWS CodeCommit** is also a first-class provider, but it works differently
> enough — EventBridge instead of webhooks, IAM SigV4 instead of a token, a pull
> request comment instead of a commit status — that it has its own guide:
> **[codecommit.md](codecommit.md)**. This page covers the two token-based
> providers.

Forge is a standalone CI system — it does not host repositories. Three things
connect it to your VCS:

0. **A connected repository** (UI: Repos → Add repository, or the API) — so
   runners can clone the source at the pipeline's SHA before running jobs:
   ```sh
   curl -X POST $FORGE/api/v1/repo-registry -d '{
     "repo": "acme/checkout-service", "provider": "github",
     "token": "<fine-grained PAT with repo read access — omit for public repos>",
     "default_branch": "main"
   }'
   ```
   Access is verified with `git ls-remote` before saving. The token is
   write-only (never returned by the API), embedded only in the clone URL
   handed to runners, and redacted from all job logs. Without a connected
   repo, jobs still run — but in an empty workspace.

   A connection can authenticate with **either** a static token (the PAT above)
   **or** a GitHub App — see [GitHub App authentication](#github-app-authentication).

1. **The pipeline config.** By default Forge reads `.forge-ci.yml` **from the
   repo itself**, fetched at the pushed/PR sha over the provider's contents API
   using the connection from step 0 — so pipeline config rides in the commit/PR,
   exactly like GitLab/GitHub. See
   [Config from the repo](#config-from-the-repo-forge-ciyml). When a repo has no
   in-repo file, Forge falls back to a **registered** config stored per repo:
   ```sh
   curl -X PUT $FORGE/api/v1/repo-configs \
     -d "$(jq -n --arg cfg "$(cat forge-ci.yml)" '{repo:"acme/checkout-service", config:$cfg}')"
   ```
   Re-PUT to update; the YAML is validated on save.

2. **A webhook** from the provider — push events, and (GitHub / Bitbucket)
   pull-request events. See [PR/MR-triggered pipelines](#prmr-triggered-pipelines).
   (CodeCommit has no webhooks; an EventBridge API Destination feeds
   `/api/v1/webhooks/codecommit` instead — see [codecommit.md](codecommit.md).)

## GitHub — step by step

1. Register the repo's pipeline config (above). Use the exact
   `owner/repo` full name.
2. (Recommended) set a shared secret on the server: start `forge-server` with
   `WEBHOOK_SECRET=<random>` — GitHub's `X-Hub-Signature-256` HMAC is then
   verified on every delivery.
3. In GitHub: **Repo → Settings → Webhooks → Add webhook**
   - Payload URL: `https://<forge-host>/api/v1/webhooks/github`
   - Content type: `application/json`
   - Secret: the same `WEBHOOK_SECRET`
   - Events: *Push* — and, for PR pipelines, also *Pull requests*
     (see [PR/MR-triggered pipelines](#prmr-triggered-pipelines))
4. Push a commit. GitHub's "Recent Deliveries" should show **201**, and the
   pipeline appears under the repo card in Forge with the pusher recorded as
   `triggered_by` (which drives the self-approval rule).

## Bitbucket Cloud — step by step

1. Register the repo's pipeline config with the Bitbucket
   `workspace/repo-slug` full name.
2. In Bitbucket: **Repository settings → Webhooks → Add webhook**
   - URL: `https://<forge-host>/api/v1/webhooks/bitbucket`
   - Triggers: *Repository push* — and, for PR pipelines, *Pull request →
     Created* and *Updated* (see [PR/MR-triggered pipelines](#prmr-triggered-pipelines))
3. Push a commit; the pipeline triggers with the actor as `triggered_by`.
   (Bitbucket Cloud has no HMAC signing — restrict by network/allowlist or a
   token in the URL if exposure is a concern.)

## GitHub App authentication

Instead of a long-lived static PAT, a GitHub-hosted repo can authenticate with a
**GitHub App**. Forge signs a short-lived JWT with the App's private key, exchanges
it for a ~1-hour installation access token, and uses that token exactly like a PAT
(clone via `x-access-token`, commit-status via `Bearer`). Tokens auto-expire and
Forge mints fresh ones on demand, caching each until it is near expiry — so there
is no long-lived credential sitting in the database.

### PAT vs App — which to use

| | Static PAT | GitHub App |
|---|---|---|
| Credential at rest | the token itself (long-lived) | a private key (long-lived) that only **mints** short-lived tokens |
| Token lifetime | until you rotate/revoke it | ~1 hour, auto-rotated by Forge |
| Scope | the user's/PAT's access | exactly the App's declared permissions, on the installed repos |
| Identity in the audit trail on GitHub | the PAT owner | the App (a bot identity) |
| Rate limits | per-user | per-installation (higher) |
| Best for | quick setup, a single repo | orgs, many repos, least-privilege, no human-owned token |

Both are equally supported. The static-token path is unchanged; App auth is
opt-in per connection.

### Create the App and collect its three values

You need three things: an **app id**, a **private key** (PEM), and the
**installation id**.

1. **Create the App.** GitHub → *Settings → Developer settings → GitHub Apps →
   New GitHub App* (an org App lives under the org's settings). Give it a name and
   a homepage URL (any valid URL). You can leave the webhook **unchecked** — Forge
   receives pushes through its own webhook (see above); the App is used only for
   auth, not for event delivery.
2. **Set permissions** (App → *Permissions & events → Repository permissions*):
   - **Contents: Read-only** — required to clone the source.
   - **Commit statuses: Read and write** — required for commit-status write-back
     (skip if you don't use it).
   - Nothing else is needed. This is the least-privilege set.
3. **Note the app id** — shown on the App's *General* page ("App ID").
4. **Generate a private key** — App *General* page → *Private keys → Generate a
   private key*. A `.pem` file downloads. This is the file's full contents
   (`-----BEGIN RSA PRIVATE KEY-----` … PKCS#1, or `-----BEGIN PRIVATE KEY-----`
   PKCS#8 — both are accepted). Treat it as a secret; it is never shown again by
   GitHub.
5. **Install the App** on the org/repos (App → *Install App*). After installing,
   the browser URL is
   `https://github.com/settings/installations/<INSTALLATION_ID>` (or, for an org,
   `…/organizations/<org>/settings/installations/<INSTALLATION_ID>`) — that number
   is the **installation id**. (Programmatically it's also available from
   `GET /app/installations` using an App JWT.)

### Configure it in Forge

Register the repo with the App fields instead of `token`:

```sh
curl -X POST $FORGE/api/v1/repo-registry \
  -d "$(jq -n --arg key "$(cat app-private-key.pem)" '{
        repo: "acme/checkout-service",
        provider: "github",
        github_app_id: "123456",
        github_installation_id: "789012",
        github_app_private_key: $key
      }')"
```

- **Admin-gated & audited.** Registration requires platform-admin (once SSO is
  enforced) and is recorded in the audit trail — with the provider, app id and
  installation id, but **never** the private key or any minted token.
- **Validated before saving.** Like the PAT path, Forge proves the config works
  before storing it: it mints an installation token and runs `git ls-remote` with
  it. A bad key/app id/installation id, or an installation missing *Contents:
  read*, is rejected with an actionable error (the key and token never appear in
  it). Skip validation with `?validate=0`.
- **Write-only key.** `github_app_private_key` is encrypted at rest with
  `FORGE_SECRET_KEY` (the same `enc:v1:` envelope as tokens and SSO secrets) and
  is **never** returned by `GET /api/v1/repo-registry`. That endpoint surfaces
  `has_github_app`, `github_app_id` and `github_installation_id` only. To rotate
  the key, POST again with a new `github_app_private_key`; omit it on an edit to
  keep the stored one.
- **Precedence.** If a connection has both an App and a static token configured,
  the App is used.
- **GitHub only.** App auth requires `provider: "github"`. Bitbucket keeps using
  app passwords / tokens.

### How tokens are minted (and kept safe)

`internal/githubapp` signs an RS256 JWT (`iss` = app id, `iat` backdated 60s for
clock skew, `exp` ≤ 10 min) with the private key, POSTs it to
`/app/installations/{id}/access_tokens`, and caches the returned token in memory
keyed by app+installation until it is within 5 minutes of expiry, then re-mints.
Minting is thread-safe and serialized per installation. The private key and the
minted tokens are never logged and never appear in returned errors. The GitHub
API base is overridable via `GITHUB_API_BASE` (used only for testing against a
stub; leave unset in production).

> The full round-trip against **live** GitHub requires a real GitHub App and its
> private key, so it is not exercised by Forge's automated tests. Those cover the
> JWT signing, the mint/cache/refresh behavior, and the clone/status resolution
> against an HTTP stub (a throwaway RSA key stands in for the App key). If a real
> App is misconfigured, registration validation surfaces it before saving, and a
> later mint failure is logged (token/key redacted) and treated like any other
> transient/permanent VCS error.

## Config from the repo (`.forge-ci.yml`)

Forge can read a repo's pipeline config **straight from the repo**, so config
rides in the commit/PR instead of only living in a registered copy — the big DX
win: a branch or PR that changes `.forge-ci.yml` runs *that* config, and reviews
of the pipeline happen in the PR alongside the code.

**How it works.** When a push or PR webhook fires, Forge fetches `.forge-ci.yml`
**at the exact event sha** over the provider's contents API, authenticated with
the same connection as the clone (a static PAT or a freshly-minted GitHub App
installation token — the App needs **Contents: read**). It then compiles and runs
that YAML. This requires a **connected repo with valid auth** (step 0); the token
is used only in the outbound `Authorization` header and is never logged.

- **GitHub:** `GET {GITHUB_API_BASE|https://api.github.com}/repos/{owner}/{repo}/contents/.forge-ci.yml?ref={sha}`
  with `Accept: application/vnd.github.raw` (base64 `content` is also decoded).
- **Bitbucket:** `GET {BITBUCKET_API_BASE|https://api.bitbucket.org}/2.0/repositories/{ws}/{repo}/src/{sha}/.forge-ci.yml`.
- Provider `other`, an unregistered repo, or a **404** (no such file) → treated
  as "not found" and Forge falls back (below), never an error.

**Precedence and the `config_source` toggle.** Each connected repo has a
`config_source` setting:

| `config_source` | Behavior |
|---|---|
| `repo` (**default**) | Prefer the in-repo `.forge-ci.yml` at the event sha; **fall back** to the registered config when the file is absent. If neither exists, the usual `404 "no pipeline config"` applies. |
| `registered` | Only ever use the registered config — the repo file is **never** fetched, even if present. |

Set it (and, optionally, a `config_path` override — default `.forge-ci.yml`) when
connecting the repo:

```sh
curl -X POST $FORGE/api/v1/repo-registry -d '{
  "repo": "acme/checkout-service", "provider": "github",
  "token": "<PAT with Contents:read>",
  "config_source": "repo",          # or "registered"
  "config_path": ".forge-ci.yml"     # optional override
}'
```

**Version stamping.** A pipeline built from an in-repo config is **not** stamped
with a registered `config_version` (it isn't a registry version — `config_version`
is `NULL`) and is recorded with `config_source = repo`; a registered-config run
keeps its version and `config_source = registered`. Either way the pipeline's
stored `config_yaml` is **exactly the YAML that ran**, so retries and audit see
precisely what executed.

**Scheduled pipelines** honor the same precedence: a scheduled fire resolves the
ref tip and then applies `config_source` at that sha (in-repo preferred by
default, registered fallback).

The manual API path (`POST /api/v1/pipelines` with an explicit `config`) is
unchanged — it always runs the config you supply.

### Who did what

Forge records three separate identities per run, because they are three
different facts and conflating them is how "who deployed this?" gets the wrong
answer:

| Field | Question it answers | Source |
|---|---|---|
| `triggered_by` | who **started** the run | pusher (push), PR author (PR), caller (API), `schedule` |
| `commit_author` | who **wrote** the code | GitHub `head_commit.author`, Bitbucket push target author |
| approvals | who **released** it | the `job_approvals` vote record, with comments |

The first two differ exactly where it matters — a merge, a rebase, a bot push,
or a re-run of an old commit all have an actor who is not the author. The
pipeline page names them separately only when they differ; for an ordinary push
by the person who wrote the code, one identity is the honest rendering.

`commit_message` carries what the run is about — the commit message for a push,
the pull-request title for a PR run — and the UI shows its subject line, so a
run is identified by what it does rather than by a hex sha.

**CodeCommit is the gap:** its EventBridge payload carries only the commit id,
no author and no message, so both stay empty for a CodeCommit push (the PR title
is available and is used for PR runs). Filling them would need a `GetCommit`
call at ingest; Forge does not make one.

### Viewing the config a run used

A pipeline built from the repo carries a **`config_url`** on
`GET /api/v1/pipelines/{id}`, and the pipeline detail page renders it as a
**view config ↗** link. It opens the config file **at that run's commit** in the
provider's own web UI, so re-reading an old run shows the definition that
actually executed rather than whatever the branch says now:

| Provider | Link shape |
|---|---|
| GitHub | `https://<host>/<owner>/<name>/blob/<sha>/<path>` |
| Bitbucket | `https://<host>/<workspace>/<slug>/src/<sha>/<path>` |
| AWS CodeCommit | `https://<region>.console.aws.amazon.com/codesuite/codecommit/repositories/<name>/browse/<sha>/--/<path>?region=<region>` |

The host comes from the **registered clone URL** when that is an HTTPS URL, so a
self-hosted GitHub Enterprise or Bitbucket Server install links to itself; only
the public hosts are defaults. The repo's `config_path` override is honored.

**The link appears only for a `config_source = repo` run.** A
registered-config run did not execute the file sitting in git at that commit, so
linking there would show a pipeline definition that is not the one that ran —
the exact confusion the link exists to remove. Those runs keep their `config
vN` chip, and the registry serves each version at
`GET /api/v1/repo-configs?repo=…&version=N`. Every other case where a correct
URL cannot be built — provider `other`, an unregistered repo, a CodeCommit repo
with no region, a missing sha — omits `config_url` entirely, and the page renders
normally with nothing to click. There is never a broken link.

## Behavior notes

- The ref from the webhook drives `only`/`except`, so pushes to `main` and to
  feature branches compile different DAGs from the same config (in-repo or
  registered).
- Push and pull-request events are handled; **any other event** (GitHub — e.g.
  `issues`, `X-GitHub-Event` other than `push`/`pull_request`) and empty push
  change lists (Bitbucket) are acknowledged with **202** and ignored. See
  [PR/MR-triggered pipelines](#prmr-triggered-pipelines) for which PR actions run.
- **Delivery dedup:** providers redeliver events (retries, manual redelivery).
  Each delivery is recorded by id — GitHub `X-GitHub-Delivery`, Bitbucket
  `X-Request-UUID`, or a SHA-256 of the body when no header is present — so a
  duplicate returns **200** (`{"status":"duplicate delivery ignored"}`) without
  creating a second pipeline. Records are bounded by `RETENTION_DAYS`.
- **Auto-cancel:** a new pipeline for the same repo+ref cancels older
  non-terminal ones (unless the YAML sets `auto_cancel: false`). Pushing twice
  in quick succession leaves only the newest pipeline running.

## PR/MR-triggered pipelines

Beyond branch pushes, Forge builds a pipeline for a **pull request** (GitHub) /
**pull request** (Bitbucket) so PR checks run on the exact code that would merge.

- **Events that trigger a build:**
  - GitHub — `X-GitHub-Event: pull_request` with action **`opened`**,
    **`synchronize`** (new commits pushed to the PR) or **`reopened`**. Every
    other action (`closed`, `edited`, `labeled`, …) is acknowledged with **202**
    and creates nothing. Enable *Pull requests* on the GitHub webhook.
  - Bitbucket — `X-Event-Key: pullrequest:created` or `pullrequest:updated`.
    Other PR event keys (`pullrequest:approved`, `:fulfilled`, …) → **202**.
- **Built on the PR HEAD, at the head branch ref.** The pipeline's SHA is the PR
  **head commit** (GitHub `pull_request.head.sha`, Bitbucket
  `pullrequest.source.commit.hash`) and the **compile ref is the head/source
  branch** — so branch-keyed `only`/`except` and `rules` still apply, and the
  commit status lands on the PR head (see below). Forge builds the committed HEAD
  of the source branch, not a speculative merge commit.
- **`CI_PIPELINE_SOURCE = merge_request`.** PR pipelines set the source to
  `merge_request` (distinct from `push`/`webhook`), so you can route jobs to run
  only on PRs:
  ```yaml
  jobs:
    pr-lint:
      stage: test
      script: [make lint]
      rules:
        - if: '$CI_PIPELINE_SOURCE == "merge_request"'
  ```
  On a plain push this job's rule does not match and it is excluded; on a PR it
  is included.
- **Merge-request context variables.** These are available both to `rules:` `if:`
  expressions **and** in every job's environment (so scripts can read them):

  | Variable | Value |
  |---|---|
  | `CI_PIPELINE_SOURCE` | `merge_request` |
  | `CI_MERGE_REQUEST_IID` | PR/MR number (GitHub `number`, Bitbucket `pullrequest.id`) |
  | `CI_MERGE_REQUEST_SOURCE_BRANCH` | head/source branch (also the compile ref) |
  | `CI_MERGE_REQUEST_TARGET_BRANCH` | base/destination branch |
  | `CI_MERGE_REQUEST_TITLE` | PR/MR title |

  Example routing on the target branch:
  ```yaml
  rules:
    - if: '$CI_MERGE_REQUEST_TARGET_BRANCH == "main"'
  ```
- **`triggered_by`** is the PR author (GitHub `pull_request.user.login`,
  Bitbucket `actor.nickname`/`display_name`).
- **Auto-cancel of superseded PR pipelines.** Because PR pipelines compile at the
  head-branch ref, the existing repo+ref auto-cancel applies: a new
  `synchronize`/`:updated` (or a push to the same branch) supersedes the older,
  still-running PR pipeline unless the YAML sets `auto_cancel: false`.
- **Commit status on the PR head.** The status writer posts to the pipeline's
  SHA, which for a PR pipeline **is** the PR head sha — so the pending/success
  check appears directly on the PR with no extra configuration (see
  [Commit status write-back](#commit-status-write-back)).
- **Dedup and HMAC are unchanged.** GitHub PR deliveries are deduped by
  `X-GitHub-Delivery` and still HMAC-verified (`X-Hub-Signature-256`) when
  `WEBHOOK_SECRET` is set; Bitbucket by `X-Request-UUID`.

## Commit status write-back

Once a repo is connected **with a token or a GitHub App**, Forge posts the
pipeline's status back to the origin VCS as the pipeline progresses, so the
commit (and any PR built on it) shows Forge's result — the green tick / red X.
For App-authed repos the same short-lived installation token used for cloning is
reused for the status post (the App needs *Commit statuses: write*).

- **What it posts:** on the commit SHA the pipeline ran, under the fixed context
  `forge-ci`, with a `target_url` linking to the Forge pipeline page
  (`FRONTEND_URL` — falling back to `EXTERNAL_URL` — `+ /pipelines/{id}`).
- **When:** on pipeline creation (queued), when work starts, and on every
  overall-status transition through to the terminal result. Posting is
  idempotent — each `(pipeline, status)` is posted at most once (a small
  `pipeline_status_posts` dedup table backs this), so the 1s scheduler tick
  never spams the provider. Delivery is asynchronous with bounded, backed-off
  retries, so a slow or broken VCS API never blocks the scheduler.
- **Status mapping:**

  | Forge pipeline phase | GitHub state | Bitbucket state |
  |---|---|---|
  | pending / running / blocked | `pending` | `INPROGRESS` |
  | success | `success` | `SUCCESSFUL` |
  | failed | `failure` | `FAILED` |
  | canceled | `error` | `STOPPED` |

  (`canceled` → GitHub `error` rather than `failure`: the run did not complete,
  but it wasn't a test failure.)
- **APIs used:**
  - GitHub — `POST /repos/{owner}/{repo}/statuses/{sha}`.
  - Bitbucket — `POST /2.0/repositories/{workspace}/{repo}/commit/{sha}/statuses/build`.

  Both bases are overridable via `GITHUB_API_BASE` / `BITBUCKET_API_BASE`
  (used for testing against a stub; leave unset in production).
- **Required token scope:**
  - GitHub — a token with **`repo:status`** (classic PAT) or the fine-grained
    **"Commit statuses: write"** permission. The same connection token used for
    cloning is reused; a public-repo clone token without this scope will clone
    fine but get a `403` on status write (logged with an actionable message —
    the token is never logged).
  - Bitbucket — a token / app password with **`repositories:write`**.
- **Skipped silently** (logged at debug, nothing posted) when the repo has **no
  token and no GitHub App** or **provider `other`** (not every git host has a
  status API).
- **Disable:** set `COMMIT_STATUS=off` on `forge-server` to turn write-back off
  globally. It is on by default and only ever acts on repos that have a token.

> Real write-back requires a token with the commit-status scope above. The
> demo/public connections (`Priytam/statemachine`, `octocat/Hello-World`) have
> no token, so nothing is posted for them.
