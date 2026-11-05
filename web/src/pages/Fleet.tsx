import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { getFleet, listLiveTemplates, relativeTime, shortSha, subscribeLive, type FleetCell, type FleetPage, type FleetRow, type LiveTemplate } from "../api";
import { HealthIcon, shortImage, SyncIcon } from "../components/live/ui";
import { Skeleton } from "../components/ui/Pill";

/** The image most of a row's targets run: what the others are measured against. */
export function majorityImage(row: FleetRow): string {
  const n = new Map<string, number>();
  for (const c of Object.values(row.cells)) {
    const img = c.images[0] ?? "";
    if (img) n.set(img, (n.get(img) ?? 0) + 1);
  }
  let best = "";
  for (const [img, k] of n) if (k > (n.get(best) ?? 0)) best = img;
  return best;
}

/**
 * The fleet: every application by name, one column per cluster or project.
 * Each cell is that app there — health, sync, the image it runs — and an
 * image that differs from what most of the fleet runs is marked, so version
 * skew across clusters shows at a glance. Click a cell for the app.
 */
export default function Fleet() {
  const [params, setParams] = useSearchParams();
  const env = params.get("environment") ?? "";
  const template = params.get("template") ?? "";
  const q = params.get("q") ?? "";
  const [search, setSearch] = useState(q);
  const [page, setPage] = useState<FleetPage | null>(null);
  const [templates, setTemplates] = useState<LiveTemplate[]>([]);
  const [envs, setEnvs] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };

  const load = useCallback(() => {
    getFleet({ environment: env, template, q }).then(
      (p) => {
        setPage(p);
        setError(null);
        if (!env) setEnvs([...new Set(p.targets.map((t) => t.environment ?? "").filter(Boolean))].sort());
      },
      (e) => setError(String(e?.message ?? e))
    );
  }, [env, template, q]);
  useEffect(load, [load]);
  useEffect(() => {
    listLiveTemplates().then(setTemplates, () => undefined);
  }, []);
  useEffect(() => {
    const t = window.setTimeout(() => set("q", search.trim()), 250);
    return () => window.clearTimeout(t);
  }, [search]); // eslint-disable-line react-hooks/exhaustive-deps
  // Live: one refetch per burst of changes anywhere in the fleet.
  const soon = useRef<number>();
  useEffect(() => {
    const again = () => {
      window.clearTimeout(soon.current);
      soon.current = window.setTimeout(load, 1500);
    };
    const stop = subscribeLive(undefined, { ready: load, app: again, gone: again });
    return () => {
      window.clearTimeout(soon.current);
      stop();
    };
  }, [load]);

  const totals = useMemo(() => {
    const out = new Map<number, { apps: number; bad: number }>();
    for (const r of page?.rows ?? []) {
      for (const [tid, c] of Object.entries(r.cells)) {
        const t = out.get(Number(tid)) ?? { apps: 0, bad: 0 };
        t.apps++;
        if (c.health !== "Healthy" || c.sync === "OutOfSync") t.bad++;
        out.set(Number(tid), t);
      }
    }
    return out;
  }, [page]);

  return (
    <div className="lv-fleet-page">
      <div className="page-head">
        <h1>Fleet</h1>
        <div className="page-head-actions">
          <Link className="btn" to="/applications">Applications</Link>
          <Link className="btn" to="/applications/templates">Templates</Link>
        </div>
      </div>
      <div className="table-toolbar">
        <select value={env} onChange={(e) => set("environment", e.target.value)} aria-label="Environment">
          <option value="">All environments</option>
          {envs.map((e) => <option key={e}>{e}</option>)}
        </select>
        <select value={template} onChange={(e) => set("template", e.target.value)} aria-label="Template">
          <option value="">All applications</option>
          {templates.map((t) => <option key={t.id} value={t.name}>Template: {t.name}</option>)}
        </select>
        <div className="table-toolbar-right">
          <input className="filter-search" placeholder="Find an application…" value={search} onChange={(e) => setSearch(e.target.value)} aria-label="Find an application" />
        </div>
      </div>
      {error && <div className="error-banner">{error}</div>}
      {!page && !error && <Skeleton h={240} />}
      {page && page.rows.length === 0 && <div className="empty card">No applications match.</div>}
      {page && page.rows.length > 0 && (
        <div className="lv-fleet-wrap card">
          <table className="lv-fleet">
            <thead>
              <tr>
                <th className="lv-fleet-app">Application <span className="muted">{page.total}</span></th>
                {page.targets.map((t) => {
                  const tot = totals.get(t.id);
                  const stale = !t.last_seen_at || Date.now() - Date.parse(t.last_seen_at) > 120_000;
                  return (
                    <th key={t.id} title={`${t.provider} · agent ${t.agent_version || "?"} · seen ${t.last_seen_at ? relativeTime(t.last_seen_at) : "never"}`}>
                      <div className="lv-fleet-target">
                        <span className={`lv-fleet-dot${stale ? " lv-fleet-dot-stale" : ""}`} /> {t.name}
                      </div>
                      <div className="lv-fleet-env muted">
                        {t.environment || "—"}
                        {tot && <> · {tot.bad ? <span className="lv-bad">{tot.bad} need attention</span> : `${tot.apps} ok`}</>}
                      </div>
                    </th>
                  );
                })}
              </tr>
            </thead>
            <tbody>
              {page.rows.map((r) => {
                const major = majorityImage(r);
                return (
                  <tr key={r.name}>
                    <th className="lv-fleet-app" scope="row">
                      <span className="lv-fleet-name">{r.name}</span>
                      <span className="muted lv-fleet-sub">
                        {r.group}
                        {r.template && <Link className="lv-fleet-tmpl" to={`/applications/fleet?template=${encodeURIComponent(r.template)}`}>template {r.template}</Link>}
                      </span>
                    </th>
                    {page.targets.map((t) => {
                      const c = r.cells[String(t.id)];
                      return <td key={t.id}>{c ? <Cell c={c} major={major} /> : <span className="lv-fleet-none" aria-label="not here">·</span>}</td>;
                    })}
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {page && page.total > page.rows.length && <p className="muted">Showing {page.rows.length} of {page.total} applications — narrow the search.</p>}
    </div>
  );
}

function Cell({ c, major }: { c: FleetCell; major: string }) {
  const img = c.images[0] ?? "";
  const skew = !!img && !!major && img !== major;
  return (
    <Link to={`/applications/${c.app_id}`} className={`lv-fleet-cell lv-fleet-${c.health.toLowerCase()}${skew ? " lv-fleet-skew" : ""}`}
      title={`${c.health} · ${c.sync}${c.revision ? ` at ${shortSha(c.revision)}` : ""}${img ? `\n${img}` : ""}${skew ? `\nmost of the fleet runs ${shortImage(major)}` : ""}`}>
      <HealthIcon health={c.health} size={14} />
      {c.sync !== "Unknown" && <SyncIcon sync={c.sync} size={13} />}
      <span className="lv-fleet-img mono">{shortImage(img).split(":").pop()}</span>
      {c.pods > 0 && <span className="muted lv-fleet-pods">{c.pods_ready}/{c.pods}</span>}
    </Link>
  );
}
