# Registering runners

Runners are pull-based: they long-poll the control plane, so they work from
anywhere that can reach `forge-server` over HTTP — no inbound ports, no
server-side pre-registration. A runner appears in the registry (and the UI)
automatically on its first poll.

Flags / env vars:

| Flag | Env | Meaning |
|---|---|---|
| `--server` | `SERVER_URL` | control plane base URL |
| `--executor` | `EXECUTOR` | `shell` or `docker` |
| `--id` | `RUNNER_ID` | stable unique name (defaults to host-pid) |
| `--tags` | `RUNNER_TAGS` | comma-separated capabilities, e.g. `gpu,linux` |
| `--token` | `RUNNER_TOKEN` | runner auth token (required when the server runs `RUNNER_AUTH=on`) |
| `--concurrency` | `RUNNER_CONCURRENCY` | jobs run in parallel; the Runners page counts capacity in these slots |
| `--pool` | `RUNNER_POOL` | autoscaled pool the runner belongs to (shown and grouped on the Runners page); docker pools set it, set it in your Deployment/VM template for others |

## Runner authentication

The runner protocol (`/api/v1/runner/*`) and artifact upload are gated by the
server env var **`RUNNER_AUTH`**:

| `RUNNER_AUTH` | Behaviour |
|---|---|
| `off` (default) | **Unauthenticated** — any client can acquire jobs, push logs and upload artifacts. Backward-compatible; the server logs a warning that this is insecure. Runners work with or without a token. |
| `on` | Every `/runner/*` call and artifact upload must carry `Authorization: Bearer <token>` matching a non-revoked row in `runner_tokens`. Missing/invalid/revoked tokens get `401`. |

When `RUNNER_AUTH=on` is set with **zero tokens** in the table, the server
auto-generates one on startup and prints it (once) to the log so an operator
can configure runners:

```
WARN RUNNER_AUTH=on with no tokens — generated a bootstrap runner token. ... runner_token=<hex>
```

Configure each runner with that value via `--token` / `RUNNER_TOKEN`.

**Token admin API** (mutations require platform admin — see docs/sso.md):

- `POST /api/v1/runner-tokens {"description": "..."}` → returns the raw `token` **once**.
- `GET  /api/v1/runner-tokens` → masked list (last-4 suffix, `revoked`, `last_used_at`); never returns the raw token.
- `POST /api/v1/runner-tokens/{id-or-token}/revoke` → revokes; the token then gets `401`.

**Runner groups via tags.** Runners are deployed independently of repos —
the fleet is global, and tags are how work is routed to it:

1. Deploy runners with capability tags: `--tags=gpu`, `--tags=docker,linux`,
   `--tags=prod-deploy` — a set of same-tagged runners is a "group".
2. A repo selects its group with **default runner tags** (repo Settings →
   Runner tags, or `PUT /api/v1/repo-settings {"repo": "...",
   "default_runner_tags": ["gpu"]}`). Every job of that repo without its own
   `tags:` inherits them.
3. A job-level `tags:` list overrides the repo default for that job.
4. A job runs only on a runner advertising **all** of its effective tags;
   a job with no effective tags runs on any runner.

**Pausing:** `POST /api/v1/runners/{id}/pause {"paused": true}` (or the UI
toggle on the top-level Runners page) drains a runner without killing it.

## 1. Shell runner on a VM / bare metal

Runs job scripts directly on the host — fastest, least isolated. Use for
trusted, internal workloads only.

1. Build or copy the binary: `go build -o forge-runner ./cmd/runner`
   (cross-compile with `GOOS=linux GOARCH=amd64 go build ...`).
2. Start it:
   ```sh
   ./forge-runner --server=https://forge.internal.example.com \
     --id=vm-build-01 --executor=shell --tags=linux,amd64
   ```
3. Make it survive reboots — systemd unit `/etc/systemd/system/forge-runner.service`:
   ```ini
   [Unit]
   Description=Forge CI runner
   After=network-online.target
   [Service]
   ExecStart=/usr/local/bin/forge-runner --server=https://forge.internal.example.com --id=%H --executor=shell --tags=linux
   Restart=always
   User=forge
   [Install]
   WantedBy=multi-user.target
   ```
   Then `systemctl enable --now forge-runner`.
4. Verify it shows **online** under `GET /api/v1/runners` or the Runners
   settings section in the UI.

