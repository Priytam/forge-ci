import { describe, expect, it } from "vitest";
import type { LiveResource } from "../../api";
import { networkTree } from "./views";

const r = (uid: string, kind: string, info: Record<string, string> = {}, parents?: string[]): LiveResource => ({
  uid, kind, name: uid, namespace: "checkout", parents, created_at: "", summary: { info: Object.entries(info).map(([name, value]) => ({ name, value })) },
});

describe("networkTree", () => {
  it("chains Ingress → Service → workload → pods by backend and selector", () => {
    const rs = [
      r("ing", "Ingress", { Backends: "svc" }),
      r("svc", "Service", { Selector: "app=cart" }),
      r("other", "Service", { Selector: "app=orders" }),
      r("dep", "Deployment", { "Pod labels": "app=cart,team=checkout" }),
      r("rs", "ReplicaSet", {}, ["dep"]),
      r("pod", "Pod", {}, ["rs"]),
      r("cm", "ConfigMap"),
    ];
    const t = new Map(networkTree(rs).map((x) => [x.uid, x.parents ?? []]));
    expect(Object.fromEntries(t)).toEqual({ ing: [], svc: ["ing"], other: [], dep: ["svc"], pod: ["dep"] });
  });
});
