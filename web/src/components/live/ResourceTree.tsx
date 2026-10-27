import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { relativeTime, type Health, type LiveApp, type LiveResource } from "../../api";
import { HealthIcon, KindIcon, KIND_SHORT, SyncIcon } from "./ui";
import {
  APP_ID,
  layoutTree,
  NODE_H,
  NODE_W,
  shapeKey,
  type Layout,
  type LayoutInput,
} from "./treeLayout";

/** Trees above this many resources are laid out in a Web Worker. Below it
 *  the layout costs a few milliseconds (2,000 resources: ~1 ms), less than
 *  a round trip to a worker. */
const WORKER_AT = 5000;

/** Nodes this far outside the viewport are still drawn, so scrolling is smooth. */
const OVERSCAN = 300;

/** The layout for input, recomputed only when the tree's shape changes. */
function useLayout(input: LayoutInput): Layout | null {
  const key = useMemo(() => shapeKey(input), [input]);
  const big = input.resources.length > WORKER_AT && typeof Worker !== "undefined";
  const [async, setAsync] = useState<Layout | null>(null);
  const worker = useRef<Worker | null>(null);
  const seq = useRef(0);
  const sync = useMemo(() => (big ? null : layoutTree(input)), [key, big]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (!big) return;
    worker.current ??= new Worker(new URL("./treeLayout.worker.ts", import.meta.url), { type: "module" });
    const w = worker.current;
    const mine = ++seq.current;
    w.onmessage = (e: MessageEvent<{ seq: number; layout: Layout }>) => {
      if (e.data.seq === seq.current) setAsync(e.data.layout);
    };
    w.postMessage({ seq: mine, input });
  }, [key, big]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => worker.current?.terminate(), []);
  return sync ?? async;
}

export interface TreeFilter {
  q: string;
  health: Health[];
}

function matches(r: LiveResource, f: TreeFilter): boolean {
  const q = f.q.trim().toLowerCase();
  if (q && !r.name.toLowerCase().includes(q) && !r.kind.toLowerCase().includes(q)) return false;
  return f.health.length === 0 || (r.health ? f.health.includes(r.health.status) : false);
}

/**
 * The application's resources as a left-to-right graph. Only the nodes and
 * edges inside the viewport are in the DOM, so a 2,000-resource tree costs
 * what the screen shows. Nodes that do not match the filter are dimmed, not
 * removed — the shape stays put while you search.
 */