## 2. Docker runner

Each job runs in its own container of the job's `image:` (default `alpine:3`),
with the workspace bind-mounted and networking disabled.

1. Host needs Docker installed and the runner user in the `docker` group.
2. Start:
   ```sh
   ./forge-runner --server=https://forge.internal.example.com \
     --id=docker-01 --executor=docker --tags=docker,linux
   ```
3. Jobs pick their image per job:
   ```yaml
   unit-tests:
     stage: test
     image: golang:1.25
     script: [go test ./...]
   ```

## 3. Kubernetes executor (ephemeral pod per job — GitLab-style)

Nothing fixed runs per job: one resident **manager** process acquires jobs and
forks each one into its own ephemeral Pod (created on acquire, deleted after).

1. The manager machine/pod needs `kubectl` with access to the target cluster.
2. Start the manager — `KUBE_CONTEXT` is **required** (it refuses the
   kubeconfig current-context to protect shared kubeconfigs):
   ```sh
   KUBE_CONTEXT=my-ci-cluster KUBE_NAMESPACE=ci \
   ./forge-runner --executor=kubernetes --id=k8s-manager-1 --tags=k8s \
     --concurrency=4    # up to 4 pods in flight from one manager
   ```
3. Per job the manager: creates `forge-job-<id>` from the job's `image:`,
   ships the prepared workspace in (clone + restored upstream artifacts),
   execs the script (logs stream live, exit code propagates), copies declared
   artifact paths back out, deletes the pod. Orphaned pods self-terminate
   within 2h; all job pods carry the `app=forge-ci-job` label.
4. Job images need `sh` and `tar` (alpine and typical build images do).

Full deployment reference — exact ServiceAccount/Role RBAC, the
manager-in-cluster Deployment (`KUBE_CONTEXT=in-cluster`), driving a
**separate** job cluster via a dedicated kubeconfig, network matrix, quotas —
lives in [kubernetes-deployment.md](kubernetes-deployment.md). The
kubectl-equipped manager image is `deploy/Dockerfile.runner-k8s`.

## 3b. Alternative: fleet of shell/docker runners on Kubernetes

When per-job pod overhead isn't wanted, run N resident runners as a
Deployment:

1. Build and push the runner image:
   ```sh
   docker build -f deploy/Dockerfile.runner -t <registry>/forge-runner:latest .
   docker push <registry>/forge-runner:latest
   ```
2. Deploy N replicas:
   ```yaml
   apiVersion: apps/v1
   kind: Deployment
   metadata: {name: forge-runners, namespace: ci}
   spec:
     replicas: 4
     selector: {matchLabels: {app: forge-runner}}
     template:
       metadata: {labels: {app: forge-runner}}
       spec:
         containers:
         - name: runner
           image: <registry>/forge-runner:latest
           env:
           - {name: SERVER_URL, value: "http://forge-server.ci.svc:8080"}
           - {name: EXECUTOR, value: "shell"}
           - {name: RUNNER_TAGS, value: "k8s,linux"}
           - {name: RUNNER_POOL, value: "k8s-pool"}   # optional: the pool, if autoscaled
           - name: RUNNER_ID
             valueFrom: {fieldRef: {fieldPath: metadata.name}}
   ```
3. `kubectl apply -f runners.yaml` — each pod registers itself by pod name.
   Scale with `kubectl scale deploy/forge-runners --replicas=10`.

## Sizing & operations

- One runner processes one job at a time; run one runner process per desired
  concurrent job.
- A runner that dies mid-job stops heartbeating; the scheduler fails the job
  after 90s and the runner registry shows it offline after 30s.
- Give every long-lived runner a stable `--id`; ephemeral pods use pod names.

### Graceful shutdown (drain)

On `SIGINT`/`SIGTERM` a runner stops acquiring new jobs and gives in-flight
jobs up to `RUNNER_DRAIN_GRACE` (default `30s`) to finish. Any job still
running when the grace period elapses is killed and **requeued** (its executor
context is canceled first, so it never double-runs) — the job returns to
`pending` and another runner picks it up, rather than being left for the 90s
stale-timeout to fail. Set `RUNNER_DRAIN_GRACE=0` to requeue in-flight jobs
immediately on signal. Set it comfortably above your typical job length (and
below your orchestrator's termination grace period, e.g. Kubernetes
`terminationGracePeriodSeconds`) to let most jobs finish in place on rollout.

