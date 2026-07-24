# SSO: Google, Microsoft (Entra ID), GitHub

Forge starts in **open mode** (no login). The moment any provider is enabled
in Admin → SSO, the whole API and UI require a signed-in session — except the
runner protocol, webhooks, and the auth endpoints themselves, which have
their own authentication. Once signed in, **approvals use the authenticated
email**: the server ignores any client-supplied approver name, so RBAC
membership should use work emails as usernames.

Each provider is configured on its own form in **Admin → SSO** inside the
deployed tool. Every form shows the exact **Redirect URI** to paste into the
provider console — it's derived from the server's `EXTERNAL_URL` env var
(set that to your real https URL in production; also set `FRONTEND_URL` to
where the dashboard lives so post-login redirects land correctly).

## Platform administrators (authorization)

Read endpoints are open to any authenticated user; **mutating admin endpoints**
(SSO config, variables, members, protected environments, repo registry, repo
configs, repo settings, runner tokens, runner pause) require a *platform
admin*. Admin is determined by:

- **Open mode** (no SSO provider enabled): every caller is treated as admin
  (bootstrap — there is no authz until you enable SSO).
- **Enforced mode** (SSO enabled): the caller must have a session whose email
  is listed in **`ADMIN_EMAILS`** (comma-separated), e.g.
  `ADMIN_EMAILS="alice@meesho.com,bob@meesho.com"`. Non-admins get `403`;
  unauthenticated callers get `401`.

`GET /api/v1/auth/me` returns an `is_admin` boolean so the UI can hide
admin-only navigation.

> Footgun: enabling SSO with an empty `ADMIN_EMAILS` locks everyone out of
> admin mutations. Set `ADMIN_EMAILS` **before** enabling a provider.

## Identity from the session (no client-asserted identity when enforced)

When SSO is enforced, the server derives the acting identity from the
authenticated session — it never trusts an actor/author/approver value in the
request body:

- **Approvals** (`POST /jobs/{id}/approvals`): the approver is the session
  email; a body `approver` is ignored. With enforcement on, a session is
  guaranteed (the request is otherwise `401`).
- **Config version author** (`PUT /repo-configs`, `POST /repo-configs/revert`):
  the recorded `author` is the session email; a body `author` is ignored.

In **open bootstrap mode** (no SSO enabled) there is no session, so a
client-supplied approver/author is still accepted — this keeps CLI/dev bootstrap
working. Mutating admin endpoints additionally require `requireAdmin`; with SSO
enforced that means a session whose email is in `ADMIN_EMAILS`, and every
mutating admin route is gated (unauthenticated → `401`, non-admin → `403`).

## Audit log

Every mutating admin/settings action is recorded in the append-only `audit_log`
table, plus a `denied` entry whenever an admin route returns `401`/`403`.

- **Read API**: `GET /api/v1/audit-log?limit=&offset=&repo=&actor=`
  (admin-only, newest first, paginated; `limit` defaults to 50, capped at 200).
- **Recorded fields**: `actor` (session email, or `bootstrap`/`anonymous`),
  `action` (e.g. `variable.create`, `sso.upsert`, `member.add`,
  `repo-config.push`, `runner-token.revoke`, `approval.vote`,
  `admin.denied`), `target`, `repo`, `detail` (safe metadata only), `source_ip`
  (`X-Forwarded-For` first hop, else `RemoteAddr`), and `result`
  (`ok`/`denied`/`error`).
- **Never logged**: secret values, tokens, client secrets or variable values —
  `detail` carries only flags/names (e.g. `masked`, `has_token`, `token_suffix`).
- **Retention**: rows are append-only in normal operation; the scheduler's
  retention sweep prunes rows older than `RETENTION_DAYS` (default 30; `0`
  disables pipeline/artifact/audit pruning).

## CSRF protection

Cookie-authenticated state-changing requests (POST/PUT/DELETE carrying the
`forge_session` cookie) must be same-origin: the `Origin` header (or `Referer`
fallback) must match `EXTERNAL_URL` or `FRONTEND_URL`, otherwise the request is
rejected with `403`. Safe methods (GET/HEAD), the runner protocol, webhooks,
and bearer-token (runner) requests are exempt. This is defense-in-depth on top
of the cookie's `SameSite=Lax`.

## Google — step by step

1. Open https://console.cloud.google.com → select/create a project.
2. **APIs & Services → OAuth consent screen**: user type *Internal* (Workspace
   org) or *External*; fill app name and contacts; scopes `openid`, `email`,
   `profile` (non-sensitive, no verification needed).
3. **APIs & Services → Credentials → Create credentials → OAuth client ID**:
   - Application type: **Web application**
   - Authorized redirect URI: paste the Redirect URI shown on Forge's Google
     form (`https://<forge-host>/api/v1/auth/callback/google`)
4. Copy the **Client ID** and **Client secret** into Forge's Google form.
5. Optional: set **Allowed domain** (e.g. `meesho.com`) — Forge both hints
   Google's account picker (`hd`) and enforces the email domain server-side.
6. Tick **Enabled**, save, then use "Continue with Google" from the login page.

## Microsoft (Entra ID / Azure AD) — step by step

1. Open https://portal.azure.com → **Microsoft Entra ID → App registrations →
   New registration**.
2. Name it (e.g. `Forge CI`); supported account types: *Accounts in this
   organizational directory only* for single-tenant.
3. Redirect URI: platform **Web**, value from Forge's Microsoft form
   (`https://<forge-host>/api/v1/auth/callback/microsoft`).
4. After creation, from **Overview** copy:
   - **Application (client) ID** → Forge's Client ID field
   - **Directory (tenant) ID** → Forge's Tenant field (or leave `common`
     for multi-tenant)
5. **Certificates & secrets → New client secret** → copy the secret **Value**
   (not the ID) into Forge. Note its expiry — rotate before then.
6. API permissions: the default delegated `openid email profile` (Microsoft
   Graph) suffice; grant admin consent if your tenant requires it.
7. Enable and test.

## GitHub — step by step

1. GitHub → **Settings → Developer settings → OAuth Apps → New OAuth App**
   (use an org-owned app under the org's settings for team use).
2. Homepage URL: your Forge URL. **Authorization callback URL**: from Forge's
   GitHub form (`https://<forge-host>/api/v1/auth/callback/github`).
3. Register, then **Generate a new client secret**; copy Client ID + secret
   into Forge's GitHub form.
4. Scopes are requested by Forge automatically (`read:user user:email` — to
   read the verified primary email).
5. Optional Allowed domain restricts sign-ins by email domain server-side.
6. Enable and test.

## Operational notes

- **Lockout escape hatch**: enabling a provider with bad credentials locks the
  UI (config API included — by design). Disable via SQL and retry:
  `UPDATE sso_providers SET enabled=false;` (takes effect within ~5s).
- Sessions last 12h, are HttpOnly cookies backed by server-side rows, and can
  be revoked by deleting from the `sessions` table.
- Secrets are write-only through the API (reads return `has_secret`), stored
  plaintext in Postgres today — same encrypt-at-rest caveat as CI variables.
- What this is not yet: no roles on top of login (any signed-in user can
  reach admin pages — repo-role checks still apply to approvals), no SAML,
  no SCIM provisioning, no session-idle timeout. Listed in the roadmap.
