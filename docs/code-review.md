# Code review in Forge

Forge shows every open pull/merge request ("change") from every connected
repo in one inbox, whichever host it lives on, lets people approve and merge
there, and puts Forge's own rules between a change and its merge — enforced
on the host too, so clicking Merge on GitHub waits for them. Nobody needs to
open GitHub, GitLab, Bitbucket, Gitea, CodeCommit or a plain git server to
see, approve or merge what is waiting. It is built so that hosting it, and
the provider APIs it uses, cost next to nothing **by construction**:

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
- **Every write is queued.** Publishing the gate and merging go through an
  outbox in Postgres: saved at once, sent by the worker under the budget,
  retried with backoff, never duplicated (idempotency keys; a newer gate
  supersedes an unsent older one; a merge retry first checks whether the
  last attempt already merged).
- **No new service, database or VM.** The refresh and outbox workers run
  inside `forge-server`; the schema change is additive.

Turn it all off with `CODE_REVIEW=off` on `forge-server`. Pages then show
what is already stored and nothing is fetched.

## What ships today (phases A, B and C)

| | |
|---|---|
| **Changes** inbox (sidebar → Changes) | open / merged / closed / all, filter by repo, search by title or `#number`; CI status of the head commit, the **merge gate** (ready / waiting / blocked), approvals and "changes requested" per change; live updates |
| **Files changed** (change page) | every file's diff, unified or side by side; the whole change against its target, or **changes since revision N**; "changes since you last looked"; lock files, vendored and other generated files, binaries and very large diffs collapsed until opened |
| Change page | the merge gate rule by rule, **Approve / Request changes / Withdraw**, **Merge** (merge / squash / rebase as the host allows), state, branches, description, labels, the Forge pipeline on the head commit, every review, every **revision** (each push is one, on every host) |
| **Merge rules** (Admin → Merge rules) | rules per repo and target branch, and which host login is which Forge user |
| Hosts | **GitHub** (App or token) and **plain git** (Forge-native changes; creating and merging them in the UI comes in phase F) |
| About page | provider calls, 304 share, calls per open change, read-path calls, refresh and write backlogs, gates and what blocks them, every budget |

Coming next, in order: line comments synced both ways and
reviews posted as the reviewer (D), GitLab / Bitbucket / Gitea / CodeCommit
(E), and Forge-native changes, code owners and a merge queue (F).

## Diffs

Forge never asks a host's API for a diff. Every diff comes from git:

