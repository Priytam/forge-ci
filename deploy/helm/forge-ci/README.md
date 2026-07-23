# Forge CI Helm chart

Umbrella chart that deploys the whole Forge CI platform: the control-plane
**server**, the React **web** dashboard, a **runner** fleet (shell / docker /
kubernetes executor), and — optionally — a bundled **Postgres**.

- Chart: `deploy/helm/forge-ci`
- App surface it wires: server `:8080` (`/api/v1/healthz`), the env matrix from
  the project README's "Security & operations" table, and the S3-compatible
  artifact store from `internal/blob`.

## Prerequisites

- Kubernetes 1.25+ and Helm 3+ (works with `helm` v4 too).
- Images built and pushed to a registry you control:

  ```sh
  docker build -f deploy/Dockerfile.server     -t <registry>/forge-ci/server:<tag> .
  docker build -f deploy/Dockerfile.runner     -t <registry>/forge-ci/runner:<tag> .
  docker build -f deploy/Dockerfile.runner-k8s -t <registry>/forge-ci/runner-k8s:<tag> .
  docker build -f web/Dockerfile               -t <registry>/forge-ci/web:<tag> web/
  ```

  Then set `image.registry=<registry>` (and per-component `*.image.tag`).

## Install

Fetch the Postgres subchart once, then install:

```sh
helm dependency build deploy/helm/forge-ci
helm install forge ./deploy/helm/forge-ci -n forge-ci --create-namespace
```

Production (managed DB + S3 + RUNNER_AUTH=on + ingress TLS + HPA):

```sh
helm install forge ./deploy/helm/forge-ci -n forge-ci --create-namespace \
  -f deploy/helm/forge-ci/values-prod.yaml \
  --set image.registry=<registry> \
  --set-file secrets.forgeSecretKey=/dev/stdin <<<"$(head -c 32 /dev/urandom | base64)"
```

(Or, preferred, set `secrets.existingSecret` and manage the secret externally —
see below.)

## Upgrade

```sh
helm dependency build deploy/helm/forge-ci   # if chart deps changed
helm upgrade forge ./deploy/helm/forge-ci -n forge-ci -f deploy/helm/forge-ci/values-prod.yaml
```

The server Deployment carries `checksum/config` and `checksum/secret`
annotations, so a changed ConfigMap/Secret rolls the pods automatically.

## Values reference

| Key | Default | Purpose |
|---|---|---|
| `image.registry` | `""` | Registry/org prefix for all component images |
| `image.pullSecrets` | `[]` | imagePullSecrets for private registries |
| `server.replicaCount` | `2` | Server replicas (ignored when HPA on) |
| `server.autoscaling.enabled` | `false` | HPA (autoscaling/v2) for the server |
| `server.service.port` / `.targetPort` | `8080` | Service port / container `LISTEN_ADDR` |
| `server.resources` | set | Server requests/limits |
| `web.enabled` | `true` | Deploy the React dashboard |
| `web.replicaCount` | `2` | Web replicas |
| `runner.enabled` | `true` | Deploy a runner |
| `runner.mode` | `shell` | `shell` \| `docker` \| `kubernetes` |
| `runner.replicaCount` | `1` | Fleet size (shell/docker) |
| `runner.concurrency` | `2` | `RUNNER_CONCURRENCY` per runner/manager |
| `runner.tags` | `linux,shell` | `RUNNER_TAGS` job-routing tags |
| `runner.rbac.create` | `true` | Create Role/RoleBinding (kubernetes mode only) |
| `runner.kube.context` | `in-cluster` | `KUBE_CONTEXT` (kubernetes mode) |
| `runner.kube.namespace` | `""` | Job-pod namespace (default: release ns) |
| `runner.limitRange.enabled` | `false` | Namespace LimitRange for job pods |
| `config.runnerAuth` | `off` | `RUNNER_AUTH` (`on` requires a token) |
| `config.adminEmails` | `""` | `ADMIN_EMAILS` |
| `config.externalUrl` / `.frontendUrl` | `""` | Public origins; derived from ingress host if empty |
| `config.retentionDays` | `30` | `RETENTION_DAYS` |
| `config.maxJobLogBytes` | `10485760` | `MAX_JOB_LOG_BYTES` |
| `config.maxArtifactBytes` | `524288000` | `MAX_ARTIFACT_BYTES` |
| `config.artifacts.store` | `local` | `local` \| `s3` |
| `config.artifacts.dir` | `/data/artifacts` | Local blob dir (`ARTIFACTS_DIR`) |
| `config.artifacts.persistence.enabled` | `false` | PVC for the local artifact dir |
| `config.artifacts.s3.*` | `""` | `S3_ENDPOINT`/`S3_BUCKET`/`S3_REGION`/`S3_USE_SSL`/`S3_ACCESS_KEY` |
| `secrets.create` | `true` | Generate the Secret |
| `secrets.existingSecret` | `""` | Use an external Secret instead |
| `secrets.forgeSecretKey` | `""` | `FORGE_SECRET_KEY` (empty = plaintext passthrough) |
| `secrets.runnerToken` | `""` | `RUNNER_TOKEN` |
| `secrets.webhookSecret` | `""` | `WEBHOOK_SECRET` |
| `secrets.s3SecretKey` | `""` | `S3_SECRET_KEY` |
| `database.url` | `""` | External `DATABASE_URL` (when `postgresql.enabled=false`) |
| `postgresql.enabled` | `true` | Bundle bitnami/postgresql |
| `ingress.enabled` | `false` | Create an Ingress |
| `ingress.className` | `""` | IngressClass (nginx / alb / gce …) |
| `ingress.host` | `forge.example.com` | Public host |
| `ingress.tls.enabled` | `false` | Terminate TLS at the ingress |

