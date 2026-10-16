# Approval notifications

When a job starts waiting for environment approval (a protected environment,
see docs/rbac-approvals.md), Forge tells the people who can approve it — once
per job — so a production fix doesn't sit unnoticed.

Set it up under **Admin ▾ → Notifications** (`GET`/`PUT
/api/v1/notification-settings`, admin only):

| Channel | Setting | Who gets it |
|---|---|---|
| Slack | an incoming-webhook URL for your team channel | the channel; the message names the eligible approvers |
| Email | SMTP host, port, from address, optional username/password | each eligible approver whose username is an email |

**Eligible approvers** are the repo's members whose role is in the
environment's `approver_roles` (minus the person who started the run when
self-approval is off). On a repo with no members anyone may approve, so the
platform admins (`ADMIN_EMAILS`) are told.

The message says which job, repo, branch and commit, the environment, how many
approvals are needed and from which roles, and links to the job.

Details:

- The scheduler checks every 5 seconds. Each wait is claimed once in the
  database (`jobs.approval_notified_at`), so several server replicas never
  double-send.
- Only waits that began in the last hour are notified — turning notifications
  on never floods anyone with old ones.
- The Slack URL and SMTP password are encrypted at rest (`FORGE_SECRET_KEY`)
  and never returned by the API; leave the field blank to keep the saved value.
- Port 465 uses implicit TLS; other ports upgrade with STARTTLS when offered.
- **Send test** posts a sample to Slack and emails the signed-in admin.
