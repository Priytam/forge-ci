import { describe, expect, it } from "vitest";
import type { LiveResource } from "../../api";
import { canaries, canaryWeight } from "./Rollout";

const dep = (name: string, ready: string, extra: Partial<LiveResource> = {}): LiveResource => ({
  kind: "Deployment", namespace: "shop", name, uid: name, summary: { ready }, created_at: "2026-10-27T00:00:00Z", ...extra,
});

describe("canary rollouts", () => {
  const rs = [dep("web", "4/4"), dep("web-canary", "1/1", { track: "canary" }), dep("worker", "1/1")];
  it("pairs each canary with its stable Deployment", () => {
    const [r] = canaries(rs);
    expect(r.canary.name).toBe("web-canary");
    expect(r.stable?.name).toBe("web");
    expect(canaries([dep("web", "2/2")])).toEqual([]);
  });
  it("weighs the canary by its share of ready pods", () => {
    expect(canaryWeight(rs[1], rs[0])).toBe(20);
    expect(canaryWeight(dep("x-canary", "0/1"), dep("x", "2/2"))).toBe(0);
  });
});