export default function ResourceTree({
  app,
  resources,
  selected,
  onSelect,
  filter,
}: {
  app: LiveApp;
  resources: LiveResource[];
  selected: string | null;
  onSelect: (uid: string | null) => void;
  filter: TreeFilter;
}) {
  const [expanded, setExpanded] = useState<string[]>([]);
  const input = useMemo(() => ({ resources, expanded }), [resources, expanded]);
  const layout = useLayout(input);
  const byUid = useMemo(() => new Map(resources.map((r) => [r.uid, r])), [resources]);

  const scroller = useRef<HTMLDivElement>(null);
  const [view, setView] = useState({ left: 0, top: 0, width: 1600, height: 900 });
  useLayoutEffect(() => {
    const el = scroller.current;
    if (!el) return;
    let frame = 0;
    const measure = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() =>
        setView({ left: el.scrollLeft, top: el.scrollTop, width: el.clientWidth, height: el.clientHeight })
      );
    };
    measure();
    el.addEventListener("scroll", measure, { passive: true });
    const ro = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    ro?.observe(el);
    return () => {
      cancelAnimationFrame(frame);
      el.removeEventListener("scroll", measure);
      ro?.disconnect();
    };
  }, []);

  if (!layout) return <div className="lv-tree-wrap lv-tree-loading muted">Laying out {resources.length} resources…</div>;

  const x0 = view.left - OVERSCAN;
  const x1 = view.left + view.width + OVERSCAN;
  const y0 = view.top - OVERSCAN;
  const y1 = view.top + view.height + OVERSCAN;
  const pos = new Map(layout.nodes.map((n) => [n.id, n]));
  const visible = layout.nodes.filter((n) => n.x + NODE_W >= x0 && n.x <= x1 && n.y + NODE_H >= y0 && n.y <= y1);
  const edges = layout.edges.filter((e) => {
    const a = pos.get(e.from)!;
    const b = pos.get(e.to)!;
    return Math.max(a.x, b.x) >= x0 && Math.min(a.x, b.x) <= x1 && Math.max(a.y, b.y) + NODE_H >= y0 && Math.min(a.y, b.y) <= y1;
  });
  const filtering = filter.q.trim() !== "" || filter.health.length > 0;

  return (
    <div className="lv-tree-wrap" ref={scroller} onClick={(e) => e.target === e.currentTarget && onSelect(null)}>
      <div className="lv-tree" style={{ width: layout.width + 24, height: layout.height + 24 }}>
        <svg className="lv-edges" width={layout.width + 24} height={layout.height + 24} aria-hidden="true">
          {edges.map((e) => (
            <path key={e.from + ">" + e.to} d={e.d} />
          ))}
        </svg>
        {visible.map((n) => {
          const style = { left: n.x, top: n.y, width: NODE_W, height: NODE_H };
          if (n.id === APP_ID) {
            return (
              <div key={n.id} className="lv-node lv-node-app" style={style}>
                <span className="lv-node-kind"><KindIcon kind="App" size={20} /></span>
                <span className="lv-node-main">
                  <span className="lv-node-name">{app.name}</span>
                  <span className="lv-node-meta">application · {app.namespace}</span>
                </span>
                <HealthIcon health={app.health} size={16} />
              </div>
            );
          }
          if (n.group) {
            const counts: Partial<Record<Health, number>> = {};
            for (const uid of n.group.uids) {
              const h = byUid.get(uid)?.health?.status;
              if (h) counts[h] = (counts[h] ?? 0) + 1;
            }
            return (
              <button key={n.id} type="button" className="lv-node lv-node-group" style={style}
                onClick={() => setExpanded((x) => [...x, n.id])} title="Show them all">
                <span className="lv-node-kind"><KindIcon kind={n.group.kind} size={18} /></span>
                <span className="lv-node-main">
                  <span className="lv-node-name">
                    +{n.group.uids.length} {n.group.kind === "Pod" ? "pods" : `${n.group.kind}s`}
                  </span>
                  <span className="lv-node-meta lv-group-counts">
                    {Object.entries(counts).map(([h, c]) => (
                      <span key={h}><HealthIcon health={h as Health} size={11} /> {c}</span>
                    ))}
                  </span>
                </span>
              </button>
            );
          }
          const r = byUid.get(n.id);
          if (!r) return null;
          const dim = filtering && !matches(r, filter);
          return (
            <button key={n.id} type="button" style={style}
              className={`lv-node${selected === r.uid ? " lv-node-on" : ""}${dim ? " lv-node-dim" : ""}${r.health ? ` lv-node-${r.health.status.toLowerCase()}` : ""}${r.sync === "OutOfSync" ? " lv-node-drift" : ""}${r.uid.startsWith("desired:") ? " lv-node-missing" : ""}`}
              onClick={() => onSelect(r.uid)} title={r.health?.message || `${r.kind} ${r.name}`}>
              <span className="lv-node-kind">
                <KindIcon kind={r.kind} size={18} />
                <span className="lv-node-tag">{KIND_SHORT[r.kind] ?? r.kind.toLowerCase()}</span>
              </span>
              <span className="lv-node-main">
                <span className="lv-node-name">{r.name}</span>
                <span className="lv-node-meta">
                  {r.summary.status && r.summary.status !== "Running" && <span className="lv-node-status">{r.summary.status}</span>}
                  {r.summary.ready && <span>{r.summary.ready}</span>}
                  {(r.summary.restarts ?? 0) > 0 && <span className="lv-restarts">↻ {r.summary.restarts}</span>}
                  <span>{relativeTime(r.created_at).replace(" ago", "")}</span>
                </span>
              </span>
              <span className="lv-node-marks">
                {r.sync && <SyncIcon sync={r.sync} />}
                {r.health && <HealthIcon health={r.health.status} size={16} title={r.health.message || r.health.status} />}
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}
