import { PipelineFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const securityFirst: Recipe = {
  slug: "security-first",
  title: "Security-first pipeline in three lines",
  category: "Security & compliance",
  summary:
    "One-line includes wire SAST, dependency, and secret scanning ahead of your build — zero template setup.",
  features: ["include: security/sast", "include: security/dependency", "include: security/secrets", "allow_failure"],
  diagram: () => (
    <PipelineFlow
      caption="Scans first, then build"
      columns={[
        {
          title: "test (scans)",
          nodes: [
            { label: "SAST", sub: "semgrep", tone: "scan" },
            { label: "deps", sub: "trivy", tone: "scan" },
            { label: "secrets", sub: "gitleaks", tone: "scan" },
          ],
        },
        { title: "build", nodes: [{ label: "build", tone: "build" }] },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        You want baseline security scanning on every pipeline without writing or
        maintaining scanner jobs. Forge ships built-in templates that resolve
        through the normal <code>include:</code> mechanism — no repo registration
        needed.
      </p>
      <p>
        Three <code>include: template:</code> lines add{" "}
        <code>security/sast</code> (semgrep), <code>security/dependency</code>{" "}
        (trivy filesystem), and <code>security/secrets</code> (gitleaks). Each
        lands in the <code>test</code> stage. Because included stages are unioned
        ahead of your main-only stages, declaring <code>[test, build]</code> makes
        the scans run <strong>security-first</strong>, before the build.
      </p>
      <p>
        Every built-in scan defaults to <code>allow_failure: true</code> — a
        finding is surfaced (the job reports failed) but does not block the
        pipeline, so you can adopt scanning without breaking delivery on day one.
        Flip any scan to <code>allow_failure: false</code> via an inline override
        once you are ready to enforce it.
      </p>
    </>
  ),
  yaml: `include:
  - template: security/sast
  - template: security/dependency
  - template: security/secrets

stages: [test, build]

jobs:
  build:
    stage: build
    image: golang:1.23
    script:
      - go build ./...
`,
};
