import { describe, expect, it } from "vitest";
import type { FleetCell } from "../api";
import { majorityImage } from "./Fleet";
import { matchingTargets } from "./LiveTemplates";

const cell = (img: string): FleetCell => ({ app_id: 1, health: "Healthy", sync: "Synced", revision: "", images: [img], pods: 1, pods_ready: 1 });

describe("fleet", () => {
  it("measures version skew against what most clusters run", () => {
    expect(majorityImage({ name: "a", group: "", template: "", cells: { 1: cell("x:v2"), 2: cell("x:v2"), 3: cell("x:v1") } })).toBe("x:v2");
  });
  it("matches templates to clusters by name and environment", () => {
    const ts = [{ name: "lab", environment: "fleet" }, { name: "east", environment: "staging" }, { name: "west", environment: "staging" }];
    expect(matchingTargets({ targets: [], environments: ["staging"] }, ts).map((t) => t.name)).toEqual(["east", "west"]);
    expect(matchingTargets({ targets: ["west"], environments: [] }, ts).map((t) => t.name)).toEqual(["west"]);
    expect(matchingTargets({ targets: [], environments: [] }, ts)).toHaveLength(3);
  });
});
