import type { LiveResource } from "../../api";
import { HealthIcon } from "./ui";

/** A fact from a resource's summary ("Node", "Selector"…). */
export function fact(r: LiveResource, name: string): string {
  return r.summary.info?.find((kv) => kv.name === name)?.value ?? "";
}

function labels(s: string): Map<string, string> {
  return new Map(s.split(",").filter(Boolean).map((p) => p.split("=") as [string, string]));
}

/**
 * The Network view as a tree: Ingress → Service → workload → its pods.
 * Services are matched to workloads by selector against pod labels, and
 * Ingresses to Services by backend name — all from what the agent reports,
 * so the ResourceTree lays it out like any other tree.
 */
export function networkTree(rs: LiveResource[]): LiveResource[] {
  const byName = (kind: string) => new Map(rs.filter((r) => r.kind === kind).map((r) => [`${r.namespace}/${r.name}`, r]));
  const services = byName("Service");
  const workloads = rs.filter((r) => ["Deployment", "StatefulSet", "DaemonSet"].includes(r.kind));
  const out = new Map<string, LiveResource>();
  const put = (r: LiveResource, parents: string[]) => out.set(r.uid, { ...r, parents });
  for (const ing of rs.filter((r) => r.kind === "Ingress")) {
    put(ing, []);
    for (const name of fact(ing, "Backends").split(",").filter(Boolean)) {
      const svc = services.get(`${ing.namespace}/${name}`);
      if (svc) put(svc, [ing.uid]);
    }
  }
  for (const svc of services.values()) {
    if (!out.has(svc.uid)) put(svc, []);
    const sel = labels(fact(svc, "Selector"));
    if (sel.size === 0) continue;
    for (const w of workloads) {
      const pl = labels(fact(w, "Pod labels"));
      if (w.namespace === svc.namespace && [...sel].every(([k, v]) => pl.get(k) === v)) put(w, [svc.uid]);
    }
  }
  // A workload's pods hang off it directly (its ReplicaSet is not a hop).
  const ownerOf = new Map(rs.map((r) => [r.uid, (r.parents ?? [])[0]]));
  const workloadOf = (uid: string): string | undefined => {
    for (let u: string | undefined = uid, i = 0; u && i < 4; u = ownerOf.get(u), i++) if (out.has(u) && u !== uid) return u;
    return undefined;
  };
  for (const pod of rs.filter((r) => r.kind === "Pod")) {
    const w = workloadOf(pod.uid);
    if (w && ["Deployment", "StatefulSet", "DaemonSet"].includes(out.get(w)!.kind)) put(pod, [w]);
  }
  return [...out.values()];
}

/** The Pods view: pods grouped by the node they run on. */
export function PodsView({ resources, selected, onSelect }: {
  resources: LiveResource[];
  selected: string | null;
  onSelect: (uid: string) => void;
}) {
  const pods = resources.filter((r) => r.kind === "Pod");
  const nodes = new Map<string, LiveResource[]>();
  for (const p of pods) {
    const n = fact(p, "Node") || "not scheduled";
    nodes.set(n, [...(nodes.get(n) ?? []), p]);
  }
  if (pods.length === 0) return <div className="empty card">This application has no pods.</div>;
  return (
    <div className="lv-pods">
      {[...nodes.entries()].sort().map(([node, ps]) => (
        <section key={node} className="card lv-pods-node">
          <h3>{node} <span className="muted">{ps.length} pod{ps.length === 1 ? "" : "s"}</span></h3>
          <div className="lv-pods-grid">
            {ps.sort((a, b) => a.name.localeCompare(b.name)).map((p) => (
              <button key={p.uid} type="button" className={`lv-pod${selected === p.uid ? " lv-pod-on" : ""}`} onClick={() => onSelect(p.uid)}
                title={p.health?.message || p.name}>
                {p.health && <HealthIcon health={p.health.status} size={12} />}
                <span className="lv-pod-name">{p.name}</span>
                <span className="muted">{p.summary.status !== "Running" ? p.summary.status : p.summary.ready}{(p.summary.restarts ?? 0) > 0 && ` ↻${p.summary.restarts}`}</span>
              </button>
            ))}
          </div>
        </section>
      ))}
    </div>
  );
}
