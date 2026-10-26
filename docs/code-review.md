# Code review in Forge

Forge shows every open pull/merge request ("change") from every connected
repo in one inbox, whichever host it lives on, so people don't need to open
GitHub, GitLab, Bitbucket, Gitea, CodeCommit or a plain git server to see
what is waiting. It is built so that hosting it, and the provider APIs it
uses, cost next to nothing **by construction**:

- **Pages never call a provider.** The inbox and change pages read Postgres
  only. Any provider call attempted while serving a page is refused and
  counted (`forge_provider_read_path_calls_24h`, which must be 0).
- **Webhooks, not polling.** A webhook updates the change at once from its
  payload and asks for one refresh of that change. Bursts within 2 seconds
  collapse into a single refresh.
- **One call per refresh.** On GitHub a refresh is one GraphQL query (the
  change and its reviews, about 1 point of the 5,000/hour budget).
- **The only scheduled traffic is free when nothing changed.** Every 15
  minutes, and only for repos with open changes (every 6 hours otherwise),
  Forge sends one *conditional* list request per repo. GitHub answers `304
  Not Modified`, which does not count against the rate limit.
- **A shared budget per credential.** Every call takes a token from a bucket
  kept in Postgres (no Redis), one bucket per GitHub App installation, token
  or IAM role, overwritten by the host's own `X-RateLimit-*` headers. When a
  bucket runs low, the lowest-priority work waits first: backfill stops at
  50% left, reconcile at 20%, webhook refreshes at 5%. User actions never wait.
- **No new service, database or VM.** The refresh worker runs inside
  `forge-server`; the schema change is additive.

Turn it all off with `CODE_REVIEW=off` on `forge-server`. Pages then show
what is already stored and nothing is fetched.

## What ships today (phase A)

| | |
|---|---|
| **Changes** inbox (sidebar → Changes) | open / merged / closed / all, filter by repo, search by title or `#number`; CI status of the head commit, approvals and "changes requested" per change; live updates |
| Change page | state, branches, description, labels, the Forge pipeline on the head commit, every review, every **revision** (each push is one, on every host) |
| Hosts | **GitHub** (App or token) and **plain git** (Forge-native changes; creating them in the UI comes in phase F) |
| About page | provider calls, 304 share, calls per open change, read-path calls, refresh backlog, every budget |

Coming next, in order: approve and merge from Forge with Forge's rules
enforced on the host (B), diffs (C), line comments synced both ways (D),
GitLab / Bitbucket / Gitea / CodeCommit (E), and Forge-native changes, code
owners and a merge queue (F).

## Setting it up

### GitHub

Code review uses the connection the repo already has (Repos → Add repository).

