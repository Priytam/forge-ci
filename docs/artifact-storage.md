# Artifact storage: local, MinIO, AWS S3, or GCS

Runners archive the `artifacts.paths` of a successful job as `artifacts.tar.gz`
and stream it to the control plane, which writes it to the configured blob
store. The backend is chosen with environment variables on **forge-server** —
runners need no storage credentials (they never talk to the store directly).

| Env var | Meaning | Default |
|---|---|---|
| `ARTIFACT_STORE` | `local` or `s3` | `local` |
| `ARTIFACTS_DIR` | local backend: directory for blobs | `data/artifacts` |
| `S3_ENDPOINT` | s3 backend: host[:port], no scheme | — |
| `S3_BUCKET` | bucket name | — |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | credentials | — |
| `S3_USE_SSL` | `true`/`false` | `true` |
| `S3_REGION` | optional region | `""` |
| `MAX_ARTIFACT_BYTES` | reject uploads larger than this (`413`); `0` disables | `524288000` (500 MiB) |

The `s3` backend speaks the S3 API and therefore covers AWS S3, MinIO, and
Google Cloud Storage (interoperability mode) with the same configuration.

## Size cap and retention GC

- **`MAX_ARTIFACT_BYTES`** caps a single artifact upload. Oversize uploads are
  rejected with `413` and the partial blob is deleted, so a runaway job cannot
  fill the store.
- **`MAX_JOB_LOG_BYTES`** (default `10485760`, 10 MiB) caps cumulative job log
  bytes: once exceeded, a single truncation notice is written and further log
  chunks are dropped.
- **`RETENTION_DAYS`** (default `30`, `0` = keep forever) drives an hourly
  retention GC on the server: expired login sessions are always collected, and
  pipelines older than the window are deleted (cascading their jobs, logs and
  artifact rows) **and their artifact blobs are removed from the blob store**.

## Option A — local disk (default, dev)

Nothing to configure. Blobs land in `data/artifacts/job-<id>/`. Suitable for a
single-server dev setup only.

## Option B — MinIO (self-hosted)

1. Run MinIO and create a bucket:
   ```sh
   docker run -d --name minio -p 9000:9000 -p 9001:9001 \
     -e MINIO_ROOT_USER=forge -e MINIO_ROOT_PASSWORD=forge-secret-1 \
     quay.io/minio/minio server /data --console-address ":9001"
   ```
2. Open the console at http://localhost:9001, log in, and create bucket
   `forge-artifacts`. (Or: `mc alias set local http://localhost:9000 forge forge-secret-1 && mc mb local/forge-artifacts`.)
3. Create a dedicated access key: Console → Access Keys → Create. Copy the key/secret.
4. Start forge-server with:
   ```sh
   ARTIFACT_STORE=s3 \
   S3_ENDPOINT=localhost:9000 \
   S3_BUCKET=forge-artifacts \
   S3_ACCESS_KEY=<key> S3_SECRET_KEY=<secret> \
   S3_USE_SSL=false \
   ./bin/forge-server
   ```
5. Verify: run a pipeline with artifacts, then check the bucket:
   `mc ls local/forge-artifacts/job-<id>/`.

## Option C — AWS S3

1. Create the bucket (pick your region):
   ```sh
   aws s3api create-bucket --bucket <org>-forge-artifacts --region ap-south-1 \
     --create-bucket-configuration LocationConstraint=ap-south-1
   ```
2. Create an IAM user (or role) with least privilege — policy limited to the
   bucket: `s3:PutObject`, `s3:GetObject`, `s3:HeadObject` on
   `arn:aws:s3:::<org>-forge-artifacts/*`.
3. Generate an access key for that principal (IAM → Security credentials).
4. Start forge-server with:
   ```sh
   ARTIFACT_STORE=s3 \
   S3_ENDPOINT=s3.ap-south-1.amazonaws.com \
   S3_REGION=ap-south-1 \
   S3_BUCKET=<org>-forge-artifacts \
   S3_ACCESS_KEY=<key> S3_SECRET_KEY=<secret> \
   ./bin/forge-server
   ```
5. Recommended bucket hygiene: enable default encryption (SSE-S3) and add a
   lifecycle rule expiring `job-*` prefixes after e.g. 30 days — Forge does
   not garbage-collect artifacts yet.

## Option D — Google Cloud Storage (interoperability mode)

GCS exposes an S3-compatible XML API with HMAC keys.

1. Create the bucket:
   ```sh
   gcloud storage buckets create gs://<org>-forge-artifacts --location=asia-south1
   ```
2. Create a service account and grant it `roles/storage.objectAdmin` **on that
   bucket only**:
   ```sh
   gcloud iam service-accounts create forge-artifacts
   gcloud storage buckets add-iam-policy-binding gs://<org>-forge-artifacts \
     --member=serviceAccount:forge-artifacts@<project>.iam.gserviceaccount.com \
     --role=roles/storage.objectAdmin
   ```
3. Create an HMAC key for the service account (this is what makes S3-mode work):
   ```sh
   gcloud storage hmac create forge-artifacts@<project>.iam.gserviceaccount.com
   ```
   Note the `accessId` (S3_ACCESS_KEY) and `secret` (S3_SECRET_KEY).
4. Start forge-server with:
   ```sh
   ARTIFACT_STORE=s3 \
   S3_ENDPOINT=storage.googleapis.com \
   S3_BUCKET=<org>-forge-artifacts \
   S3_ACCESS_KEY=<accessId> S3_SECRET_KEY=<secret> \
   ./bin/forge-server
   ```

## Declaring artifacts in a job

```yaml
build-app:
  stage: build
  artifacts:
    paths: [dist/, report.txt]   # workspace-relative; missing paths are skipped
  script: [make build]
```

Download via UI (repo → pipeline → job) or API:
`GET /api/v1/artifacts?repo=<repo>` then `GET /api/v1/artifacts/{id}/download`.
