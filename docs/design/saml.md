# Design: SAML 2.0 sign-in

Status: **design** — not built. Today, put a SAML IdP behind an OIDC bridge
and use Forge's generic OpenID Connect sign-in.

## Why not yet

Every major IdP that speaks SAML also speaks OpenID Connect (Okta, Entra ID,
Ping, OneLogin, JumpCloud, Google Workspace, ADFS 2016+). The companies that
are SAML-only usually run ADFS 2012 or a niche IdP, and already run a broker.
A SAML service provider is also a large security surface (XML signature
wrapping, canonicalization, replay) that deserves a dedicated, audited
library rather than a quick implementation.

## Today: bridge

Run Keycloak, Dex or Authentik as a SAML **service provider** to your IdP and
an OIDC **provider** to Forge. Groups/roles from the SAML assertion become an
OIDC groups claim, so group → role mappings work unchanged.

## When it is built

- **Library:** `github.com/crewjam/saml` (service-provider side), pinned and
  reviewed; no hand-written XML signature code.
- **Config** (`sso_providers` row `saml`, `settings`): IdP metadata URL or
  XML, entity ID (default `<EXTERNAL_URL>/api/v1/auth/saml/metadata`), the
  attribute names for email, name and groups, NameID format.
- **Endpoints:** `GET /api/v1/auth/saml/metadata` (SP metadata for the IdP),
  `GET /api/v1/auth/login/saml` (AuthnRequest, HTTP-Redirect binding),
  `POST /api/v1/auth/saml/acs` (assertion consumer, HTTP-POST binding).
- **Checks:** signed assertion (or signed response) against the IdP
  certificate from metadata; audience = our entity ID; `NotBefore` /
  `NotOnOrAfter` with 60s skew; `InResponseTo` matched to a request we issued
  (stored like the OIDC state, single use) — IdP-initiated sign-in off by
  default; assertion ID replay cache until expiry.
- **Then** the same `finishLogin`: groups recorded, mappings applied, session
  created. No SAML single logout in the first version (Forge sessions are
  short and revocable server-side).
- **Tests:** a test IdP that signs with a throwaway key, plus the known
  signature-wrapping and comment-injection cases.
