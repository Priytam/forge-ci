# GitHub / Bitbucket integration

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

1. **A registered pipeline config** — Forge stores the pipeline YAML per repo
   (it can't read `.forge-ci.yml` out of a repo it doesn't host):
   ```sh
   curl -X PUT $FORGE/api/v1/repo-configs \
     -d "$(jq -n --arg cfg "$(cat forge-ci.yml)" '{repo:"acme/checkout-service", config:$cfg}')"
   ```
   Re-PUT to update; the YAML is validated on save.

2. **A webhook** from the provider — push events, and (GitHub / Bitbucket)
   pull-request events. See [PR/MR-triggered pipelines](#prmr-triggered-pipelines).

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

## Behavior notes

- The ref from the webhook drives `only`/`except`, so pushes to `main` and to
  feature branches compile different DAGs from the same registered YAML.
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
