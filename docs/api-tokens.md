# Personal API tokens

A personal API token lets a script or CI job call the Forge API **on behalf of
the user who created it**. A request with

```sh
curl -H "Authorization: Bearer fci_…" $FORGE/api/v1/pipelines?repo=acme/app
```

is treated exactly like that user's SSO session: same identity (`triggered_by`,
audit `actor`), same admin and repo rights. Tokens only matter when SSO is
enforced; in open mode the API needs no token.

## Creating and revoking

Signed-in users manage their own tokens at **(your email) ▾ → API tokens**
(`/tokens`), or via the API:

| Call | Notes |
|---|---|
| `POST /api/v1/api-tokens {"description","expires_in_days"}` | Default 90 days |
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
