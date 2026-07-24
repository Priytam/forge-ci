# Scheduled pipelines (cron)

A repo can have any number of **schedules**. Each schedule fires a pipeline for a
`(repo, ref)` on a cron cadence — the same way a push webhook does, but on a
clock instead of a commit. Scheduled runs go through the exact webhook trigger
path: the repo's registered config is loaded, the ref is resolved to its tip SHA,
and the config is compiled and run.

## Cron format & timezone

Schedules use the **standard 5-field cron** expression:

```
┌───────────── minute        (0-59)
│ ┌─────────── hour          (0-23)
│ │ ┌───────── day of month  (1-31)
│ │ │ ┌─────── month         (1-12 or JAN-DEC)
│ │ │ │ ┌───── day of week    (0-6 or SUN-SAT; 0 = Sunday)
│ │ │ │ │
* * * * *
```

The usual operators are supported: `*` (any), ranges (`1-5`), lists (`1,15,30`),
steps (`*/5`, `0-30/10`), and month/day names (`JAN`, `MON`). Seconds and
`@`-descriptors (e.g. `@daily`) are **not** accepted — an explicit 5-field
expression is always required. Parsing is done by the well-vetted
`github.com/robfig/cron/v3` library.

**All schedules are evaluated in UTC.** `next_run_at` is computed in UTC from the
cron expression, so a schedule fires at the same absolute instant regardless of
the server's local clock or where replicas run. For example `0 3 * * *` fires at
03:00 UTC every day.

## `CI_PIPELINE_SOURCE`

A scheduled pipeline is compiled with `CI_PIPELINE_SOURCE == "schedule"`, so
`rules:` can key jobs on it — a nightly job that only runs on the schedule:

```yaml
jobs:
  nightly-e2e:
    stage: test
    script: [./run-e2e.sh]
    rules:
      - if: '$CI_PIPELINE_SOURCE == "schedule"'
```

The other source values are `push`/`webhook` (a VCS push), `api` (a manual
`POST /api/v1/pipelines`), and `push` (config validation).

## Firing semantics (replica-safe, catch-up-safe)

The scheduler checks for due schedules every ~30s (and once shortly after
startup). For each enabled schedule whose `next_run_at <= now()`, it:

1. Computes the next fire time **strictly after now** from the cron expression.
2. **Atomically claims** the schedule with a compare-and-set:
   `UPDATE … SET next_run_at = <next>, last_run_at = now() WHERE id = ? AND
   next_run_at = <the value it read> AND enabled`. Only one tick/replica can win
   this update; every other observer sees the advanced value and skips.
3. On winning the claim, builds and creates the pipeline.

Two consequences fall out of claiming *before* building:

- **Single-fire across replicas.** The compare-and-set is the dedup key (the same
  pattern as commit-status `ClaimStatusPost`). Two servers ticking at once cannot
  both fire the same schedule.
- **Catch-up = fire once, then advance; no backfill.** `next_run_at` jumps to the
  next slot *after now*, not the slot right after the missed one. A schedule that
  missed hours of windows (server down, long pause) fires **once** on the next
  check and then resumes its normal cadence — it never storm-fires the backlog.

If a schedule's repo has **no registered config**, or its ref can't be resolved
to a SHA, the fire is **skipped with a logged warning**. Because the claim
already advanced `next_run_at`, a broken schedule is retried on its next slot
rather than re-attempted every tick.

## Retention

Schedules are **configuration, not run data** — the `RETENTION_DAYS` sweep never
deletes them. (The pipelines a schedule creates are ordinary pipelines and are
subject to normal retention.) A schedule is removed only by an explicit
`DELETE /api/v1/schedules/{id}`.

## API

All mutations are admin-gated and audited (`schedule.create` / `schedule.update`
/ `schedule.delete`); the list endpoint is open.

### `GET /api/v1/schedules?repo=<repo>`

List a repo's schedules (omit `repo` for all). Each row carries `enabled`,
`cron`, `ref`, `last_run_at` (nil until first fire) and `next_run_at`.

### `POST /api/v1/schedules`

```json
{ "repo": "owner/name", "ref": "main", "cron": "0 3 * * *", "enabled": true }
```

Validates the cron expression (**400** on a bad expression), computes
`next_run_at` in UTC, and inserts. Returns the created schedule. `enabled`
defaults to `true` if omitted.

### `PUT /api/v1/schedules/{id}`

```json
{ "cron": "*/30 * * * *", "ref": "release", "enabled": false }
```

All fields are optional; omitted fields are left unchanged. `next_run_at` is
recomputed from the effective cron (**400** on a bad expression).

### `DELETE /api/v1/schedules/{id}`

Deletes the schedule. **404** if it does not exist.