1. **Credential.** A GitHub App (recommended — see
   [vcs-integration.md](vcs-integration.md#github-app-authentication)) or a
   token. GitHub's GraphQL API needs one, so a repo connected anonymously
   cannot be refreshed (its webhooks still show in the inbox).
   - App permissions: **Pull requests: Read** (and the existing
     *Contents: Read*, *Commit statuses: Read & write*).
   - Fine-grained token: the same. Classic token: `repo`.
2. **Webhook events.** On the repo's (or the App's) webhook, also tick
   **Pull requests** and **Pull request reviews**. (Push events keep driving
   pipelines as before.) Same payload URL:
   `https://<forge-host>/api/v1/webhooks/github`.
3. That's all. Within a minute of the server starting, the first reconcile
   imports the repo's open pull requests (a one-off "backfill"); from then on
   webhooks keep them current.

A pull request webhook updates the change even when the repo has no
pipeline config — code review does not depend on CI.

### Plain git (any git server)

A git server has no pull requests, so changes on it are **Forge-native**:
Forge stores them and re-reads the two branch tips with a single
`git ls-remote` (no API). Creating them from the UI arrives with phase F;
the plumbing — model, refresh, revisions — is already in place and covered by
the same conformance suite as GitHub.

### GitLab, Bitbucket, Gitea, CodeCommit

Phase E. Until then their changes don't appear in the inbox; their pipelines
are unaffected.

## How a change stays current

```
webhook ──► apply payload (page updates now) ──► refresh requested (due in 2 s)
                                                    │ more events? ride along
                                                    ▼
                                   refresh worker (forge-server, 1 s tick on Postgres)
                                     claims with a lease (no two replicas take it)
                                     takes a token from the credential's budget
                                     one GetChange ──► store change, revision, reviews
                                                    ──► NOTIFY ──► open pages refetch
reconcile (per repo, 15 min / 6 h) ──► conditional list ──► 304: done
                                                         ──► 200: refresh only what differs
```

- Webhooks that arrive out of order never overwrite a newer copy.
- A change closed or merged while its webhook was lost is repaired by the
  next reconcile.
- When the host rate-limits Forge (`429`, or `403` with an exhausted limit),
  the bucket is emptied until the host's reset and the refresh is retried
  then. A permanent error (revoked token, missing permission) is not retried
  on a timer: the change page shows "Last sync with the host failed" with the
  last good copy, and the next webhook or reconcile tries again. Transient
  errors back off (10 s doubling, capped at 30 min) and give up after 8 tries.

## Metrics

On **Admin → About & maintenance** ("Code review: provider API use") and in
Prometheus format at `GET /api/v1/metrics`. All of them are read from
Postgres, so every replica reports the same numbers and they survive scale to
zero; they cover the last 24 hours (gauges).

| Metric | Reading it |
|---|---|
| `forge_provider_calls_24h{provider,class,result}` | every provider call. `class` is `user`, `webhook`, `reconcile` or `backfill`; `result` is `ok`, `not_modified`, `error`, `rate_limited` |
| `forge_provider_read_path_calls_24h` | calls attempted while serving a page. **Must be 0**; anything else is a bug (the call was refused, so it cost nothing — but alert on it) |
| `forge_provider_not_modified_ratio_24h` | share of calls answered 304. On a quiet day this is most reconciles |
| `forge_provider_calls_per_open_change_24h` | the footprint per change; expect a handful (one per webhook burst plus reconciles) |
| `forge_provider_budget_remaining{credential,provider}` / `forge_provider_budget_capacity` | what is left of each credential's window. `credential` is `github-app:<installation id>`, `github:token:<fingerprint>` (never the token) or `<provider>:anon:<host>` |
| `forge_changes_open` | open changes across all repos |
| `forge_change_refresh_backlog` / `forge_change_refresh_oldest_seconds` | refreshes due now and the oldest one's age. A backlog that keeps growing means a budget is exhausted or a host is down |

At Travafa scale (≈15 repos, ≈40 open changes, ≈300 change events a day)
expect ≈300 refreshes plus ≈1,400 reconciles a day, most of them 304s: well
under 1% of one GitHub installation's hourly budget.

## API

All read routes are served from Postgres with a read-path guard; JSON
responses carry an `ETag` and answer `304` to `If-None-Match`.

- `GET /api/v1/changes?state=open|merged|closed|all&repo=&author=&q=&limit=&offset=`
  — the inbox (`X-Total-Count`, `X-Has-More` headers).
- `GET /api/v1/changes/{id}` — `{change, capabilities}`: the change with
  `revision_list` and `reviews`, and what its host supports natively.
- `GET /api/v1/changes/stream` — Server-Sent Events: one `change` event
  (`{"id": N}`) per update.
- `GET /api/v1/code-review/metrics` — the About page numbers.
- `GET /api/v1/code-review/capabilities` — capabilities per provider.

## For plugin authors

A host plugs in by implementing `vcs.ChangeSource` (`internal/vcs/review.go`)
and declaring `Capabilities`; it makes HTTP calls only through
`vcs.ChangeClient`, which enforces the read-path guard and the accounting.
A provider is done when it passes the shared conformance suite
(`internal/vcs/vcstest`, run by `internal/vcs/conformance_test.go`): event
normalisation and idempotent replay, get-change after push / review / close,
unknown changes, conditional listing, read-path refusal and rate-limit
headers. The suite branches on capabilities, never on the provider's name,
and fails if a plugin implements `ChangeSource` without a harness.
