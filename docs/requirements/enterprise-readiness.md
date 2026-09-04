# Requirement: enterprise-readiness gaps (for org-wide / production use)

**Status:** proposed · **Priority:** P1–P2 · needed before Forge holds
production pipelines company-wide and passes a security review. None blocks a
single-team pilot.

## P1 — likely security-review blockers

### RBAC: roles above login
Today any SSO-authenticated user reaches the admin pages; repo-role checks apply
only to approvals (`docs/sso.md`, "What this is not yet"). Add real
authorization tiers — who may edit pipeline config, manage runners/tokens,
change protected environments and repo settings — distinct from "is logged in".
Admin is currently only `ADMIN_EMAILS`; that does not scale to an org.

### SCIM provisioning / de-provisioning
No SCIM. A departing employee must be removed by hand. For company-wide SSO this
is usually a hard audit requirement. Add SCIM 2.0 user/group provisioning against
the IdP (Entra), or at minimum an automated de-provisioning path.

### Secret management at scale
Single `FORGE_SECRET_KEY`, envelope AES-GCM, no rotation, no external backend.
For production secrets across many repos, add a **KMS-backed** cipher option and
a key-rotation procedure (re-encrypt on rotate). Vault backend optional.

## P2 — hardening

- **Session idle timeout** — sessions last 12h with no idle expiry; add an
  idle/absolute timeout configurable per deployment.
- **Child / multi-project pipelines** — needed if a single pipeline must span
  the backend+frontend sub-projects of a monorepo, or trigger across repos.
  Confirm demand before building.

## Not required (already supported)
Branch→environment rules (`only/except` + `rules:` + `environment:` +
protected envs), per-environment secrets (`environment_scope`), pull-based
tag-routed runners per VPC, OIDC keyless AWS auth, MS Entra login (OIDC),
security-scan templates, environments board with rollback/freeze/drift.
