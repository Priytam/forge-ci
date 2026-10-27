import { describe, expect, it } from "vitest";
import { compact, lineDiff, project, stats } from "./diff";
import { toYaml } from "./yaml";

describe("project", () => {
  it("keeps only what Git sets", () => {
    const live = {
      kind: "Deployment",
      metadata: { name: "cart", uid: "u1", labels: { app: "cart", extra: "x" } },
      spec: { replicas: 3, revisionHistoryLimit: 10, template: { spec: { containers: [{ name: "app", image: "cart:v1", ports: [{ containerPort: 8080, protocol: "TCP" }] }] } } },
      status: { replicas: 3 },
    };
    const desired = { kind: "Deployment", metadata: { name: "cart", labels: { app: "cart" } }, spec: { replicas: 2, template: { spec: { containers: [{ name: "app", image: "cart:v1" }] } } } };
    expect(project(live, desired)).toEqual({
      kind: "Deployment",
      metadata: { name: "cart", labels: { app: "cart" } },
      spec: { replicas: 3, template: { spec: { containers: [{ name: "app", image: "cart:v1" }] } } },
    });
  });
});

describe("lineDiff", () => {
  it("finds the changed line between live and desired YAML", () => {
    const lines = lineDiff(toYaml({ spec: { replicas: 3, paused: false } }), toYaml({ spec: { replicas: 2, paused: false } }));
    expect(lines.filter((l) => l.kind !== "same").map((l) => `${l.kind} ${l.text.trim()}`)).toEqual(["del replicas: 3", "add replicas: 2"]);
    expect(stats(lines)).toEqual({ added: 1, removed: 1 });
  });

  it("compacts long unchanged runs into gaps", () => {
    const a = Array.from({ length: 40 }, (_, i) => `line ${i}`).join("\n");
    const b = a.replace("line 20", "line twenty");
    const c = compact(lineDiff(a, b), 2);
    expect(c[0]).toBeNull();
    expect(c.filter(Boolean)).toHaveLength(6); // 2 + del + add + 2
    expect(c[c.length - 1]).not.toBeNull();
  });
});
