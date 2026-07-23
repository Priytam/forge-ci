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