### Cancellation

Cancelling a running job (`POST /api/v1/jobs/{id}/cancel` or a whole pipeline)
is delivered to the runner through the heartbeat response: the next heartbeat
(≤10s) returns `{cancel:true}`, the runner kills the job's process group /
container / pod exactly like a timeout, and reports the job as `canceled`.

## Queue depth, load and autoscaling

The **Runners** page shows the queue, grouped by the runner tags jobs need:
how many jobs wait, how long the oldest has waited, and how many eligible
runners are online and busy ("none — jobs can't start" means no online runner
has those tags). Each runner shows what it is running now and how busy it was
over the last 24 hours. Same data: `GET /api/v1/runner-load`.

`GET /api/v1/metrics` (Prometheus text, no auth) exports:

| Metric | Labels | Meaning |
|---|---|---|
| `forge_queue_pending_jobs` | `tags` (sorted, comma-joined) | jobs waiting for a runner with those tags |
| `forge_queue_oldest_wait_seconds` | `tags` | age of the oldest waiting job |
| `forge_jobs_pending`, `forge_jobs_running` | — | totals |
| `forge_runners_online`, `forge_runners_busy` | — | online unpaused runners, and those running a job |

Scale runner Deployments on them, for example with KEDA's Prometheus scaler:

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: forge-runners-linux}
spec:
  scaleTargetRef: {name: forge-runners}
  minReplicaCount: 1
  maxReplicaCount: 20
  triggers:
    - type: prometheus
      metadata:
        serverAddress: http://prometheus.monitoring:9090
        query: sum(forge_queue_pending_jobs{tags=~"(^|.*,)linux(,.*|$)"})
        threshold: "2"
```

A runner that is stopped mid-scale-down drains: it finishes (or requeues) its
current job before exiting (see *Graceful shutdown* above).

Or let Forge scale the runners itself: **autoscaled pools** resize a
Kubernetes Deployment, containers on a Docker host, or anything a command
can (cloud VM groups, Terraform) from the queue — see
[autoscaling.md](autoscaling.md).

## History: load, queue latency, job duration

The Runners page charts busy slots, jobs started, queue-wait p50/p95 and
job-duration p50/p95 over 24h / 7 days / 30 days, per runner or overall,
with click-through to the jobs behind a point — see
[observability.md](observability.md).

Capacity and queue depth are snapshotted once a minute (kept 62 days), so
the Capacity and Queue tiles and the Utilisation chart show them over the
range and against the previous period; utilisation uses the recorded
available slot-time once snapshots cover the range. A runner not seen for
`RUNNER_STALE_AFTER` (server env, default `24h`) is stale: left out of
capacity and tag counts, and an admin can remove it.

## Drain, upgrade, lost jobs, maintenance mode

See [runner-operations.md](runner-operations.md): draining a runner from
the Runners page, runner versions and self-update (one runner or a rolling
"upgrade all"), what happens to a job whose runner vanishes
(`retry: {when: [runner_system_failure]}`), and maintenance mode for server
upgrades.

## Persistent BuildKit cache and cache affinity

### Cache affinity (`runner_affinity: true`)

```yaml
build-image:
  stage: build
  tags: [docker-builder]
  runner_affinity: true
  script: [./build.sh]
```

After a job with `runner_affinity: true` succeeds, Forge remembers which
runner ran it (per repo + job name). The next run of that job **prefers that
runner**: other runners leave it in the queue while the preferred runner is
online, unpaused and has the job's tags, for up to `RUNNER_AFFINITY_WAIT`
(server env, default `1m`, Go duration). After that — or immediately when
the preferred runner is offline or paused — any eligible runner takes it, and
affinity moves to whichever runner succeeds. Jobs without the key are never
held.

It pays off where the runner keeps state between jobs: **shell runners**
(language caches in the runner user's home, a local Docker daemon used by the
job) and dedicated build hosts. With the docker/kubernetes executors each job
starts in a fresh container, so prefer the registry cache
(docs/docker-layer-cache.md) there.

### Persistent per-runner BuildKit cache — not built yet

A long-lived BuildKit daemon per runner (shared across jobs) would make
container builds on docker/kubernetes runners hit a warm local cache. It is
**deliberately not shipped**: done naively it is a cross-repo security hole.
See docs/design/persistent-buildkit-cache.md for the design and the open
decisions.
