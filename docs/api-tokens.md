# Personal API tokens

A personal API token lets a script or CI job call the Forge API **on behalf of
the user who created it**. A request with

```sh
curl -H "Authorization: Bearer fci_…" $FORGE/api/v1/pipelines?repo=acme/app
```

is treated exactly like that user's SSO session: same identity (`triggered_by`,
audit `actor`), same admin and repo rights. Tokens only matter when SSO is
enforced; in open mode the API needs no token.

## Scopes

A token can do at most what its owner can; its **scope** can limit that:

| Scope | What it may do |
|---|---|
| `admin` (default) | Everything the owner may do, admin actions included if the owner is an admin |
| `operate` | Everything the owner may do **except admin-only actions**: start, retry, cancel and play pipelines; Live sync, rollback and sync policy (on repos the owner may operate; an admin owner may operate any repo). Live targets, runners admin, settings and rules need `admin` |
| `read` | `GET`/`HEAD` only; any change gets `403 this API token is read-only` |

Pass `"scope"` when creating a token (personal or service account); tokens
made before scopes existed are `admin`. The token list shows each one's scope.

## Scripts and automation

Use a token, not a copied browser cookie: SSO sessions expire (and a
script's `forge_session` cookie with them), tokens last as long as you
choose. Everything an operator script needs takes a token — Live targets
(`POST/PATCH /api/v1/live/targets`), app and bulk sync policy
(`PUT /api/v1/live/apps/{id}/policy`, `POST /api/v1/live/apps/policy`),
runners (`POST /api/v1/runners/{id}/drain|resume|upgrade`), retries
(`POST /api/v1/jobs/{id}/retry`, `POST /api/v1/jobs/retry-failed`) and
pipelines. Only managing tokens, service accounts and platform admins needs a
signed-in browser session, so a leaked token cannot mint more.

When a request carries both an `Authorization: Bearer fci_…` header and a
browser cookie, the token is what counts: an expired cookie riding along no
longer turns the call into a 401 or a CSRF 403.

For shared automation prefer a [service account](#service-accounts) with an
`operate` token; give it `admin` only for scripts that change targets,
runners or settings.

## Creating and revoking

Signed-in users manage their own tokens at **(your email) ▾ → API tokens**
(`/tokens`), or via the API:

| Call | Notes |
|---|---|
| `POST /api/v1/api-tokens {"description","expires_in_days","scope"}` | Default 90 days, scope `admin` |
| `POST /api/v1/api-tokens {"description","no_expiry":true}` | Never expires (if policy allows) |
| `GET /api/v1/api-tokens` | Your tokens: created, last used, expires (`null` = never), status |
| `GET /api/v1/api-tokens?all=1` | Admins: everyone's tokens |
| `POST /api/v1/api-tokens/{id}/revoke` | Owner, or any admin |

Creating a token needs a browser SSO session — a token cannot mint another
token. The raw token is shown once; only its SHA-256 hash is stored.
`last_used_at` is updated at most once a minute.

## Admin policy

`PUT /api/v1/api-token-policy {"allow_no_expiry": bool, "max_lifetime_days": n}`
(admin only; UI: **Admin ▾ → API token policy**). Defaults: non-expiring
tokens allowed, no cap.

The policy is checked on **every request**, not only at creation:

- `allow_no_expiry: false` disables existing never-expiring tokens at once
  (status `disabled`); turning it back on re-enables them.
- `max_lifetime_days: n` rejects new tokens longer than `n` days and disables
  any token older than `n` days.

Revoke a token to stop it permanently.

## Service accounts

For automation that shouldn't depend on one person — a deploy script, a GitHub
Action, a cron job — use a **service account** (Admin ▾ → Service accounts,
`/api/v1/service-accounts`):

- It acts as `svc:<name>` (e.g. `svc:deployer`): that is the identity in the
  audit log, on pipelines it starts, and for repo membership. Grant it repo
  access by adding `svc:deployer` as a member (Repo → Settings → Members), or
  mark it **admin** for platform-wide rights.
- Admins create its tokens (`POST /api/v1/service-accounts/{id}/tokens`
  `{"description", "expires_in_days" | "no_expiry": true}`); the same token
  policy applies as for personal tokens. Tokens are `fci_…` and used the same
  way.
- **Disable** the account and every token stops at once; **delete** removes it
  and its tokens. Individual tokens can be revoked.
- Managing service accounts needs an admin signed in with SSO — a token
  (including an admin service account's) can't create accounts or tokens.
