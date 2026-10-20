# Observability: history, time ranges, webhook health

## Time range picker

The dashboard and the runner history share one control: **24h · 7 days ·
30 days**. The choice is kept in the URL (`?range=7d`, so a view can be
shared) and remembered per browser. Buckets follow the range: hourly for
24h, 6-hourly for 7 days, daily for 30 days. Every number is compared with
the **same-length window before it** ("success rate 92%, ▲4 vs last
week").

API: `GET /api/v1/stats?range=24h|7d|30d` (or `range=custom&from=&to=`
RFC 3339, with an optional `bucket=1h|6h|1d`). The response carries `range`,
`totals` and `previous` (pipelines, finished, succeeded, success rate, jobs
started) and dense `pipelines`/`jobs` series.

Ranges up to 7 days are computed from the jobs table; longer ranges read
**hourly rollups** (`job_stats_hourly`, `pipeline_stats_hourly`) that the
scheduler fills every 10 minutes for completed hours and backfills a week,
so a 30-day page stays one small scan as history grows. Rollups are kept
for 400 days.

## Runner history (Runners page)

For one runner or all of them, over the chosen range:

| Chart | Series | Reading it |
|---|---|---|
| Load | busy slots (busy seconds ÷ bucket length), jobs started | "3 of 3 slots busy at 11am" means you're short of runners then |
| Queue wait | p50 / p95 of queued → started | the "do we need more runners" signal |
| Job duration | p50 / p95 of started → finished | "did builds get slower after the upgrade?" — filter by repo or job name |

Hover a point for its values; click it to list the jobs in that bucket, and
open them in the job list (`/jobs?runner=&started_from=&started_to=`).

API: `GET /api/v1/runner-history?range=&runner=&repo=&job=`,
`GET /api/v1/runner-history/jobs?from=&to=&runner=&repo=&job=`.

Prometheus (`GET /api/v1/metrics`), over the last hour:
`forge_job_queue_wait_seconds{quantile="0.5|0.95"}`,
`forge_job_duration_seconds{quantile=…}`, `forge_jobs_started_last_hour`,
`forge_runner_busy_slots{runner=…}` — alongside the queue-depth metrics.
Alert on p95 queue wait to know you need runners before people do.

## Webhook health

Every delivery a git host sends is logged — accepted, duplicate, rejected
signature, bad payload — with the repo it was for (read from the payload
even when the signature fails), the event, Forge's answer, the reason and
the source address. Kept 30 days.

- **Repo → Settings → Webhooks**: the last 24 hours in numbers (accepted /
  rejected / errors / duplicates, how many distinct sources sent them) and
  the recent deliveries. A warning names the problem: *"this repo sends
  from 2 hooks; deliveries from 10.0.0.9 fail the signature check"* — a
  second hook with a stale secret, the thing you otherwise only see in the
  server logs.
- **Admin → About**: repos with failing deliveries in the last 24 hours.

API: `GET /api/v1/webhook-log?repo=` (signed-in),
`GET /api/v1/webhook-log/failing` (admin).
