# Sign-in: any OpenID Connect IdP, LDAP / Active Directory, Google, Microsoft, GitHub

Forge does not assume an identity provider. Pick what your company runs:

| Provider | Use it for |
|---|---|
| **OpenID Connect** (`oidc`) | any OIDC IdP — Okta, Keycloak, Auth0, Entra ID, Authentik, Dex, Zitadel, PingFederate, Google Workspace, JumpCloud… Group claims map to roles. |
| **LDAP / Active Directory** (`ldap`) | a directory with username + password; group membership maps to roles |
| Google, Microsoft, GitHub | one-click buttons when that is all you need (no groups) |
| SAML | not built in yet — put a SAML IdP behind an OIDC bridge (Keycloak, Dex, Authentik) today; see [design/saml.md](design/saml.md) |

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
  `ADMIN_EMAILS="alice@acme.io,bob@acme.io"`, **or** belong to an IdP group
  mapped to *platform admin* (below). Non-admins get `403`; unauthenticated
  callers get `401`.

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

## OpenID Connect — any identity provider

1. In your IdP, create an OIDC **web application** (confidential client,
   authorization-code flow). Redirect URI: shown on Forge's OpenID Connect
   form — `https://<forge-host>/api/v1/auth/callback/oidc`.
2. On **Admin → Sign-in & identity → OpenID Connect**: the **issuer URL**
   (Forge reads `<issuer>/.well-known/openid-configuration`), client ID and
   secret, and a **button label** ("Okta", "Company login").
3. For groups, request them: add your IdP's group scope to **Scopes**
   (`openid email profile groups` for most) and set **Groups claim** if it
   isn't `groups` — dotted paths work (`realm_access.roles` for Keycloak
   realm roles).

Forge uses the authorization-code flow with **PKCE** and a **nonce**, checks
the ID token's issuer, audience, expiry and nonce, and reads email, name and
groups from the ID token and the userinfo endpoint. An `email_verified: false`
claim is refused. The issuer must be `https://` (a loopback `http://` issuer is
allowed for a local IdP). Typical issuers:

| IdP | Issuer |
|---|---|
| Okta | `https://<org>.okta.com/oauth2/default` |
| Keycloak | `https://<host>/realms/<realm>` |
| Auth0 | `https://<tenant>.auth0.com/` |
| Entra ID | `https://login.microsoftonline.com/<tenant-id>/v2.0` (groups claim: `groups`, emit group names or IDs) |
| Authentik | `https://<host>/application/o/<slug>/` |
| Google Workspace | `https://accounts.google.com` (no groups claim) |

## LDAP / Active Directory

Users sign in on the Forge login page with their directory username and
password. On **Admin → Sign-in & identity → LDAP**:

| Setting | Example | Notes |
|---|---|---|
| Server URL | `ldaps://ldap.acme.io:636` | `ldap://` needs **StartTLS** — plaintext is refused unless `FORGE_LDAP_ALLOW_PLAINTEXT=true` (lab only) |
| Bind DN / password | `cn=forge,ou=services,dc=acme,dc=io` | read-only service account used to find the user |
| User search base | `ou=people,dc=acme,dc=io` | |
| User filter | `(|(uid={username})(sAMAccountName={username})(mail={username}))` | default; `{username}` is escaped |
| Email / name attribute | `mail` / `displayName` | email is required |
| Group attribute | `memberOf` | AD and OpenLDAP with the memberof overlay; group DNs become their CN |
| Group search base | `ou=groups,dc=acme,dc=io` | for directories without memberOf: finds groups listing the user (`member`, `uniqueMember`, `memberUid`) |

Sign-in: bind as the service account, find exactly one user, bind **as the
user** with their password, read email, name and groups. Every credential
problem returns the same "invalid username or password". Empty passwords are
refused (an anonymous bind must never sign anyone in), and five failures in
five minutes per username or client address lock sign-in for a while.

## Group → role mappings

**Admin → Sign-in & identity → Group → role mappings** turns IdP groups into
Forge access:

- **platform admin** — like being in `ADMIN_EMAILS`;
- **a role on repos** — `developer`, `owner` or `admin` on every repo
  matching a pattern: `*`, `acme/*` (one namespace level), `acme/**` (any
  depth), or one repo.

Groups are refreshed at **every sign-in** (an empty list too), so leaving a
group in the IdP removes its roles from the next sign-in on; removing a
mapping takes effect at once. A group-granted role counts like a repo
membership everywhere — running and canceling jobs, **approving deploys**,
and approval notifications — and the highest of a user's direct and group
roles wins. A repo matched by any mapping is no longer in open "no members
yet" mode. `GET /api/v1/identity/me/groups` shows a user the groups Forge has
for them.

## Session lifetime

New sign-ins last **12 hours** by default. Change it on the admin page
(1 hour to 30 days) or with `SESSION_TTL` (a Go duration, e.g. `72h`); the
admin setting wins.

## Operational notes

- **Lockout escape hatch**: enabling a provider with bad credentials locks the
  UI (config API included — by design). Disable via SQL and retry:
  `UPDATE sso_providers SET enabled=false;` (takes effect within ~5s).
- Sessions last 12h unless changed (above), are HttpOnly cookies backed by
  server-side rows, and can be revoked by deleting from the `sessions` table.
- Secrets are write-only through the API (reads return `has_secret`). Client
  secrets are encrypted at rest with AES-256-GCM when `FORGE_SECRET_KEY` is set
  (marked by an `enc:v1:` prefix; any plaintext rows are re-encrypted on every
  start). **Without that key they are stored in plaintext** — same caveat as CI
  variables. See the "Secrets at rest" note in the README.
- What this is not yet: SAML (design in [design/saml.md](design/saml.md)),
  SCIM provisioning, a session-idle timeout.
