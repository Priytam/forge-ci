# OIDC / keyless cloud authentication

Forge issues a **short-lived, signed OpenID Connect ID token per job** so that a
job can exchange it for credentials with **no static keys** stored anywhere.
It is a standard OIDC issuer, so it works with **anything that trusts one** —
the examples below are AWS, GCP, Azure and HashiCorp Vault; Kubernetes
(service account issuer discovery), Cloudflare, Terraform Cloud and others
follow the same pattern:

| Target | Exchange |
|---|---|
| AWS | `sts assume-role-with-web-identity` (`AWS_WEB_IDENTITY_TOKEN_FILE`) |
| GCP | Workload Identity Federation (`--credential-source-file`) |
| Azure | Entra ID workload identity federation (federated credential on an app / managed identity) |
| HashiCorp Vault | JWT/OIDC auth method (`vault write auth/jwt/login`) |

Inside every job the token is in `$FORGE_OIDC_TOKEN` and, kept fresh for the
whole job, in the file `$FORGE_OIDC_TOKEN_FILE` — always point credential
configs at the file. (Forge uses the same path itself: runners cloning AWS
CodeCommit sign the clone with credentials from this token —
[codecommit.md](codecommit.md).)

The cloud provider validates the token against Forge's **public** JWKS (fetched
over the OIDC discovery document). The whole trust chain is a public key: Forge
never holds cloud credentials, and the job never holds a long-lived secret. The
token lives ~15 minutes and is scoped to one job.

## What Forge issues

On every job acquire, Forge mints an RS256 JWT and injects it into the job
environment as two variables (same value):

| Env var | Notes |
| --- | --- |
| `FORGE_OIDC_TOKEN` | the ID token |
| `CI_JOB_JWT` | GitLab-style alias |

The token is a secret-equivalent: it is added to the job's redaction set, so it
is **masked from job logs** (`echo $FORGE_OIDC_TOKEN` prints `[REDACTED]`, never
the raw JWT).

The token is always minted; a job that doesn't use keyless cloud auth simply
ignores it (harmless, no configuration needed).

## Endpoints (public, auth-exempt)

These serve at the **root** well-known paths and are reachable without a Forge
session even when SSO enforcement is on, because AWS/GCP fetch them anonymously:

| Endpoint | Purpose |
| --- | --- |
| `GET {EXTERNAL_URL}/.well-known/openid-configuration` | OIDC discovery: `issuer`, `jwks_uri`, supported alg/response/subject types, claims |
| `GET {EXTERNAL_URL}/.well-known/jwks.json` | the RSA public key as a JWK (`kid`, `kty=RSA`, `use=sig`, `alg=RS256`, `n`, `e`) |

`EXTERNAL_URL` is the token **issuer** and must be **publicly reachable by
AWS/GCP** so they can fetch the JWKS. The `jwks_uri` in the discovery doc always
points at wherever JWKS is actually served, so cloud providers follow it
automatically.

Example discovery document:

```json
{
  "issuer": "https://forge.example.com",
  "jwks_uri": "https://forge.example.com/.well-known/jwks.json",
  "response_types_supported": ["id_token"],
  "subject_types_supported": ["public"],
  "id_token_signing_alg_values_supported": ["RS256"],
  "claims_supported": ["iss","aud","sub","iat","nbf","exp","jti",
                       "repo","ref","sha","pipeline_id","job_id","environment"],
  "scopes_supported": ["openid"]
}
```

## Token claims

| Claim | Value |
| --- | --- |
| `iss` | `EXTERNAL_URL` (must match the discovery `issuer`) |
| `aud` | `OIDC_AUDIENCE` (default `forge-ci`) — set per cloud (see below) |
| `sub` | `repo:{repo}:ref:{ref}` — with `:environment:{env}` appended for an environment-targeting job |
| `iat`, `nbf` | issued/not-before (backdated 60s for clock skew) |
| `exp` | `iat` + 15 minutes |
| `jti` | unique token id |
| `repo`, `ref`, `sha` | the pipeline's repo, ref, and commit |
| `pipeline_id`, `job_id` | numeric ids |
| `environment` | the job's deploy environment (empty when none) |

`sub` examples:

```
repo:acme/widgets:ref:refs/heads/main
repo:acme/widgets:ref:refs/heads/main:environment:production
```

Cloud trust policies condition on `aud` and `sub` (and can map the custom
claims), so you can, e.g., allow only `main` to assume a production role.

## Signing key

The RSA-2048 signing key is resolved in this order:

