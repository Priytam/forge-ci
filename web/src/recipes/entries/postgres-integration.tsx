import { PipelineFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const postgresIntegration: Recipe = {
  slug: "postgres-integration",
  title: "Integration tests with a Postgres service",
  category: "Testing",
  summary:
    "Run DB-backed tests against a throwaway Postgres sidecar reachable by alias, with a JUnit report.",
  features: ["services", "service alias + env", "variables", "reports.junit"],
  diagram: () => (
    <PipelineFlow
      caption="Job + Postgres sidecar"
      columns={[
        {
          nodes: [
            { label: "integration", sub: "go test", tone: "test" },
            { label: "postgres:16", sub: "alias: db", tone: "neutral" },
          ],
        },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        Integration tests need a real database, but standing one up out-of-band is
        fragile. Forge's <code>services:</code> model starts a container alongside
        the job, on a shared network, and tears it down on every exit path.
      </p>
      <p>
        The job declares a <code>postgres:16-alpine</code> service with{" "}
        <code>alias: db</code> and its <code>env</code> (user, password, database).
        The test process reaches it by that alias —{" "}
        <code>postgres://postgres:secret@db:5432/...</code> — set once via a job{" "}
        <code>variables</code> entry. This works identically on docker and
        kubernetes runners; the <strong>shell</strong> executor cannot run
        services, so route this to a container runner.
      </p>
      <p>
        Container-running does not mean the DB is accepting connections, so the
        script polls with <code>pg_isready</code> before testing. Results are
        piped through <code>go-junit-report</code> and exposed via{" "}
        <code>reports.junit</code>.
      </p>
    </>
  ),
  yaml: `stages: [test]

jobs:
  integration:
    stage: test
    image: golang:1.23
    services:
      - image: postgres:16-alpine
        alias: db
        env:
          POSTGRES_USER: postgres
          POSTGRES_PASSWORD: secret
          POSTGRES_DB: app_test
    variables:
      DATABASE_URL: postgres://postgres:secret@db:5432/app_test?sslmode=disable
    script:
      - until pg_isready -h db -U postgres; do echo "waiting for db"; sleep 1; done
      - go test -v -tags=integration ./... 2>&1 | go-junit-report -set-exit-code > report.xml
    artifacts:
      reports:
        junit: [report.xml]
      expire_in: 1w
`,
};
