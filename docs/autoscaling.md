# Runner autoscaling

Forge can grow and shrink **pools** of runners to match the queue, on any
infrastructure. A pool is a set of identical runners (same tags) that a
**provider** can count and resize:

| Provider | Resizes | Needs |
|---|---|---|
| `kubernetes` | a Deployment or StatefulSet of runners, through the API's scale subresource | the server running in the cluster with a service account allowed to `get`/`patch` `deployments/scale`, or `api_url` + `token` |
| `docker` | runner containers on a Docker host, through the Engine API | the host's socket (`unix:///var/run/docker.sock`) or `tcp://host:2375` reachable from the server |
| `exec` | anything a command can — AWS/GCP/Azure VM groups, Terraform, Nomad, bare metal | the CLI on the server host |

Another provider is one file in `internal/autoscale/`. You can still use an
external autoscaler instead (KEDA on `forge_queue_pending_jobs`, see
[runners.md](runners.md)); pools are for when you'd rather Forge did it.

## How it decides

Every 30 seconds (`AUTOSCALE_INTERVAL`), for each enabled pool:

1. **Demand** = jobs waiting whose required tags the pool's runners cover
   + jobs running on the pool's runners.
2. **Desired** = `ceil(demand / jobs_per_runner)`, clamped to `[min, max]`.
3. **Scale up** happens at once.
4. **Scale down** waits until the pool has had no work for
   `scale_down_after` (default 10 minutes), then removes **one runner per
   pass**. A runner that is stopped **drains**: it finishes its job (or
   requeues it after the grace period) before exiting, so nothing is lost
   (see *Graceful shutdown* in runners.md). The docker provider stops idle
   runners first; Kubernetes picks pods itself, which is why the step is one.

Give each pool its own tag set (for example `linux,docker` for one pool,
`linux,gpu` for another). Two pools covering the same jobs would both scale
for them.

## Setting up a pool — Runners page → Pools (admin)

| Field | Meaning |
|---|---|
| name | lowercase; becomes part of the runner ids |
| provider | kubernetes / docker / exec |
| tags | the tags the pool's runners register with (`RUNNER_TAGS`) |
| min / max | bounds; `min: 0` scales to nothing when idle |
| jobs per runner | the runner's concurrency (1 for shell/docker, more for kubernetes-executor runners) |
| scale down after | idle seconds before shrinking |
| settings | provider config (JSON); secret keys are stored encrypted and shown masked |

### kubernetes

```json
{"namespace": "ci", "deployment": "forge-runners-linux"}
```

The server's service account needs:

```yaml
rules:
  - apiGroups: ["apps"]
    resources: ["deployments/scale", "statefulsets/scale"]
    verbs: ["get", "patch"]
```

Optional: `kind: StatefulSet`, `api_url` + `token` (+ `ca` PEM, or
`insecure: "true"`) for a cluster the server does not run in. The runner
Deployment's pods should have a `terminationGracePeriodSeconds` at least as
long as the runner's drain grace (`RUNNER_DRAIN_GRACE`), so a pod chosen for
scale-down can finish its job.

### docker

```json
{"image": "ghcr.io/you/forge-runner:1.9", "host": "unix:///var/run/docker.sock",
 "server_url": "https://forge.example.com", "runner_token": "frt_…",
 "executor": "docker", "docker_sock": "true", "env.RUNNER_DRAIN_GRACE": "10m"}
```

Each runner is a container named `forge-<pool>-…` (its `RUNNER_ID`), labelled
`io.forge-ci.pool=<pool>`; `env.KEY` settings become environment variables.
Scale-down stops idle runners first (`SIGTERM`, 120 s), then removes them.

### exec

```json
{"count_cmd": "aws autoscaling describe-auto-scaling-groups --auto-scaling-group-names forge-runners --query 'AutoScalingGroups[0].DesiredCapacity' --output text",
 "scale_cmd": "aws autoscaling set-desired-capacity --auto-scaling-group-name forge-runners --desired-capacity $FORGE_DESIRED",
 "env.AWS_REGION": "eu-west-1"}
```

`count_cmd` prints the current size; `scale_cmd` sets it. Both run with
`sh -c` and `FORGE_POOL`, `FORGE_TAGS`, `FORGE_CURRENT`, `FORGE_DESIRED`
(scale only), `FORGE_MIN`, `FORGE_MAX`, `FORGE_BUSY_RUNNERS`, plus any
`env.KEY` settings. The same shape fits `gcloud compute instance-groups
managed resize`, `az vmss scale`, `terraform apply -var runners=$FORGE_DESIRED`,
or a script that talks to your own provisioner. Commands run on the server
host, so the image needs the CLI (add it to your server image, or have the
command call a sidecar).

## Status and operations

The Pools table shows, per pool, the current and desired size, pending jobs,
the last action and any error; **Evaluate now** runs a pass immediately
(`POST /api/v1/runner-pools/evaluate`). Only one server replica acts per
pass (a database advisory lock). API: `GET/POST/PUT/DELETE
/api/v1/runner-pools[/{id}]` (admin).

Runners in a pool register normally: the first job they take appears on the
Runners page like any other runner; runner tokens (`RUNNER_AUTH=on`) work as
usual — pass the token through the provider's settings.
