import { PipelineFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const monorepo: Recipe = {
  slug: "monorepo",
  title: "Monorepo — per-service matrix, fail-fast",
  category: "Scale & orchestration",
  summary:
    "Build and test each service in parallel via a matrix, each with its own lockfile-keyed cache, cancelling the run on first failure.",
  features: ["parallel: matrix", "fail_fast", "cache (files)", "tags", "needs (fan-in)"],
  diagram: () => (
    <PipelineFlow
      caption="Fan out per service, fail fast"
      columns={[
        {
          title: "build",
          nodes: [
            { label: "web", tone: "build" },
            { label: "api", tone: "build" },
            { label: "worker", tone: "build" },
          ],
        },
        {
          title: "test",
          nodes: [
            { label: "web", tone: "test" },
            { label: "api", tone: "test" },
            { label: "worker", tone: "test" },
          ],
        },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        In a monorepo each service builds and tests independently, but you do not
        want to burn runner minutes on the remaining services once one has clearly
        broken.
      </p>
      <p>
        A <code>parallel: matrix</code> over <code>SERVICE</code> expands both the{" "}
        <code>build</code> and <code>test</code> jobs into one instance per
        service. Each instance caches its own{" "}
        <code>services/&lt;name&gt;/node_modules</code>, keyed on that service's
        lockfile with a per-service <code>prefix</code>, so caches never collide.
        The interpolated paths are quoted so YAML does not read{" "}
        <code>$&#123;SERVICE&#125;</code> as a flow map.
      </p>
      <p>
        Top-level <code>fail_fast: true</code> makes the scheduler cancel every
        other non-terminal job the moment one genuinely fails (allowed-failure
        jobs never trip it). <code>needs: [build]</code> fans in — each{" "}
        <code>test</code> instance waits on the build matrix — and{" "}
        <code>tags: [linux]</code> pins the work to the right runner pool.
      </p>
    </>
  ),
  yaml: `fail_fast: true

stages: [build, test]

default:
  image: node:20
  tags: [linux]

jobs:
  build:
    stage: build
    parallel:
      matrix:
        - SERVICE: [web, api, worker]
    script:
      - cd "services/$SERVICE"
      - npm ci
      - npm run build
    cache:
      key:
        files: ["services/\${SERVICE}/package-lock.json"]
        prefix: "\${SERVICE}"
      paths: ["services/\${SERVICE}/node_modules"]
      policy: pull-push

  test:
    stage: test
    needs: [build]
    parallel:
      matrix:
        - SERVICE: [web, api, worker]
    script:
      - cd "services/$SERVICE"
      - npm test
    cache:
      key:
        files: ["services/\${SERVICE}/package-lock.json"]
        prefix: "\${SERVICE}"
      paths: ["services/\${SERVICE}/node_modules"]
      policy: pull
`,
};
