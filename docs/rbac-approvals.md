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

> **Identity caveat:** Forge does not yet authenticate users — the approver
> name is client-asserted. The RBAC mechanism is fully enforced server-side,
> but until an OIDC proxy fronts the API, identity itself is trust-based.
> Front `forge-server` with an authenticating reverse proxy and map the
> proxy's identity header before production use.

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
