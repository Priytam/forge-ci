/**
 * The resource tree's layout: left to right, App → top-level resources →
 * what they own (Deployment → ReplicaSet → Pods). Each leaf takes a row and a
 * parent sits level with the middle of its children. Large fan-outs collapse:
 * past GROUP_AT siblings of one kind, the first GROUP_SHOW stay and the rest
 * become one "+N pods" node until expanded.
 *
 * Pure and O(n log n): it runs in a Web Worker for big trees, and only when
 * the tree's shape changes — a health or status update moves nothing.
 */

export interface LayoutResource {
  uid: string;
  kind: string;
  name: string;
  parents?: string[];
}

export interface LayoutInput {
  resources: LayoutResource[];
  /** group ids the user expanded */
  expanded: string[];
}

export interface LayoutNode {
  /** a resource uid, APP_ID, or a group id */
  id: string;
  x: number;
  y: number;
  /** a collapsed group: the hidden siblings */
  group?: { kind: string; uids: string[] };
}

export interface LayoutEdge {
  from: string;
  to: string;
  d: string;
}

export interface Layout {
  nodes: LayoutNode[];
  edges: LayoutEdge[];
  width: number;
  height: number;
}

export const APP_ID = "app";
export const NODE_W = 248;
export const NODE_H = 54;
export const COL_GAP = 64;
export const ROW_GAP = 12;
export const GROUP_AT = 8;
export const GROUP_SHOW = 4;

/** Kinds in the order a reader expects them, top to bottom. */
const KIND_RANK: Record<string, number> = {
  Deployment: 0, StatefulSet: 1, DaemonSet: 2, Job: 3, Service: 4, Revision: 5, Ingress: 6,
  HorizontalPodAutoscaler: 7, ConfigMap: 8, PersistentVolumeClaim: 9, ReplicaSet: 10, Pod: 11,
};

function order(a: LayoutResource, b: LayoutResource): number {
  return (KIND_RANK[a.kind] ?? 50) - (KIND_RANK[b.kind] ?? 50) || a.kind.localeCompare(b.kind) || a.name.localeCompare(b.name);
}

/** Identifies the tree's shape: a new layout is needed only when it changes. */
export function shapeKey(input: LayoutInput): string {
  const parts = input.resources.map((r) => r.uid + ">" + (r.parents ?? []).join(","));
  parts.sort();
  return parts.join("|") + "#" + [...input.expanded].sort().join(",");
}

export function layoutTree(input: LayoutInput): Layout {
  const byUid = new Map(input.resources.map((r) => [r.uid, r]));
  const children = new Map<string, LayoutResource[]>();
  const add = (parent: string, r: LayoutResource) => {
    const list = children.get(parent);
    if (list) list.push(r);
    else children.set(parent, [r]);
  };
  for (const r of input.resources) {
    const parent = (r.parents ?? []).find((p) => byUid.has(p));
    add(parent ?? APP_ID, r);
  }
  const expanded = new Set(input.expanded);
  const nodes: LayoutNode[] = [];
  const edges: LayoutEdge[] = [];
  const rowH = NODE_H + ROW_GAP;
  let row = 0;
  let maxDepth = 0;

  // Places id and its subtree; returns id's y. Iterative over siblings,
  // recursive over depth (trees are shallow: App → workload → rs → pod).
  const place = (id: string, depth: number, group?: LayoutNode["group"]): number => {
    maxDepth = Math.max(maxDepth, depth);
    const x = depth * (NODE_W + COL_GAP);
    const kids = group ? [] : visibleChildren(id);
    let y: number;
    if (kids.length === 0) {
      y = row * rowH;
      row++;
    } else {
      const ys = kids.map((k) => place(k.id, depth + 1, k.group));
      y = (ys[0] + ys[ys.length - 1]) / 2;
      const x1 = x + NODE_W;
      const x2 = x1 + COL_GAP;
      kids.forEach((k, i) => {
        const y1 = y + NODE_H / 2;
        const y2 = ys[i] + NODE_H / 2;
        const c = COL_GAP / 2;
        edges.push({ from: id, to: k.id, d: `M ${x1} ${y1} C ${x1 + c} ${y1}, ${x2 - c} ${y2}, ${x2} ${y2}` });
      });
    }
    nodes.push({ id, x, y, group });
    return y;
  };

  // A node's children in display order, large same-kind runs collapsed.
  const visibleChildren = (id: string): { id: string; group?: LayoutNode["group"] }[] => {
    const kids = (children.get(id) ?? []).slice().sort(order);
    const out: { id: string; group?: LayoutNode["group"] }[] = [];
    for (let i = 0; i < kids.length; ) {
      let j = i;
      while (j < kids.length && kids[j].kind === kids[i].kind) j++;
      const run = kids.slice(i, j);
      const gid = `${id}/${run[0].kind}+`;
      if (run.length > GROUP_AT && !expanded.has(gid)) {
        run.slice(0, GROUP_SHOW).forEach((r) => out.push({ id: r.uid }));
        out.push({ id: gid, group: { kind: run[0].kind, uids: run.slice(GROUP_SHOW).map((r) => r.uid) } });
      } else {
        run.forEach((r) => out.push({ id: r.uid }));
      }
      i = j;
    }
    return out;
  };

  place(APP_ID, 0);
  return {
    nodes,
    edges,
    width: (maxDepth + 1) * (NODE_W + COL_GAP) - COL_GAP,
    height: Math.max(1, row) * rowH - ROW_GAP,
  };
}
