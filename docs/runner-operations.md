# Runner operations: drain, upgrade, lost jobs, maintenance mode

## Drain and resume (Runners page, admin)

**Drain** tells a runner to take no new jobs and finish what it has; the
row shows "draining — 2 jobs left" until it is idle. **Resume** puts it back
in service. Use it to pick a safe moment to reset or patch a machine: drain,
wait for 0 left, do the work, resume. A hard reboot without draining kills
the jobs on that runner (they are then handled as *lost*, below).

`POST /api/v1/runners/{id}/drain`, `…/resume` (admin). Draining is
server-side state: the runner needs no change, and it works on every
executor.

## Versions and "outdated"

Runners report their version on every poll; the Runners page shows it, and
**outdated** when it differs from the runner the server bundles under
`/downloads`. Both binaries are stamped at build time:

```sh
go build -ldflags "-X github.com/priytamjeepandey/forge-ci/internal/buildinfo.Version=1.9.0" ./cmd/server
# the Dockerfiles: docker build --build-arg VERSION=1.9.0 …
```

Unstamped builds report `dev` and are never called outdated.
`GET /api/v1/status` (public) returns the server version;
Admin → **About** shows server and bundled-runner versions, uptime,
database and migration status.

## Self-update

A runner started with `RUNNER_SELF_UPDATE=on` updates itself when asked:
once it is idle it downloads the server's binary for its platform from
`/downloads`, checks the download runs and reports the expected version,
swaps it in next to itself (the runner's directory must be writable) and
re-executes with the same arguments. Runners installed from a package or a
container image should be updated that way instead — leave self-update off
and let the image roll.

- **Upgrade** on one runner: drain → update when idle → resume (automatic).
- **Upgrade all runners**: a rolling upgrade, one runner at a time. Each
  outdated online runner is drained, updated and waited for before the next
  starts; a runner that doesn't come back within 10 minutes is skipped and
  listed. Stop it any time; the runner mid-upgrade finishes its own step.

`POST /api/v1/runners/{id}/upgrade`, `GET/POST/DELETE /api/v1/runners/rollout`.

## Lost jobs

A job whose runner stops heart-beating (power loss, hard reboot, network
cut) is marked **failed** with the reason `runner lost: no heartbeat for
90s` after the stale window (`STALE_JOB_AFTER`, default 90s). The reason
shows on the job and in the pipeline-failed notification ("Failed jobs:
deploy (runner lost…)").

To retry infrastructure losses without retrying real failures:

```yaml
deploy:
  retry:
    max: 1
    when: [runner_system_failure]   # also: script_failure, always (default)
```

`retry: 2` still means "retry on any failure up to twice". With `when:`
listing only `runner_system_failure`, a script that exits non-zero is not
retried, but a job whose runner vanished is re-queued (same attempt
counter, a `[forge] runner lost … retrying` note in the log) until `max`
is used up.

## Maintenance mode

Admin → About → **Turn maintenance on** pauses job acquisition on **every**
runner: running jobs continue, new jobs queue, and every page shows "Forge
is upgrading — new jobs queue, running jobs continue" (with your message).
Turn it off after the upgrade. `PUT /api/v1/maintenance {"on": true,
"message": "…"}`; `GET /api/v1/status` reports it.
