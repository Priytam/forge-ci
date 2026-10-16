# Docker layer caching

Two complementary ways to avoid rebuilding unchanged image layers.

## 1. Registry cache (works on any runner)

BuildKit can store its layer cache in the registry next to the image and pull
it on the next build — whichever runner that is. This is the recommended
default; the full pipeline is the **Docker — BuildKit registry layer cache**
recipe (Recipes page).

```sh
docker buildx create --use --driver docker-container
docker buildx build \
  --cache-from type=registry,ref=$IMAGE:buildcache-$CI_COMMIT_REF_SLUG \
  --cache-from type=registry,ref=$IMAGE:buildcache \
  --cache-to   type=registry,ref=$IMAGE:buildcache,mode=max \
  --tag "$IMAGE:$CI_COMMIT_SHORT_SHA" --push .
```

- `mode=max` also caches intermediate stages of multi-stage builds.
- Read the branch's own cache first, then `main`'s, so new branches start warm;
  only `main` writes the shared `:buildcache` tag.
- Use `resource_group: build-$CI_COMMIT_REF_SLUG` so two builds of one branch
  don't race on the cache tag.
- Order the Dockerfile for caching: copy lockfiles and install dependencies
  before copying the rest of the source.

### Registry credentials for long builds

Any OCI registry works: GHCR, GitLab Container Registry, Docker Hub, Harbor,
Quay, Gitea's registry, AWS ECR, Google Artifact Registry, Azure ACR, a
self-hosted `registry:2`. A `docker login` with a short-lived access token can
expire mid-build, so prefer, in order:

1. A **long-lived registry credential** stored as a masked, protected variable
   (`REGISTRY_USER` / `REGISTRY_PASSWORD`) — simplest, works everywhere.
2. A **credential helper** that fetches a token per request, fed by Forge's
   refreshing OIDC token file (docs/oidc.md):
   - AWS ECR — `amazon-ecr-credential-helper`, with `AWS_ROLE_ARN` and
     `AWS_WEB_IDENTITY_TOKEN_FILE` (Forge sets it to the refreshing file);
   - Google Artifact Registry — a Workload Identity cred-config with
     `--credential-source-file="$FORGE_OIDC_TOKEN_FILE"`, then
     `gcloud auth configure-docker REGION-docker.pkg.dev`;
   - Azure ACR — `az login --federated-token "$(cat "$FORGE_OIDC_TOKEN_FILE")"`
     then `az acr login`; repeat the login before a long push if needed;
   - Harbor / Quay / registries trusting an OIDC issuer — exchange the token
     per their docs.

## 2. Persistent runner cache

For the very fastest rebuilds on dedicated build machines, see
docs/runners.md → *Persistent BuildKit cache and cache affinity*.