See `values.yaml` for the fully-commented set and `values-prod.yaml` for a
worked production example.

## Ingress & routing

With `ingress.enabled=true` the chart creates a single-host Ingress that routes:

- `/api` → the **server** Service (REST API + runner protocol)
- `/`    → the **web** Service (dashboard); if `web.enabled=false`, `/` also
  goes to the server.

`config.externalUrl` / `config.frontendUrl` default to
`https://<ingress.host>` (http when TLS is off), which drives the SSO redirect
and CSRF allow-lists.

> The web container image bakes an nginx that proxies `/api` to an upstream
> named `server`. When you reach the dashboard **through the Ingress** this is
> irrelevant — the Ingress splits `/api` off to the server Service before nginx
> sees it. If you expose the web Service directly (no Ingress), route `/api`
> yourself or hit the server Service on `:8080`.

## Secret management

The chart needs these sensitive keys: `DATABASE_URL`, `FORGE_SECRET_KEY`,
`RUNNER_TOKEN`, `WEBHOOK_SECRET`, and (for S3) `S3_SECRET_KEY`.

**Generated (default).** `secrets.create=true` renders a Secret from
`secrets.*`. `DATABASE_URL` is assembled from the Postgres subchart when
`postgresql.enabled=true`, or from `database.url` for a managed DB. Generate the
encryption key with `head -c 32 /dev/urandom | base64` and pass it via
`--set-file`/`--set` (never commit it).

**External (recommended for prod).** Set `secrets.existingSecret=<name>` and
create that Secret with Vault / External Secrets / SealedSecrets. The chart then
skips secret generation and references your Secret via `envFrom`. It must carry
all the keys above.

## Runner modes

- **shell / docker** — a resident fleet Deployment (`runner.replicaCount`,
  `runner.tags`). docker mode needs a reachable docker daemon (DinD sidecar or
  a mounted socket via `runner.extraVolumes`/`runner.extraEnv`).
- **kubernetes** — one resident manager (`runner.managerReplicas`, default 1)
  running the runner-k8s image with `EXECUTOR=kubernetes`,
  `KUBE_CONTEXT=in-cluster`, and a namespaced Role granting exactly
  `pods` create/get/list/watch/delete, `pods/exec` create, `pods/log` get
  (see `docs/kubernetes-deployment.md`). Jobs scale via `runner.concurrency`.

## Runner-token bootstrap (RUNNER_AUTH=on)

1. If you did not provide `secrets.runnerToken`, the server generates a
   bootstrap token on first boot and logs it once:

   ```sh
   kubectl -n forge-ci logs deploy/forge-forge-ci-server | grep -i "bootstrap runner token"
   ```

2. Put that value in `secrets.runnerToken` (or mint one via
   `POST /api/v1/runner-tokens`) and `helm upgrade`. Runners pick it up from the
   Secret as `RUNNER_TOKEN`.

## Uninstall

```sh
helm uninstall forge -n forge-ci
```

PVCs (bundled Postgres, local-artifact persistence) and any externally-managed
Secret are not removed by `helm uninstall` — delete them explicitly if desired.

## Rendering / testing locally

```sh
helm lint deploy/helm/forge-ci
helm template forge deploy/helm/forge-ci
helm template forge deploy/helm/forge-ci -f deploy/helm/forge-ci/values-prod.yaml
```

For the full cloud story (EKS/GKE + managed DB + object storage via Terraform),
see [docs/cloud-deployment.md](../../../docs/cloud-deployment.md).
