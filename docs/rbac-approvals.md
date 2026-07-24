# Roles, membership, and approval rules

## Roles

Membership is per repo, one role per user:

| Role | Intent |
|---|---|
| `admin` | Manages the repo's CI: variables, runners, members, approval rules |
| `owner` | Senior reviewer; typically an allowed production approver |
| `developer` | Runs pipelines; cannot approve protected deployments by default |

Add members (admin action):

```sh
curl -X POST $FORGE/api/v1/members -d '{"repo":"acme/checkout-service","username":"sre-lead","role":"owner"}'
curl "$FORGE/api/v1/members?repo=acme/checkout-service"      # list
curl -X DELETE $FORGE/api/v1/members/<id>                     # remove
```

> **Bootstrap mode:** a repo with **zero** members skips role checks entirely
> (anyone may approve). The moment the first member is added, enforcement
> turns on. Add members before you rely on the gate.

> **Identity:** When SSO is enforced (any provider enabled — see
> `docs/sso.md`), the approver is the **authenticated session email**; a body
> `approver` is ignored and the endpoint is unreachable without a session
> (`401`). RBAC membership should therefore use work emails as usernames. In
> **open bootstrap mode** (no SSO enabled) there is no session, so the approver
> name is client-asserted — enable SSO before relying on approver identity.
> Every vote (and every denied vote) is recorded in `audit_log` as
> `approval.vote` with the resolved approver as `actor` (see `docs/sso.md` →
> Audit log).

## Approval rules (protected environments)

A job with `environment: <name>` blocks before running when `<name>` matches a
protected environment. Rules live server-side — a PR editing pipeline YAML
cannot weaken them. The rule with `repo` set overrides the global (`repo:""`)
default of the same name.

```sh
curl -X POST $FORGE/api/v1/protected-environments -d '{
  "repo": "acme/checkout-service",
  "name": "production",
  "required_approvals": 2,
  "approval_timeout_hours": 24,
  "approver_roles": ["admin", "owner"],
  "allow_self_approval": false
}'
```

| Field | Behavior |
|---|---|
| `required_approvals` | Votes needed before the job is released to a runner |
| `approver_roles` | Only members with one of these roles may vote |
| `allow_self_approval` | When false, the pipeline's `triggered_by` user cannot approve their own deployment (separation of duties) |
| `approval_timeout_hours` | Blocked jobs fail automatically after this window |

Enforced transitions (all server-side, all audited in `job_approvals`):

1. Job with a protected environment → `blocked` once its needs pass.
2. Vote: `POST /api/v1/jobs/{id}/approvals {"approver","verdict","comment"}`
   - approver's role not in `approver_roles` → **403**
   - approver == pipeline author and self-approval disabled → **403**
   - second vote by the same approver → **409**
3. Any `rejected` vote → job **failed**. `required_approvals` approvals →
   job `pending` (released).
4. A new push voids nothing retroactively but old pipelines' blocked jobs can
   be left to time out; votes are always pinned to the job (and thus the SHA).

A default global rule ships out of the box: environment `production`,
1 approval, roles `admin,owner`.
