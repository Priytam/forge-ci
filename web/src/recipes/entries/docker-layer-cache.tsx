import { LinearFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const dockerLayerCache: Recipe = {
  slug: "docker-layer-cache",
  title: "Docker — BuildKit registry layer cache",
  category: "Containers & registries",
  summary:
    "Reuse image layers between builds on any runner: BuildKit pulls cache from the registry (--cache-from) and pushes it back (--cache-to), so unchanged layers are never rebuilt.",
  features: ["services (dind)", "docker buildx", "resource_group", "$CI_COMMIT_REF_SLUG", "rules"],
  diagram: () => (
    <LinearFlow
      caption="Cache travels with the registry, not the runner"
      steps={[
        { label: "pull cache", sub: ":buildcache", tone: "commit" },
        { label: "build", sub: "buildx, only changed layers", tone: "build" },
        { label: "push", sub: "image + cache (mode=max)", tone: "deploy" },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        Runners are interchangeable, so a local Docker cache on one machine rarely helps the next
        build. A <strong>registry cache</strong> does: BuildKit stores every layer, including
        intermediate build stages (<code>mode=max</code>), in a separate <code>:buildcache</code>{" "}
        tag next to your image, and any runner reuses it.
      </p>
      <p>
        Each branch reads its own cache first and falls back to <code>main</code>'s, so a new
        feature branch starts warm. Only <code>main</code> writes the shared cache; branches write
        their own <code>buildcache-$CI_COMMIT_REF_SLUG</code> tag. Put cache-friendly steps first in
        your Dockerfile (copy lockfiles and install dependencies before copying source).
      </p>
      <p>
        <strong>Google Artifact Registry:</strong> skip the <code>docker login</code> line and run
        the job in an image with both <code>gcloud</code> and the Docker CLI. Point a Workload
        Identity cred-config at <code>$FORGE_OIDC_TOKEN_FILE</code> (docs/oidc.md), then{" "}
        <code>gcloud auth configure-docker REGION-docker.pkg.dev</code>. The credential helper
        fetches a fresh token on every push, and Forge keeps the token file current, so builds
        longer than an hour don't fail on an expired registry token.
      </p>
    </>
  ),
  yaml: `stages: [build]

variables:
  IMAGE: registry.example.com/acme/ecom

jobs:
  build-image:
    stage: build
    image: docker:27
    tags: [docker]
    services:
      - docker:27-dind
    variables:
      DOCKER_HOST: tcp://docker:2376
      DOCKER_TLS_CERTDIR: /certs
    # One build per branch at a time, so two pushes don't race on the cache tag.
    resource_group: build-$CI_COMMIT_REF_SLUG
    script:
      - echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin
      - docker buildx create --use --driver docker-container
      - |
        CACHE_TO="$IMAGE:buildcache-$CI_COMMIT_REF_SLUG"
        [ "$CI_COMMIT_BRANCH" = "main" ] && CACHE_TO="$IMAGE:buildcache"
        docker buildx build \\
          --cache-from type=registry,ref=$IMAGE:buildcache-$CI_COMMIT_REF_SLUG \\
          --cache-from type=registry,ref=$IMAGE:buildcache \\
          --cache-to type=registry,ref=$CACHE_TO,mode=max \\
          --tag "$IMAGE:$CI_COMMIT_SHORT_SHA" \\
          --push .
`,
};
