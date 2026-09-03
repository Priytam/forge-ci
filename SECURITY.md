# Security Policy

## Reporting a vulnerability

**Please do not open a public issue, pull request or discussion for a security
vulnerability.**

Report it privately instead, either way:

1. **GitHub private vulnerability reporting** — the repository's **Security**
   tab → *Report a vulnerability*. Preferred: it keeps the report, the fix and
   the advisory in one place.
2. **Email** — mrpjpandey@gmail.com, with `SECURITY` in the subject.

Please include the affected version or commit, a description of the impact, and
the smallest reproduction you can manage. If you have a suggested fix, say so —
but a clear report on its own is genuinely valuable.

You will get an acknowledgement within **5 business days**. This is a
small project without a paid security team, so please size your expectations
accordingly; a fix timeline will be agreed with you once the issue is
understood. Credit is given in the advisory unless you would rather not be
named.

## Supported versions

Forge CI is pre-1.0. Only the `main` branch receives security fixes. There are
no backports to older tags.

## Deployment defaults you must change

Several defaults deliberately favour a frictionless local experience and are
**not safe for a shared or production deployment**. These are documented
behaviour, not vulnerabilities — but they are the difference between a demo and
a real installation.

| Setting | Default | Why it matters |
|---|---|---|
| `RUNNER_AUTH` | `off` | The runner protocol and artifact upload are **unauthenticated**. Anyone who can reach the server can claim jobs, post logs, and upload artifacts. Set `RUNNER_AUTH=on` and issue tokens via `POST /api/v1/runner-tokens`. |
| `FORGE_SECRET_KEY` | unset | Without it, CI variables, VCS tokens, SSO client secrets and the OIDC signing key are stored **in plaintext** in Postgres. Set it to a base64 32-byte key; existing plaintext rows are re-encrypted on startup. |
| SSO | open mode | With no provider enabled there is **no authorization at all** — every caller is treated as a platform admin. Enable a provider, and set `ADMIN_EMAILS` *before* you do. |
| `EXTERNAL_URL` / `FRONTEND_URL` | localhost | These gate CSRF checks and OAuth redirects. Set both to your real HTTPS origins. |
| Executor | `shell` | The shell executor runs pipeline scripts **directly on the runner host** with the runner's privileges. Untrusted pipelines need the `docker` executor and isolated, disposable runners. |

## Hardening checklist

- [ ] `FORGE_SECRET_KEY` set, and backed up — losing it makes encrypted rows unrecoverable
- [ ] `ADMIN_EMAILS` populated **before** enabling any SSO provider
- [ ] An SSO provider enabled, with `allowed_domain` restricted to your organisation
- [ ] `RUNNER_AUTH=on`, with per-runner tokens, rotated on runner decommission
- [ ] `EXTERNAL_URL` and `FRONTEND_URL` set to real HTTPS origins
- [ ] Runners isolated from the control plane and from each other; `docker` executor for untrusted code
- [ ] Postgres reachable only from the control plane, with backups and encryption at rest
- [ ] `RETENTION_DAYS` set deliberately — it prunes the audit log too
- [ ] Protected environments configured server-side, so pipeline YAML cannot weaken its own approval gate
- [ ] SSO client secret expiry tracked; it will break sign-in for everyone when it lapses

## Known gaps

Being explicit, since these come up in security review. Forge CI currently has
**no SAML**, **no SCIM provisioning** (so de-provisioning a departing user is
manual), **no roles above login** (any signed-in user can reach admin pages,
though repo-role checks still apply to approvals), **no session idle timeout**
(sessions last 12 hours), and **a single encryption key** with no rotation or
external KMS/Vault integration. See `docs/feature-comparison.md` for the roadmap.
