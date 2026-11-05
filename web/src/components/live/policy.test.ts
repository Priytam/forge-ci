import { describe, expect, it } from "vitest";
import type { LiveResource } from "../../api";
import { desiredResources, prunable } from "./AppPanels";
import { requester } from "./ui";

const r = (name: string, extra: Partial<LiveResource> = {}): LiveResource => ({
  kind: "ConfigMap", name, uid: name, summary: {}, created_at: "2026-10-27T00:00:00Z", ...extra,
});

describe("sync scope", () => {
  const rs = [
    r("in-git", { sync: "Synced" }),
    r("drifted", { sync: "OutOfSync" }),
    r("missing", { uid: "desired:/ConfigMap/shop/missing", sync: "OutOfSync" }),
    r("dropped", { sync: "OutOfSync", requires_pruning: true }),
    r("someone-elses"), // not in Git, not applied by Forge: never pruned
    r("hook", { hook: "PreSync" }),
  ];
  it("syncs what Git has", () => {
    expect(desiredResources(rs).map((x) => x.name)).toEqual(["in-git", "drifted", "missing"]);
  });
  it("prunes only what Forge applied and Git dropped", () => {
    expect(prunable(rs).map((x) => x.name)).toEqual(["dropped"]);
  });
});

describe("requester", () => {
  it("names Forge's own operations by why it started them", () => {
    expect(requester({ params: { kind: "sync", trigger: "self-heal" }, requested_by: "Forge auto-sync" })).toBe("Self-heal");
    expect(requester({ params: { kind: "sync" }, requested_by: "ana@lab.local" })).toBe("ana@lab.local");
  });
});