1. **`OIDC_PRIVATE_KEY`** (PEM, PKCS#1 or PKCS#8) from the env — when set it
   always wins and nothing is persisted (operator-managed key).
2. the persisted key in the `oidc_keys` table (decrypted with
   `FORGE_SECRET_KEY`).
3. otherwise Forge **generates an RSA-2048 key on first start and persists it**
   (encrypted at rest, `enc:v1:` prefix) so the published JWKS and its `kid`
   stay **stable across restarts**.

The `kid` is derived deterministically from the public key, so a given key
always yields the same `kid`.

## Configuration

| Env var | Purpose | Default |
| --- | --- | --- |
| `EXTERNAL_URL` | token issuer + JWKS origin; must be publicly reachable by AWS/GCP | `http://localhost:8080` |
| `OIDC_AUDIENCE` | the `aud` claim | `forge-ci` |
| `OIDC_PRIVATE_KEY` | PEM signing key override; else generated + persisted | — (generated) |
| `FORGE_SECRET_KEY` | encrypts the persisted signing key at rest | — (passthrough) |

---

## AWS setup (STS AssumeRoleWithWebIdentity)

*One example of many — the Azure and Vault sections below follow the same shape.*

Forge must be reachable at a stable HTTPS `EXTERNAL_URL`. Set
`OIDC_AUDIENCE=sts.amazonaws.com` (AWS expects that audience).

1. **Create an IAM OIDC identity provider**

   ```sh
   aws iam create-open-id-connect-provider \
     --url "https://forge.example.com" \
     --client-id-list "sts.amazonaws.com" \
     --thumbprint-list "<tls-cert-thumbprint>"
   ```

   The provider URL is your `EXTERNAL_URL`; the client-id is the token `aud`. The
   thumbprint is the SHA-1 of the root CA of the TLS cert serving your JWKS (AWS
   docs: "Obtaining the thumbprint for an OIDC IdP").

2. **Create an IAM role with a trust policy** conditioning on `aud` and `sub`:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [{
       "Effect": "Allow",
       "Principal": { "Federated": "arn:aws:iam::<acct>:oidc-provider/forge.example.com" },
       "Action": "sts:AssumeRoleWithWebIdentity",
       "Condition": {
         "StringEquals": {
           "forge.example.com:aud": "sts.amazonaws.com",
           "forge.example.com:sub": "repo:acme/widgets:ref:refs/heads/main:environment:production"
         }
       }
     }]
   }
   ```

3. **Job snippet** — exchange the token for temporary credentials:

   ```yaml
   jobs:
     deploy:
       image: amazon/aws-cli
       environment: production
       script:
         - >
           creds=$(aws sts assume-role-with-web-identity
           --role-arn arn:aws:iam::<acct>:role/forge-deploy
           --role-session-name "forge-$CI_JOB_ID"
           --web-identity-token "$FORGE_OIDC_TOKEN"
           --query Credentials --output json)
         - export AWS_ACCESS_KEY_ID=$(echo "$creds" | jq -r .AccessKeyId)
         - export AWS_SECRET_ACCESS_KEY=$(echo "$creds" | jq -r .SecretAccessKey)
         - export AWS_SESSION_TOKEN=$(echo "$creds" | jq -r .SessionToken)
         - aws s3 ls
   ```

---

## GCP setup (Workload Identity Federation)

Set `OIDC_AUDIENCE` to the full provider audience GCP expects (the
`//iam.googleapis.com/projects/.../providers/<id>` resource, or a custom
audience you configure on the provider).

1. **Create a Workload Identity Pool + OIDC provider** pointing at the issuer,
   mapping claims to attributes:

   ```sh
   gcloud iam workload-identity-pools create forge-pool \
     --location=global --display-name="Forge CI"

   gcloud iam workload-identity-pools providers create-oidc forge-provider \
     --location=global --workload-identity-pool=forge-pool \
     --issuer-uri="https://forge.example.com" \
     --allowed-audiences="//iam.googleapis.com/projects/<num>/locations/global/workloadIdentityPools/forge-pool/providers/forge-provider" \
     --attribute-mapping="google.subject=assertion.sub,attribute.repo=assertion.repo,attribute.ref=assertion.ref,attribute.environment=assertion.environment"
   ```

