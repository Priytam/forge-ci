import { PipelineFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const nodejsApp: Recipe = {
  slug: "nodejs-app",
  title: "Node.js app — matrix test with a warm cache",
  category: "Languages & build",
  summary:
    "npm ci once, then run the suite across Node 18/20/22 in parallel, sharing a node_modules cache keyed on the lockfile.",
  features: ["cache (files)", "parallel: matrix", "needs", "artifacts", "reports.junit"],
  diagram: () => (
    <PipelineFlow
      caption="Install once, fan out over Node versions"
      columns={[
        { title: "install", nodes: [{ label: "npm ci", sub: "warm cache", tone: "build" }] },
        {
          title: "test",
          nodes: [
            { label: "Node 18", tone: "test" },
            { label: "Node 20", tone: "test" },
            { label: "Node 22", tone: "test" },
          ],
        },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        A typical npm package needs to prove it works on every Node version it
        claims to support. The slow part is dependency install, so you want to
        pay for it <strong>once</strong> and reuse it across the test fan-out.
      </p>
      <p>
        The <code>install</code> job runs <code>npm ci</code> and declares a{" "}
        <code>cache</code> keyed on <code>package-lock.json</code> — Forge hashes
        the lockfile content, so an unchanged lockfile is a clean hit and a bumped
        one is a clean miss. The <code>test</code> job expands via{" "}
        <code>parallel: matrix</code> into three instances (Node 18/20/22), each
        pulling the same cache with <code>policy: pull</code> so no test instance
        fights to re-save it.
      </p>
      <p>
        <code>needs: [install]</code> wires the whole matrix behind the single
        install job, and JUnit output is surfaced per instance through{" "}
        <code>artifacts.reports.junit</code>.
      </p>
    </>
  ),
  yaml: `stages: [install, test]

default:
  image: node:20

jobs:
  install:
    stage: install
    script:
      - npm ci
    cache:
      key:
        files: [package-lock.json]
        prefix: npm
      paths: [node_modules/]
      policy: pull-push
    artifacts:
      paths: [node_modules/]
      expire_in: 1h

  test:
    stage: test
    needs: [install]
    image: node:\${NODE_VERSION}
    parallel:
      matrix:
        - NODE_VERSION: ["18", "20", "22"]
    cache:
      key:
        files: [package-lock.json]
        prefix: npm
      paths: [node_modules/]
      policy: pull
    script:
      - node --version
      - npm test -- --ci
    artifacts:
      reports:
        junit: ["reports/junit-*.xml"]
      expire_in: 1w
`,
};
