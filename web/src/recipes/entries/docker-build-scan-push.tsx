import { LinearFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const dockerBuildScanPush: Recipe = {
  slug: "docker-build-scan-push",
  title: "Docker — build, scan, push on main",
  category: "Containers & registries",
  summary:
    "Build an image with docker-in-docker, block on a Trivy container scan, and push to the registry only on main.",
  features: ["include: security/container", "services (dind)", "tags", "rules", "artifacts"],
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
        Build and push run on a <code>docker</code>-tagged runner with a{" "}
        <code>docker:27-dind</code> service for the daemon. The push job's{" "}
        <code>rules</code> restrict it to{" "}
        <code>$CI_COMMIT_BRANCH == "main"</code> — feature branches build and
        scan but never publish. Registry credentials come from masked, protected
        repo variables (<code>REGISTRY_USER</code> / <code>REGISTRY_PASSWORD</code>).
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
    tags: [docker]
    services:
      - docker:27-dind
    variables:
      DOCKER_HOST: tcp://docker:2376
      DOCKER_TLS_CERTDIR: /certs
    script:
      - docker build -t "$IMAGE:$CI_COMMIT_REF_NAME" .
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
    tags: [docker]
    services:
      - docker:27-dind
    variables:
      DOCKER_HOST: tcp://docker:2376
      DOCKER_TLS_CERTDIR: /certs
    needs: [build-image, container-scan]
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        when: on_success
      - when: never
    script:
      - docker load -i image.tar
      - echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin
      - docker push "$IMAGE:$CI_COMMIT_REF_NAME"
`,
};
