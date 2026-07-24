import { LinearFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const goService: Recipe = {
  slug: "go-service",
  title: "Go service — vet, build, test with JUnit",
  category: "Languages & build",
  summary:
    "Build and vet, then race-test with a module + build cache keyed on go.sum, publishing a JUnit report.",
  features: ["cache (files)", "needs", "reports.junit", "default.variables"],
  diagram: () => (
    <LinearFlow
      caption="Go build then test"
      steps={[
        { label: "go vet", sub: "+ build", tone: "build" },
        { label: "go test", sub: "-race → JUnit", tone: "test" },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        A Go service wants fast feedback: static checks first, then the race
        detector on the full suite. Re-downloading modules and recompiling the
        standard library on every run is the tax you want to avoid.
      </p>
      <p>
        Both jobs point <code>GOMODCACHE</code> and <code>GOCACHE</code> at a
        workspace-relative <code>.gocache</code> directory and cache it, keyed on{" "}
        <code>go.sum</code>. The <code>build</code> job uses{" "}
        <code>policy: pull-push</code> to warm the cache; <code>test</code> uses{" "}
        <code>policy: pull</code>. Keeping the cache inside the workspace matters —
        absolute paths like <code>/go/pkg/mod</code> are skipped by the cache
        archiver.
      </p>
      <p>
        The test job pipes <code>go test</code> through{" "}
        <code>go-junit-report</code> and exposes the result via{" "}
        <code>artifacts.reports.junit</code>, so Forge parses pass/fail counts
        into the job's report tab.
      </p>
    </>
  ),
  yaml: `stages: [build, test]

default:
  image: golang:1.23
  variables:
    CGO_ENABLED: "0"

jobs:
  build:
    stage: build
    script:
      - export GOMODCACHE="$PWD/.gocache/mod" GOCACHE="$PWD/.gocache/build"
      - go mod download
      - go vet ./...
      - go build ./...
    cache:
      key:
        files: [go.sum]
        prefix: gomod
      paths: [.gocache]
      policy: pull-push

  test:
    stage: test
    needs: [build]
    script:
      - export GOMODCACHE="$PWD/.gocache/mod" GOCACHE="$PWD/.gocache/build"
      - go install github.com/jstemmer/go-junit-report/v2@latest
      - go test -v -race ./... 2>&1 | go-junit-report -set-exit-code > report.xml
    cache:
      key:
        files: [go.sum]
        prefix: gomod
      paths: [.gocache]
      policy: pull
    artifacts:
      reports:
        junit: [report.xml]
      expire_in: 1w
`,
};
