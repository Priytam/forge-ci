import { describe, expect, it } from "vitest";
import { actionImpact, buildViews, capacity, fleetUtilisation, runnerState, stateCounts } from "./model";
import { FLEET, loadFor, NOW, runner } from "./fixtures";

describe("runnerState", () => {
  it("gives paused its own state, never online (bug 2)", () => {
    expect(runnerState({ online: true, paused: true })).toBe("paused");
    expect(runnerState({ online: true, paused: true, running_jobs: 1 })).toBe("paused");
  });
  it("orders offline > draining > paused > busy > online", () => {
    expect(runnerState({ online: false, paused: true, draining: true })).toBe("offline");
    expect(runnerState({ online: true, paused: true, draining: true })).toBe("draining");
    expect(runnerState({ online: true, paused: false, running_jobs: 2 })).toBe("busy");
    expect(runnerState({ online: true, paused: false })).toBe("online");
  });
});

describe("capacity", () => {
  const views = buildViews(FLEET, loadFor(FLEET));
  const cap = capacity(views);
  it("counts slots of non-stale runners, minus paused and draining", () => {
    // builder-1, -2, -3 (1 each) + k8s-1 (4) + gce-builder-1 (1) + gpu-1 (1); gce-runner-dev is stale.
    expect(cap.total).toBe(9);
    // available: builder-1 (1) + k8s-1 (4) + gce-builder-1 (1)
    expect(cap.available).toBe(6);
    expect(cap.paused).toBe(1);
    expect(cap.draining).toBe(1);
    expect(cap.offline).toBe(1);
    expect(cap.busy).toBe(3);
  });
  it("leaves stale runners out of tag totals (bug 6)", () => {
    const gcp = cap.byTag.find((t) => t.tag === "gcp")!;
    expect(gcp).toMatchObject({ runners: 1, total: 1, online: 1, available: 1 });
  });
  it("flags tags with no available slots", () => {
    expect(cap.starved).toEqual(["gpu"]);
    expect(cap.byTag.find((t) => t.tag === "docker")).toMatchObject({ total: 7, available: 5 });
  });
  it("counts states, stale as offline", () => {
    expect(stateCounts(views)).toEqual({ online: 1, busy: 2, paused: 1, draining: 1, offline: 2 });
  });
});

describe("utilisation", () => {
  it("is busy time over the slot time the runner was around, from the range's load", () => {
    const r = runner({ id: "a", concurrency: 2, created_at: new Date(NOW - 12 * 3600_000).toISOString() });
    const [v] = buildViews([r], loadFor([r]));
    // 43200 busy seconds over 12h × 2 slots
    expect(v.utilisation).toBeCloseTo(0.5);
    expect(v.jobs).toBe(12);
  });
  it("follows the load's range, not a fixed 24h (bug 4)", () => {
    const r = runner({ id: "a" });
    const week = loadFor([r], NOW - 7 * 86400_000, NOW);
    expect(buildViews([r], week)[0].utilisation).toBeCloseTo(43200 / (7 * 86400));
  });
  it("fleet utilisation weights by slots", () => {
    const rs = [runner({ id: "a", concurrency: 3 }), runner({ id: "b" })];
    expect(fleetUtilisation(rs, 4 * 3600, NOW - 4 * 3600_000, NOW)).toBeCloseTo(0.25);
    expect(fleetUtilisation([], 10, NOW - 1000, NOW)).toBeNull();
  });
});

describe("actionImpact", () => {
  const views = buildViews(FLEET, loadFor(FLEET));
  it("says how many slots of each tag a pause removes", () => {
    const k8s = views.find((v) => v.id === "k8s-1")!;
    const imp = actionImpact(k8s, views, "pause");
    expect(imp.text).toContain("removes 4 of 5 docker slots and 4 of 4 k8s slots");
    expect(imp.text).toContain("Nothing will run “k8s” jobs");
    expect(imp.danger).toBe(true);
  });
  it("removal keeps history", () => {
    const stale = views.find((v) => v.id === "gce-runner-dev")!;
    expect(actionImpact(stale, views, "remove").text).toMatch(/history stay/);
  });
});
