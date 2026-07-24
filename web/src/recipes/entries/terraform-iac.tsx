import { PipelineFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const terraformIac: Recipe = {
  slug: "terraform-iac",
  title: "Terraform — fmt, validate, plan, manual apply",
  category: "Infrastructure as code",
  summary:
    "Format and validate, save the plan as an artifact, then apply behind a manual gate that only exists on main.",
  features: ["rules (when: manual)", "artifacts", "needs", "environment", "default.image"],
  diagram: () => (
    <PipelineFlow
      caption="Validate → plan → manual apply"
      columns={[
        {
          title: "validate",
          nodes: [
            { label: "fmt", tone: "test" },
            { label: "validate", tone: "test" },
          ],
        },
        { title: "plan", nodes: [{ label: "plan", sub: "→ tfplan", tone: "build" }] },
        { title: "apply", nodes: [{ label: "apply", sub: "manual · main", tone: "gate" }] },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        Infrastructure changes deserve a review of the exact plan before anything
        mutates. This pipeline turns <code>terraform plan</code> into a reviewable
        artifact and puts <code>apply</code> behind an explicit human action.
      </p>
      <p>
        <code>fmt</code> and <code>validate</code> run in parallel in the first
        stage. <code>plan</code> writes a binary <code>tfplan</code> and archives
        it with <code>artifacts</code>, so <code>apply</code> consumes the very
        plan that was reviewed — no drift between plan and apply.
      </p>
      <p>
        The <code>apply</code> job's <code>rules</code> use{" "}
        <code>when: manual</code> gated on <code>main</code>: it is created in a{" "}
        <strong>blocked</strong> state and only runs after someone plays it
        (`POST /jobs/&#123;id&#125;/play`). Combined with a protected{" "}
        <code>environment</code>, you get play-then-approve semantics for
        production infrastructure.
      </p>
    </>
  ),
  yaml: `stages: [validate, plan, apply]

default:
  image: hashicorp/terraform:1.9

jobs:
  fmt:
    stage: validate
    script:
      - terraform fmt -check -recursive

  validate:
    stage: validate
    script:
      - terraform init -backend=false
      - terraform validate

  plan:
    stage: plan
    needs: [fmt, validate]
    script:
      - terraform init
      - terraform plan -out=tfplan
    artifacts:
      paths: [tfplan]
      expire_in: 1d

  apply:
    stage: apply
    needs: [plan]
    environment: production
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        when: manual
      - when: never
    script:
      - terraform init
      - terraform apply -auto-approve tfplan
`,
};
