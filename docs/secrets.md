# External secrets and registry login

Forge stores repo variables encrypted at rest, and that is enough for many
teams. When secrets already live in a secret manager, a job can fetch them
**at run time, on the runner, with its own identity** — nothing is copied
into Forge, rotation happens in one place, and the manager's audit log shows
which job read what.

Identity is the job's **OIDC token** ([oidc.md](oidc.md)) wherever the manager
can trust it, so there are no static cloud keys anywhere: Vault's JWT auth,
AWS STS web identity, GCP and Azure workload identity federation. A runner
without a token (OIDC disabled) or a manager not set up for it falls back to
the **runner's own** ambient identity (`VAULT_TOKEN`, the AWS credential
chain, `GOOGLE_OAUTH_ACCESS_TOKEN`, `AZURE_ACCESS_TOKEN`).

Each manager is a provider in `internal/secrets/`; adding another is adding
one file.

## `secrets:` — fetch into variables

```yaml
deploy:
  stage: deploy
  secrets:
    DB_PASSWORD: {vault: "ci/prod/db#password"}               # KV v2, mount "secret"
    TLS_KEY:     {vault: "ci/prod/tls#key@kv", file: true}    # @mount; file: path in a variable
    API_KEY:     {aws: "prod/api#key"}                        # Secrets Manager, JSON key
    SA_JSON:     {gcp: "projects/my-proj/secrets/deployer", file: true}
    CERT:        {azure: "https://acme-ci.vault.azure.net/secrets/deploy-cert"}
  script:
    - psql "postgres://app:$DB_PASSWORD@db/app" -c 'select 1'
    - gcloud auth activate-service-account --key-file "$SA_JSON"
```

Secrets are fetched **after** checkout, cache and artifact restore and
**before** the script. One failure fails the job before it starts (a job
never runs with half its secrets) with the manager's error in the log —
never a value. Every fetched value is **masked** in the log, like a masked
variable. `file: true` writes the value to a file in the job's run directory
and puts the **path** in the variable — for keys, certificates and JSON
credentials. Secrets merge through `extends`/`include` by name (child wins).

| Provider | Reference | Settings (job variables, else runner env) |
|---|---|---|
| `vault` | `path#field` or `path#field@mount` (KV v2; mount default `secret`) | `VAULT_ADDR`; `VAULT_NAMESPACE`; auth: `VAULT_TOKEN`, or JWT: `VAULT_AUTH_ROLE` (+ `VAULT_AUTH_PATH`, default `jwt`) |
| `aws` | secret name or ARN, optional `#json-key` | `AWS_REGION`; `AWS_ROLE_ARN` to assume with the job token, else the runner's credential chain |
| `gcp` | `projects/P/secrets/S[/versions/V]`, or `S` with `GCP_PROJECT` | `GCP_WORKLOAD_IDENTITY_PROVIDER` (+ `GCP_SERVICE_ACCOUNT` to impersonate), else `GOOGLE_OAUTH_ACCESS_TOKEN` |
| `azure` | `https://<vault>.vault.azure.net/secrets/<name>[/<version>]` | `AZURE_TENANT_ID` + `AZURE_CLIENT_ID` (federated credential), else `AZURE_ACCESS_TOKEN` |

Set the settings once as repo variables (they aren't secret) and the
trust on the manager's side as in [oidc.md](oidc.md): a Vault JWT role, an
AWS role trust policy, a GCP workload identity pool, an Azure federated
credential — each bound to Forge's issuer and the job's `sub`/`aud` claims,
so only the repos and environments you name can read the secret.

Any other manager (1Password, Doppler, Infisical, CyberArk…) works the way it
always has — call its CLI in the script with a token from a masked variable —
until it gets a provider.

## `registry_login:` — pull and push from private registries

```yaml
build:
  stage: build
  image: 123456789012.dkr.ecr.eu-west-1.amazonaws.com/ci/builder:1   # pulled with the login
  registry_login:
    - 123456789012.dkr.ecr.eu-west-1.amazonaws.com   # ECR     (AWS_ROLE_ARN)
    - europe-docker.pkg.dev                          # Artifact Registry / gcr.io (GCP_WORKLOAD_IDENTITY_PROVIDER)
    - acme.azurecr.io                                # ACR     (AZURE_TENANT_ID / AZURE_CLIENT_ID)
    - ghcr.io                                        # anything else: from DOCKER_AUTH_CONFIG
  script:
    - docker buildx build --push -t europe-docker.pkg.dev/my-proj/app/api:$CI_COMMIT_SHORT_SHA .
```

Before the job runs, the runner builds a Docker `config.json` from the
**`DOCKER_AUTH_CONFIG`** variable (the standard `{"auths": {...}}` document —
Docker Hub, GHCR, Quay, Harbor, GitLab, Nexus, any OCI registry) plus a
**minted login** for each cloud registry listed, using the job's identity.
The **docker executor pulls the job image and service images with it**, and
the job sees it as `DOCKER_CONFIG`, so `docker push`, `buildx`, `crane`,
`skopeo` and `helm registry` inside the job authenticate without a
`docker login` line. Minted passwords are masked in the log and expire on
their own (ECR 12h, Artifact Registry 1h, ACR 3h).

A registry listed without a cloud recipe and without an entry in
`DOCKER_AUTH_CONFIG` fails the job with a message saying so. The kubernetes
executor pulls the job image with the cluster's own `imagePullSecrets`; the
login still applies inside the job.

## Where values go

Fetched secrets and the Docker config live in the job's **run directory**
(`/var/run/forge` inside containers, a temp dir on the shell executor),
mounted read-only and deleted when the job ends. They are never sent back to
the server and never written to the workspace, so they can't end up in an
artifact by accident.
