# Notifications

Forge announces what matters to wherever your team works. Each destination
is a **notifier**; Slack, Microsoft Teams, a signed JSON **webhook** and
**email** are built in, and another is one file in `internal/notify/`.

| Event | When |
|---|---|
| `approval_needed` | a job starts waiting for environment approval |
| `pipeline_failed` | a pipeline finishes with a failed job (not an `allow_failure` one) |
| `pipeline_fixed` | a pipeline succeeds after the previous finished one on the same branch failed |
| `pipeline_succeeded` | a pipeline finishes green |
| `deploy_succeeded` / `deploy_failed` | an `environment:` job finishes |

## Routes — Admin → Notifications

A **route** is a destination plus the events it takes, optionally limited to
repos (`*`, `acme/*`, `acme/**`, or one repo). Add as many as you like: a
`#deploys` Slack channel for deploy events across the org, a Teams channel
for one team's failures, your incident tool over a webhook.

| Kind | Target | Notes |
|---|---|---|
| `slack` | incoming-webhook URL | Block Kit message with an "Open in Forge" button |
| `teams` | Teams workflow ("post to a channel when a webhook request is received") or connector URL | Adaptive Card |
| `webhook` | your https URL | JSON body (below); `X-Forge-Event`, `X-Forge-Delivery`, and with a route secret `X-Forge-Signature: sha256=<hex HMAC-SHA256 of the body>` |
| `email` | addresses separated by commas, or `approvers` (each eligible approver) | uses the SMTP settings on the same page |

Targets and secrets are encrypted at rest and never returned by the API;
**Test** sends a sample event through one route. `GET/POST/PUT/DELETE
/api/v1/notification-routes[/{id}]`, `POST …/{id}/test` (admin).

Webhook body:

```json
{
  "event": "pipeline_failed", "repo": "acme/shop", "ref": "main", "sha": "…",
  "pipeline_id": 812, "job_id": 0, "status": "failed",
  "title": "acme/shop: pipeline #812 on main failed",
  "text": "Pipeline #812 for acme/shop on main @ 1a2b3c4d failed.\nFix totals",
  "link": "https://forge.example.com/pipelines/812",
  "fields": {"Branch": "main @ 1a2b3c4d", "Failed jobs": "unit, lint", "Started by": "harish@acme.io"},
  "at": "2026-10-17T09:30:00Z"
}
```

Verify the signature as `hex(HMAC_SHA256(secret, raw_body))` and compare in
constant time.

## Approval defaults

The **approval defaults** on the same page keep working as before: a Slack
webhook for the team channel and/or an email to each eligible approver when
a deploy waits for approval. **Eligible approvers** are the repo's members
(direct or through an IdP group) whose role the environment's rule accepts,
minus the person who started the run when self-approval is off; on a repo
with no members, the platform admins (`ADMIN_EMAILS`).

## Approve or reject from the email

When the server runs with `FORGE_SECRET_KEY`, each approver's email carries
**personal Approve / Reject links**. A link names the job, the approver and
the verdict and is signed (HMAC-SHA256 with a key derived from the secret
key); it expires after 24 hours. Opening it shows a confirmation page — the
vote is cast only by the button on that page (a `POST`), so mail scanners
and link previews that follow the link never vote. The vote goes through the
normal approval rules as that approver (role, self-approval, one vote per
person) and is audited as `approval.<verdict>` via email link. Slack and
Teams messages carry an "Open in Forge" button instead: a channel-wide
approve button would let anyone in the channel approve.

## Details

- The scheduler checks every 5 seconds. Each event is claimed once in the
  database (`jobs.approval_notified_at`, `pipeline_notifications`,
  `jobs.deploy_notified_at`), so several server replicas never double-send.
- Only events from the last hour are sent — turning notifications on never
  floods anyone with old ones — and nothing is claimed while no route or
  default wants the event.
- A pipeline counts as finished when every job is done and at least one
  wasn't canceled; a canceled pipeline is not announced.
- Port 465 uses implicit TLS for SMTP; other ports upgrade with STARTTLS when
  offered.
