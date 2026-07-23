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
     image: golang:1.24
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
