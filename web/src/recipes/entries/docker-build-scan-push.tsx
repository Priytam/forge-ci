import { LinearFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const dockerBuildScanPush: Recipe = {
  slug: "docker-build-scan-push",
  title: "Docker — build, scan, push on main",
  category: "Containers & registries",
  summary:
    "Build an image with docker-in-docker and a BuildKit registry layer cache, block on a Trivy container scan, and push to the registry only on main.",
  features: [
    "include: security/container",
    "services (dind)",
    "docker buildx",
    "rules: variables",
    "tags",
    "artifacts",
  ],
  diagram: () => (
    <LinearFlow
      caption="Build → scan → gated push"
      steps={[
        { label: "build", sub: "docker + dind", tone: "build" },
        { label: "scan", sub: "trivy (blocking)", tone: "scan" },
        { label: "push", sub: "main only", tone: "deploy" },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        The classic container delivery flow: build the image, fail the pipeline
        if it ships a known-vulnerable package, and only publish trusted images
        from your default branch.
      </p>
      <p>
        <code>include: security/container</code> pulls in Forge's built-in Trivy
        job with zero setup. By default that scan is{" "}
        <code>allow_failure: true</code> (advisory); here we override it to{" "}
        <code>allow_failure: false</code> so a finding actually blocks the push.
        Point it at the image you build by setting the <code>SCAN_IMAGE</code>{" "}
        variable.
      </p>
      <p>
        Build and push run on a runner tagged <code>privileged</code> with a{" "}
        <code>docker:27-dind</code> service for the daemon. dind only starts in
        a privileged container, so that runner sets{" "}
        <code>RUNNER_PRIVILEGED_SERVICES=true</code> (docs/runners.md); keep it
        to runners dedicated to image builds.
      </p>
      <p>
        Every job gets a fresh daemon with an empty layer cache, so the build
        uses BuildKit&apos;s registry cache: it reads{" "}
        <code>$IMAGE:buildcache</code> with <code>--cache-from</code> on every
        branch and only <code>main</code> writes it back with{" "}
        <code>--cache-to</code> (set by the rule&apos;s variables). Unchanged
        layers come from the registry instead of being rebuilt. Order the
        Dockerfile so dependency installs come before copying the source. A
        missing cache is a warning, not a failure. The push job&apos;s{" "}
        <code>rules</code> restrict it to{" "}
        <code>$CI_COMMIT_BRANCH == "main"</code> — feature branches build and
        scan but never publish. Registry credentials come from masked, protected
        repo variables (<code>REGISTRY_USER</code> /{" "}
        <code>REGISTRY_PASSWORD</code>).
      </p>
    </>
  ),
  yaml: `include:
  - template: security/container

stages: [build, test, push]

variables:
  IMAGE: registry.example.com/acme/web
  SCAN_IMAGE: registry.example.com/acme/web:\${CI_COMMIT_REF_NAME}

jobs:
  build-image:
    stage: build
    image: docker:27
    tags: [docker, privileged]
    services:
      # dind on the job network as "docker"; plain TCP, the network is per job
      - name: docker:27-dind
        alias: docker
        env: { DOCKER_TLS_CERTDIR: "" }
    variables:
      DOCKER_HOST: tcp://docker:2375
      CACHE: type=registry,ref=registry.example.com/acme/web:buildcache
    rules:
      # only main writes the shared layer cache; every branch reads it
      - if: '$CI_COMMIT_BRANCH == "main"'
        variables: { CACHE_TO: "--cache-to type=registry,ref=registry.example.com/acme/web:buildcache,mode=max" }
      - when: on_success
    script:
      - until docker info >/dev/null 2>&1; do sleep 1; done
      - if [ -n "$REGISTRY_PASSWORD" ]; then echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin; fi
      - docker buildx create --use --driver docker-container
      - docker buildx build --load --cache-from "$CACHE" $CACHE_TO -t "$IMAGE:$CI_COMMIT_REF_NAME" .
      - docker save "$IMAGE:$CI_COMMIT_REF_NAME" -o image.tar
    artifacts:
      paths: [image.tar]
      expire_in: 1h

  container-scan:
    stage: test
    allow_failure: false

  push-image:
    stage: push
    image: docker:27
    tags: [docker, privileged]
    services:
      - name: docker:27-dind
        alias: docker
        env: { DOCKER_TLS_CERTDIR: "" }
    variables:
      DOCKER_HOST: tcp://docker:2375
    needs: [build-image, container-scan]
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        when: on_success
      - when: never
    script:
      - until docker info >/dev/null 2>&1; do sleep 1; done
      - docker load -i image.tar
      - echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin
      - docker push "$IMAGE:$CI_COMMIT_REF_NAME"
`,
};