2. **Grant a service account** to a principal set filtered on the mapped
   attributes (e.g. only a given repo):

   ```sh
   gcloud iam service-accounts add-iam-policy-binding deploy@<proj>.iam.gserviceaccount.com \
     --role=roles/iam.workloadIdentityUser \
     --member="principalSet://iam.googleapis.com/projects/<num>/locations/global/workloadIdentityPools/forge-pool/attribute.repo/acme/widgets"
   ```

3. **Job snippet** — write the token to a file and use a credential config:

   ```yaml
   jobs:
     deploy:
       image: google/cloud-sdk
       script:
         - >
           gcloud iam workload-identity-pools create-cred-config
           projects/<num>/locations/global/workloadIdentityPools/forge-pool/providers/forge-provider
           --service-account=deploy@<proj>.iam.gserviceaccount.com
           --credential-source-file="$FORGE_OIDC_TOKEN_FILE"
           --credential-source-type=text
           --output-file=/tmp/gcp-creds.json
         - export GOOGLE_APPLICATION_CREDENTIALS=/tmp/gcp-creds.json
         - gcloud auth login --cred-file=/tmp/gcp-creds.json
         - gcloud storage ls
   ```

---

## Azure setup (workload identity federation)

1. In Entra ID, on an app registration (or user-assigned managed identity) →
   **Federated credentials → Add → Other issuer**:
   - Issuer: Forge's `EXTERNAL_URL`
   - Subject: e.g. `repo:acme/widgets:ref:refs/heads/main:environment:production`
   - Audience: set `OIDC_AUDIENCE=api://AzureADTokenExchange` on the server
     (Azure's default audience for federation).
2. Grant that identity the Azure roles the job needs.
3. In the job:

   ```yaml
   deploy:
     image: mcr.microsoft.com/azure-cli
     script:
       - az login --service-principal -u "$AZURE_CLIENT_ID" -t "$AZURE_TENANT_ID"
           --federated-token "$(cat "$FORGE_OIDC_TOKEN_FILE")"
       - az account show
   ```

   SDKs that support `AZURE_FEDERATED_TOKEN_FILE` refresh by re-reading it:
   set `AZURE_FEDERATED_TOKEN_FILE=$FORGE_OIDC_TOKEN_FILE`.

## HashiCorp Vault setup (JWT auth)

```sh
vault auth enable jwt
vault write auth/jwt/config oidc_discovery_url="$FORGE_EXTERNAL_URL" bound_issuer="$FORGE_EXTERNAL_URL"
vault write auth/jwt/role/deploy role_type=jwt user_claim=sub \
  bound_audiences=forge-ci \
  bound_claims='{"repo":"acme/widgets","environment":"production"}' \
  policies=deploy ttl=15m
```

In the job:

```sh
export VAULT_TOKEN=$(vault write -field=token auth/jwt/login role=deploy jwt=@"$FORGE_OIDC_TOKEN_FILE")
vault kv get secret/deploy/db
```

## Notes & limitations

- The token is **short-lived (15 min), per-job, and never a static key**.
- **Long builds:** the runner keeps a fresh token in the file named by
  `$FORGE_OIDC_TOKEN_FILE`, re-minting it every 5 minutes for as long as the
  job runs (`POST /api/v1/runner/jobs/{id}/oidc-token`). Point credential
  configs at the **file**, not the `$FORGE_OIDC_TOKEN` variable (which is fixed
  at job start): GCP's `--credential-source-file="$FORGE_OIDC_TOKEN_FILE"` and
  the AWS SDKs' `AWS_WEB_IDENTITY_TOKEN_FILE` (set to the same file unless the
  job sets it) re-read it on every refresh, so a 3-hour build keeps working.
  For registry pushes, use `gcloud auth configure-docker <region>-docker.pkg.dev`
  (a credential helper that asks gcloud for a token on every push) rather than
  `docker login` with a one-off access token. The file lives at
  `/var/run/forge/oidc-token` in docker/kubernetes jobs and in a per-job temp
  directory for shell jobs; needs runner and server from this release.
- `EXTERNAL_URL` **must be publicly reachable** by AWS/GCP for the JWKS fetch —
  a private/localhost issuer cannot be used for real cloud exchange.
- v1 uses a **single signing key** (no rotation/overlap yet). To rotate, replace
  `OIDC_PRIVATE_KEY` (or the `oidc_keys` row) — cloud providers re-fetch the
  JWKS and pick up the new `kid` on their cache cycle.
- The AWS/GCP credential *exchange* itself requires a real cloud account and is
  not exercised by Forge's test suite; the token minting and its cryptographic
  validity against the published JWKS are verified (see
  `internal/oidc/oidc_test.go`).
