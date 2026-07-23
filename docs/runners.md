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

**Tag routing:** a job with `tags: [gpu]` runs only on runners advertising
`gpu`. Untagged jobs run on any runner. **Pausing:** `POST
/api/v1/runners/{id}/pause {"paused": true}` (or the UI toggle) drains a
runner without killing it.

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

## 3. Kubernetes (runner fleet as a Deployment)

Until a native `kubernetes` executor lands (jobs as ephemeral Pods), run a
fleet of shell/docker runners *on* Kubernetes:

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
