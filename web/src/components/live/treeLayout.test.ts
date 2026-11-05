import { describe, expect, it } from "vitest";
import { APP_ID, GROUP_SHOW, layoutTree, NODE_W, COL_GAP, shapeKey, type LayoutResource } from "./treeLayout";

function app(pods: number): LayoutResource[] {
  const out: LayoutResource[] = [
    { uid: "dep", kind: "Deployment", name: "cart-api" },
    { uid: "rs", kind: "ReplicaSet", name: "cart-api-7d9f", parents: ["dep"] },
    { uid: "svc", kind: "Service", name: "cart-api" },
    { uid: "cm", kind: "ConfigMap", name: "cart-api-config" },
  ];
  for (let i = 0; i < pods; i++) out.push({ uid: `pod-${i}`, kind: "Pod", name: `cart-api-7d9f-${String(i).padStart(5, "0")}`, parents: ["rs"] });
  return out;
}

describe("layoutTree", () => {
  it("lays the tree out left to right, parents level with their children", () => {
    const l = layoutTree({ resources: app(3), expanded: [] });
    const at = new Map(l.nodes.map((n) => [n.id, n]));
    const col = (id: string) => at.get(id)!.x / (NODE_W + COL_GAP);
    expect([col(APP_ID), col("dep"), col("rs"), col("pod-0"), col("svc")]).toEqual([0, 1, 2, 3, 1]);
    // the ReplicaSet sits between its first and last pod
    expect(at.get("rs")!.y).toBe((at.get("pod-0")!.y + at.get("pod-2")!.y) / 2);
    // Deployment first, then Service, then ConfigMap — top to bottom
    expect(at.get("dep")!.y).toBeLessThan(at.get("svc")!.y);
    expect(at.get("svc")!.y).toBeLessThan(at.get("cm")!.y);
    expect(l.edges.map((e) => `${e.from}>${e.to}`)).toContain("rs>pod-1");
    // no two nodes overlap
    const seen = new Set(l.nodes.map((n) => `${n.x},${n.y}`));
    expect(seen.size).toBe(l.nodes.length);
  });

  it("collapses a large fan-out into one group node until expanded", () => {
    const res = app(40);
    const l = layoutTree({ resources: res, expanded: [] });
    const group = l.nodes.find((n) => n.group);
    expect(group?.id).toBe("rs/Pod+");
    expect(group?.group?.uids).toHaveLength(40 - GROUP_SHOW);
    expect(l.nodes.filter((n) => n.id.startsWith("pod-"))).toHaveLength(GROUP_SHOW);
    const open = layoutTree({ resources: res, expanded: ["rs/Pod+"] });
    expect(open.nodes.filter((n) => n.id.startsWith("pod-"))).toHaveLength(40);
  });

  it("treats resources whose parent is not in the app as top level", () => {
    const l = layoutTree({ resources: [{ uid: "p", kind: "Pod", name: "orphan", parents: ["gone"] }], expanded: [] });
    expect(l.edges).toEqual([expect.objectContaining({ from: APP_ID, to: "p" })]);
  });

  it("keeps its shape key when only status changes", () => {
    const a = app(5);
    const b = a.map((r) => ({ ...r, name: r.name + "-renamed-status-only" }));
    expect(shapeKey({ resources: a, expanded: [] })).toBe(shapeKey({ resources: b, expanded: [] }));
    expect(shapeKey({ resources: a, expanded: [] })).not.toBe(shapeKey({ resources: app(6), expanded: [] }));
  });

  // Budget: p95 first paint < 300 ms for a 2,000-resource app. Layout is the
  // part that grows with the tree; it must be a small slice of that.
  it("lays out 2,000 resources fast, collapsed or fully expanded", () => {
    const res = app(1996);
    const time = (expanded: string[]) => {
      const runs: number[] = [];
      for (let i = 0; i < 7; i++) {
        const t0 = performance.now();
        layoutTree({ resources: res, expanded });
        runs.push(performance.now() - t0);
      }
      return runs.sort((a, b) => a - b)[5]; // ~p85 of 7 runs
    };
    const collapsed = time([]);
    const expanded = time(["rs/Pod+"]);
    console.log(`layout of 2,000 resources: collapsed ${collapsed.toFixed(1)} ms, expanded ${expanded.toFixed(1)} ms`);
    expect(collapsed).toBeLessThan(50);
    expect(expanded).toBeLessThan(80);
  });
});
