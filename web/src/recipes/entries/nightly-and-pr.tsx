import { PipelineFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const nightlyAndPr: Recipe = {
  slug: "nightly-and-pr",
  title: "Nightly schedule + per-PR checks",
  category: "Scale & orchestration",
  summary:
    "Route jobs by trigger source: unit tests always, lint on merge requests, a heavy e2e suite only on the nightly schedule.",
  features: ["rules (CI_PIPELINE_SOURCE)", "schedules", "merge_request", "reports.junit"],
  diagram: () => (
    <PipelineFlow
      caption="Same config, different triggers"
      columns={[
        {
          title: "every run",
          nodes: [{ label: "unit", tone: "test" }],
        },
        {
          title: "by source",
          nodes: [
            { label: "lint", sub: "merge_request", tone: "build" },
            { label: "e2e", sub: "schedule", tone: "scan" },
          ],
        },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        One registered config serves three cadences. Forge compiles it
        source-aware, so the same YAML produces different DAGs depending on what
        triggered the pipeline (<code>CI_PIPELINE_SOURCE</code>).
      </p>
      <p>
        <code>unit</code> has no rules, so it runs on every pipeline. <code>lint</code>{" "}
        uses <code>rules: if $CI_PIPELINE_SOURCE == "merge_request"</code> — it
        only appears on PR/MR-triggered pipelines. <code>nightly-e2e</code> is
        gated to <code>$CI_PIPELINE_SOURCE == "schedule"</code>, so the expensive
        end-to-end suite runs only on the cron-driven nightly run and never on
        ordinary pushes.
      </p>
      <p>
        The nightly cadence itself is configured in the <strong>Schedules</strong>{" "}
        UI (a cron expression on a ref), which fires pipelines with source{" "}
        <code>schedule</code>. No separate config file is needed — the schedule
        and the <code>rules</code> together decide what runs when.
      </p>
    </>
  ),
  yaml: `stages: [test, nightly]

default:
  image: node:20

jobs:
  unit:
    stage: test
    script:
      - npm ci
      - npm test

  lint:
    stage: test
    rules:
      - if: '$CI_PIPELINE_SOURCE == "merge_request"'
        when: on_success
      - when: never
    script:
      - npm ci
      - npm run lint

  nightly-e2e:
    stage: nightly
    rules:
      - if: '$CI_PIPELINE_SOURCE == "schedule"'
        when: on_success
      - when: never
    script:
      - npm ci
      - npm run test:e2e
    artifacts:
      reports:
        junit: ["reports/junit-*.xml"]
      expire_in: 3d
`,
};
