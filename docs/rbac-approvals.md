# Roles, membership, and approval rules

## Roles

Membership is per repo, one role per user:

| Role | Intent |
|---|---|
| `admin` | Manages the repo's CI: variables, runners, members, approval rules |
| `owner` | Senior reviewer; typically an allowed production approver |
| `developer` | Runs pipelines; cannot approve protected deployments by default |

### Who can run what

| Action | Who |
|---|---|
| Retry or cancel a pipeline, play or cancel a job | Platform admins, plus any member of the repo (`admin`, `owner` or `developer`). On a repo with no members (bootstrap mode) any signed-in user. |
| Variables and secrets, runners and runner tokens, members, approval rules, SSO, token policy | Platform admins only (`ADMIN_EMAILS`) |

Members are matched to the signed-in email case-insensitively. The UI asks
`GET /api/v1/permissions?repo=<repo>` (`{is_admin, can_run, can_cancel}`) to
decide which buttons to show; the server re-checks every action, and denied
attempts are audited as `operate.denied`. The checks live in Go
(`internal/api/security.go`: `canOperate`, `requireAdmin`), so they ship with
the server image — there is no separate policy file to keep in sync.

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

## Approver groups: "N approvals, at least M from group X"

Admins define named groups of people under **Admin ▾ → Approver groups**
(`GET /api/v1/approver-groups`, `PUT`/`DELETE /api/v1/approver-groups/{name}`
with `{"description", "members": ["harish@acme.io", ...]}`).

An approval rule can then require part of its approvals from a group:

```sh
curl -X POST $FORGE/api/v1/protected-environments -d '{
  "repo": "acme/ecom", "name": "production",
  "required_approvals": 2, "approver_roles": ["owner"],
  "required_group": "release-managers", "required_group_approvals": 1
}'
```

- The job is released only when it has `required_approvals` approvals **and**
  at least `required_group_approvals` of them are from group members.
- Group members may vote on that environment even without a repo role.
- Self-approval and one-vote-per-person rules still apply; members are matched
  case-insensitively by email.
- A group named by a rule can't be deleted (409) until the rule changes.
- Approval notifications include the group's members and say "at least M from
  <group>".


## One environment, one rule

Rule names are canonicalised through the environment registry (an alias or
case variant such as `prod` or `production` becomes the registered
`Production`), in three places so an old rule can never win silently:

- **saving** a rule canonicalises its name;
- **resolving** the rule for a job canonicalises the job's environment the
  same way (and matches case-insensitively), so a job stored under an old
  spelling meets the canonical rule;
- **at start and whenever the registry is saved**, rules saved under an
  alias or case variant are **folded** into the canonical rule, keeping the
  stricter of each setting (more approvals, the shorter timeout, self-approval
  off, the group requirement), and the leftover row is deleted. The server log
  says how many were folded.

A rule whose name matches no registered environment (nor an alias) is shown
as **inactive** on the Members & approval rules page — no job can ever match
it — with a Delete button.

## Seeing why a job is still blocked

Job detail (`GET /api/v1/jobs/{id}`) and the pipeline view both return the
full gate: `required_approvals`, `required_group`,
`required_group_approvals`, `group_approvals` (group votes so far) and
`group_members`. The gate shows "1 of 2 from release-approvers — waiting on
Ravi, Ashwin", and tells a viewer whose vote cannot release the job that
their vote counts but one more is still needed from the group.
