import { LinearFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const javaGradle: Recipe = {
  slug: "java-gradle",
  title: "Java / Gradle — assemble, test, report",
  category: "Languages & build",
  summary:
    "Assemble the jar, then run the test task with a Gradle cache keyed on the build scripts and JUnit XML reports.",
  features: ["cache (files)", "artifacts.paths", "reports.junit", "needs"],
  diagram: () => (
    <LinearFlow
      caption="Gradle build then test"
      steps={[
        { label: "assemble", sub: "→ build/libs", tone: "build" },
        { label: "test", sub: "→ JUnit XML", tone: "test" },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        Gradle's incremental build and dependency resolution live under{" "}
        <code>GRADLE_USER_HOME</code>. Point it at a workspace-relative{" "}
        <code>.gradle</code> directory so it can be cached between pipelines.
      </p>
      <p>
        The cache key hashes the build definition —{" "}
        <code>settings.gradle.kts</code>, <code>build.gradle.kts</code>, and the
        wrapper properties — so a dependency bump invalidates the cache cleanly.
        The <code>build</code> job publishes the assembled jar as an artifact; the{" "}
        <code>test</code> job pulls the same cache and exports Gradle's{" "}
        <code>TEST-*.xml</code> files through <code>reports.junit</code>.
      </p>
      <p>
        <code>--no-daemon</code> keeps each job hermetic — no lingering daemon
        between runner invocations.
      </p>
    </>
  ),
  yaml: `stages: [build, test]

default:
  image: gradle:8.7-jdk21
  variables:
    GRADLE_USER_HOME: .gradle

jobs:
  build:
    stage: build
    script:
      - gradle --no-daemon assemble
    cache:
      key:
        files: [settings.gradle.kts, build.gradle.kts, gradle/wrapper/gradle-wrapper.properties]
        prefix: gradle
      paths: [.gradle/caches, .gradle/wrapper]
      policy: pull-push
    artifacts:
      paths: [build/libs/]
      expire_in: 1w

  test:
    stage: test
    needs: [build]
    script:
      - gradle --no-daemon test
    cache:
      key:
        files: [settings.gradle.kts, build.gradle.kts, gradle/wrapper/gradle-wrapper.properties]
        prefix: gradle
      paths: [.gradle/caches, .gradle/wrapper]
      policy: pull
    artifacts:
      reports:
        junit: ["build/test-results/test/TEST-*.xml"]
      expire_in: 1w
`,
};
