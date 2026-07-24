import { LinearFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const gitopsKubernetes: Recipe = {
  slug: "gitops-kubernetes",
  title: "Deploy to Kubernetes with rollback",
  category: "Deploy & release",
  summary:
    "Build and push an image, then roll it out to a protected prod cluster with kubectl — revert from the environments board.",
  features: ["environment", "services (dind)", "rules", "needs", "tags"],
  diagram: () => (
    <LinearFlow
      caption="Build/push → kubectl rollout"
      steps={[
        { label: "build & push", sub: "image:$SHA", tone: "build" },
        { label: "rollout", sub: "prod · kubectl", tone: "deploy" },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        A rolling deploy to Kubernetes: build an immutable image tagged by commit
        SHA, push it, and update the deployment so the cluster pulls the new tag.
      </p>
      <p>
        The <code>deploy-k8s</code> job targets the protected{" "}
        <code>production</code> environment (approval gate) and runs{" "}
        <code>kubectl set image</code> followed by{" "}
        <code>kubectl rollout status</code> so the job only succeeds once the new
        pods are healthy. Cluster access comes from a mounted kubeconfig context;
        image tags are the commit SHA so every deploy is traceable.
      </p>
      <p>
        Because each deploy is recorded against the <code>production</code>{" "}
        environment, the <strong>Environments board</strong> keeps the deployment
        history — a bad release is reverted by re-running the last good deployment
        from that board (or <code>kubectl rollout undo</code>), no config change
        required.
      </p>
    </>
  ),
  yaml: `stages: [build, deploy]

variables:
  IMAGE: registry.example.com/acme/web

jobs:
  build-push:
    stage: build
    image: docker:27
    tags: [docker]
    services:
      - docker:27-dind
    variables:
      DOCKER_HOST: tcp://docker:2376
      DOCKER_TLS_CERTDIR: /certs
    script:
      - docker build -t "$IMAGE:$CI_COMMIT_SHA" .
      - echo "$REGISTRY_PASSWORD" | docker login "$REGISTRY" -u "$REGISTRY_USER" --password-stdin
      - docker push "$IMAGE:$CI_COMMIT_SHA"

  deploy-k8s:
    stage: deploy
    image: bitnami/kubectl:1.30
    needs: [build-push]
    environment: production
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        when: on_success
      - when: never
    script:
      - kubectl config use-context prod
      - kubectl -n web set image deployment/web web="$IMAGE:$CI_COMMIT_SHA"
      - kubectl -n web rollout status deployment/web --timeout=120s
`,
};