- **A mirror on a runner.** Runners keep one blobless partial mirror per repo
  (`git` with `--filter=blob:none`) and fetch into it **only when a diff names
  a commit the mirror lacks** — never on a timer. File contents arrive lazily,
  only for files the diff touches. A repo's diffs go to the runner that holds
  its mirror (affinity), so the mirror stays warm. See
  [runners.md](runners.md#git-mirrors-for-code-review-diffs-runner-tasks).
- **Computed once, warm before anyone looks.** When a webhook moves a change's
  head or target, Forge queues its diff at once; a runner computes it in well
  under a second. The pull request's diff starts at the merge base with the
  target (what the change itself did), like on the host; "since revision N"
  diffs the two pushes directly and is computed the first time someone asks.
- **Stored by commit, served forever.** A diff's bytes live in the existing
  artifact blob store under a key made of commit shas (they can never change),
  as a small manifest plus a separate chunk for each large or generated file.
  The browser keeps them for good (`Cache-Control: private, max-age=31536000,
  immutable` — private because a diff of a private repo must never sit in a
  shared cache), and the server keeps recently served ones in memory. Opening
  a diff again costs a 304 or nothing.
- **Retention.** Diffs nobody has opened for 30 days and that no open change
  shows are deleted; opening one again recomputes it.

Hosts: GitHub (via `refs/pull/<n>/head`, so forks and deleted branches work)
and plain git. The others arrive with their plugins (phase E).

If a runner cannot compute a diff (the commits were force-pushed away, the
repo cannot be reached), the page says why and offers **Try again**.

## Merge rules and the gate

A rule applies to the changes of one repo (or every repo) whose **target
branch** matches its pattern (`main`, `release/*`). A repo's own rule beats a
rule for every repo, an exact branch beats a pattern, a longer pattern beats
a shorter one. **A change no rule matches has no gate**: nothing is
published to its host, so adding Forge never changes a repo's checks until
you write a rule for it.

| Rule | Passes when | Otherwise |
|---|---|---|
| Approvals | N people approve (each person once, their latest verdict) | waiting |
| Approval from a group | at least M of them are in the approver group | waiting |
| No changes requested | nobody's latest verdict is "request changes" | **blocked** |
| CI | the newest Forge pipeline on the head commit passed, and every *required job* in it | waiting while it runs, **blocked** when it failed |
| Labels | every required label is there and no blocking one (`do-not-merge`) is | waiting / **blocked** |
| Linked ticket | the regular expression matches the title, description or branch | **blocked** |
| Merge window | now is outside every weekly freeze window (`fri 18:00 - mon 09:00`, in the rule's timezone) | waiting until the window ends |
| Draft | the change is not a draft | waiting |

Who counts:

- **Approvers** need one of the rule's repo roles (default admin, owner,
  developer) or membership of its approver group. A repo with no members
  runs in bootstrap mode: anyone counts (as for deploy approvals).
- **Reviews given on the host** count once their login is linked to a Forge
  user (Admin → Merge rules → Host logins), which is also how "no
  self-approval" and approver groups recognise people across hosts. The gate
  says which approvals it did not count and why (own change, earlier
  revision, not linked, no approver role). Linking by OAuth comes in phase D.
- **Re-approve after push** (on by default): an approval made on an earlier
  head does not count.

The gate is re-evaluated only when an input moves — a webhook, a review, a CI
job changing state, a rule or login edit — or when a freeze window opens or
closes. A changed verdict is queued for publishing as the commit status
**`forge/merge-gate`** on the head commit (success / pending / failure, with
the reason and a link to the change in Forge).

### Enforce it on GitHub

1. In the repo: **Settings → Branches → Branch protection rule** for the
   target branch (or a ruleset): **Require status checks to pass** and add
   **`forge/merge-gate`** (it appears in the list once Forge has published it
   on any commit).
2. From then on GitHub's own Merge button stays disabled until the gate
   passes — Forge's rules hold even for people who never open Forge.
3. Forge does not change branch protection itself (that would need the
   *Administration* permission); an admin sets it once.

GitHub App permissions for phase B: **Commit statuses: Read & write** (the
gate), **Pull requests: Read & write** and **Contents: Read & write** (merging
from Forge). Without the write permissions the gate still works; Merge from
Forge then fails with GitHub's reason on the change page.

### Approve and merge from Forge

- **Approve / Request changes / Withdraw** record a verdict in Forge on the
  current head, at once. They count in Forge's gate; they are not posted to
  GitHub as reviews yet (phase D posts them as the reviewer).
- **Merge** needs a repo role (as for retrying pipelines) and a passing gate.
  It merges **exactly the head you were looking at**: if someone pushed
  since, Forge refuses (and GitHub would too). The merge is queued and sent
  once the passing gate has reached the host; the page shows "Merging…",
  then the merged change — or the host's reason if it refused.
- Removing a rule that governed a change publishes a passing gate ("No Forge
  merge rules apply") so the host is never left blocked.

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
| `forge_provider_outbox_backlog` / `forge_provider_outbox_oldest_seconds` | gate publishes and merges waiting to be sent. Should drain within seconds; an old item means the host refuses writes (check the change page's action error) |
| `forge_gate_evaluations_24h` | gate verdicts that changed |
| `forge_diff_cache_hit_ratio_24h` | share of diff opens that found the diff ready (target > 0.99) |
| `forge_diff_reads_24h{result}` | diff opens: `hit`, `miss` (queued then), `pending` (still computing), `failed` |
| `forge_diff_open_p95_ms` | how long opening a diff took on this server, p95 of the last 1,000 (target < 300) |
| `forge_diff_compute_p95_ms` / `forge_diffs_computed_24h` | runner compute time and count |
| `forge_mirror_fetches_24h` | diffs that needed a git fetch into a mirror (git protocol, not an API call; there are no provider diff calls at all) |
| `forge_runner_task_backlog` | diffs waiting for or running on a runner; growing means no runner can take tasks (git < 2.27, `RUNNER_TASKS=off`, all paused) |
| `forge_diff_stored_bytes` | diff bytes in the blob store |
| `forge_gate_blocked_changes{rule}` | open changes each rule is holding right now (`approvals`, `ci`, `freeze`, …) |

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
- `GET /api/v1/merge-policies`, `GET /api/v1/host-identities` — rules and
  login links (the latter admin-only).
- `GET /api/v1/changes/{id}/diff[?from=<rev sha>&to=<rev sha>][&retry=1]` —
  the diff's status (`ready` with `manifest_url`, `pending`, `failed`,
  `unsupported`), queueing it when it was never computed.
- `GET /api/v1/diff-blobs/<repo key>/<mode>/<from>..<to>/manifest.json` and
  `.../f/<n>.patch` — the immutable manifest and chunks.

Runner protocol (runner token): `POST /api/v1/runner/tasks/acquire`
(long-poll, `{runner_id, kinds}`) and `POST /api/v1/runner/tasks/{id}/complete`.

The change page's `GET /api/v1/changes/{id}` also returns `gate` (state,
summary, `rules[]`, what the host shows), `actions` (queued and finished
writes) and `permissions` (`can_review`, `can_merge`).

Writes (session or API token; CSRF rules as everywhere):

- `POST /api/v1/changes/{id}/reviews` `{verdict: approve|request_changes|dismiss, body}`
- `POST /api/v1/changes/{id}/merge` `{expected_head, method}` → `202 {action}`
  (`409` when the head moved, the gate does not pass, or the change is a draft)
- `PUT /api/v1/merge-policies` (admin) — create or replace the rule for
  `(repo, branch_pattern)`; omitted booleans default to CI required,
  re-approve after push, changes requested block. `DELETE /api/v1/merge-policies/{id}`.
- `PUT /api/v1/host-identities` `{provider, login, email}` (admin),
  `DELETE /api/v1/host-identities/{provider}/{login}`.

## For plugin authors

A host plugs in by implementing `vcs.ChangeSource` (`internal/vcs/review.go`)
and declaring `Capabilities`; it makes HTTP calls only through
`vcs.ChangeClient`, which enforces the read-path guard and the accounting.
Writes implement `vcs.ChangeWriter` (`PublishCheck`, `Merge` with an
expected head). A provider is done when it passes the shared conformance
suite (`internal/vcs/vcstest`, run by `internal/vcs/conformance_test.go`):
event normalisation and idempotent replay, get-change after push / review /
close, unknown changes, conditional listing, read-path refusal, rate-limit
headers, the published gate blocking a merge on the host until it passes,
and merge-with-expected-head. The suite branches on capabilities, never on the provider's name,
and fails if a plugin implements `ChangeSource` without a harness.
