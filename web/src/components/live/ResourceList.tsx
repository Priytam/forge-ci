import { useState } from "react";
import { relativeTime, type LiveResource } from "../../api";
import { HEALTH_ORDER, HealthPill, KindIcon } from "./ui";

type Key = "kind" | "name" | "health" | "ready" | "restarts" | "age";

const SORT: Record<Key, (a: LiveResource, b: LiveResource) => number> = {
  kind: (a, b) => a.kind.localeCompare(b.kind),
  name: (a, b) => a.name.localeCompare(b.name),
  health: (a, b) => rank(a) - rank(b),
  ready: (a, b) => (a.summary.ready ?? "").localeCompare(b.summary.ready ?? ""),
  restarts: (a, b) => (a.summary.restarts ?? 0) - (b.summary.restarts ?? 0),
  age: (a, b) => Date.parse(a.created_at) - Date.parse(b.created_at),
};

function rank(r: LiveResource): number {
  return r.health ? HEALTH_ORDER.indexOf(r.health.status) : HEALTH_ORDER.length;
}

/** The application's resources as a sortable table. */
export default function ResourceList({ resources, selected, onSelect }: {
  resources: LiveResource[];
  selected: string | null;
  onSelect: (uid: string) => void;
}) {
  const [sort, setSort] = useState<{ key: Key; dir: 1 | -1 }>({ key: "health", dir: 1 });
  const rows = [...resources].sort((a, b) => sort.dir * SORT[sort.key](a, b) || a.kind.localeCompare(b.kind) || a.name.localeCompare(b.name));
  const th = (key: Key, label: string) => (
    <th aria-sort={sort.key === key ? (sort.dir === 1 ? "ascending" : "descending") : undefined}>
      <button type="button" className="th-sort" onClick={() => setSort({ key, dir: sort.key === key ? (-sort.dir as 1 | -1) : 1 })}>
        {label}
        <span className="th-arrow" aria-hidden="true">{sort.key === key ? (sort.dir === 1 ? "↑" : "↓") : ""}</span>
      </button>
    </th>
  );
  return (
    <div className="card table-card lv-list">
      <table>
        <thead>
          <tr>
            {th("kind", "Kind")}
            {th("name", "Name")}
            {th("health", "Health")}
            <th>Status</th>
            {th("ready", "Ready")}
            {th("restarts", "Restarts")}
            {th("age", "Age")}
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.uid} className={selected === r.uid ? "lv-row-on" : undefined} onClick={() => onSelect(r.uid)}>
              <td className="lv-list-kind"><KindIcon kind={r.kind} size={14} /> {r.kind}</td>
              <td><button type="button" className="link-btn" onClick={() => onSelect(r.uid)}>{r.name}</button></td>
              <td>{r.health ? <HealthPill health={r.health.status} title={r.health.message} /> : <span className="muted">—</span>}</td>
              <td className="muted">{r.summary.status ?? ""}</td>
              <td className="num">{r.summary.ready ?? ""}</td>
              <td className="num">{r.summary.restarts ?? ""}</td>
              <td className="muted" title={r.created_at}>{relativeTime(r.created_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
