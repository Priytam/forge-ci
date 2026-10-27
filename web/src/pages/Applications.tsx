import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { listLiveApps, relativeTime, subscribeLive, type LiveApp, type LiveAppsPage, type LiveFacet } from "../api";
import Avatar, { displayName } from "../components/Avatar";
import Icon from "../components/Icon";
import { HEALTH_ORDER, HEALTH_TONE, HealthIcon, HealthPill, KindIcon, podsLabel, shortImage, SyncPill } from "../components/live/ui";
import { Skeleton } from "../components/ui/Pill";

const FACETS: { id: LiveFacet; label: string }[] = [
  { id: "health", label: "Health" },
  { id: "sync", label: "Sync status" },
  { id: "environment", label: "Environment" },
  { id: "repo", label: "Repo" },
  { id: "target", label: "Cluster / project" },
  { id: "namespace", label: "Namespace / region" },
  { id: "group", label: "Part of" },
];

/** How many values a facet lists before "Show all". */
const FACET_SHOW = 8;

/**
 * Every application Forge Live sees, across clusters and clouds: tiles or a
 * list, filtered on the left with counts per value. Rows update in place as
 * agents report; the page never asks a cluster anything.
 */
export default function Applications() {
  return <ApplicationList />;
}

/** The application list: every repo's, or one repo's (its Applications tab). */
export function ApplicationList({ fixedRepo }: { fixedRepo?: string }) {
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  const mode = params.get("view") === "list" ? "list" : "tiles";
  const query = params.get("q") ?? "";
  const [search, setSearch] = useState(query);
  const [page, setPage] = useState<LiveAppsPage | null>(null);
  const [error, setError] = useState<string | null>(null);
  const selected = (f: LiveFacet) => (params.get(f) ?? "").split(",").filter(Boolean);
  const key = params.toString();

  const load = useCallback(() => {
    const filter: Partial<Record<LiveFacet, string[]>> & { q?: string } = { q: query };
    for (const f of FACETS) filter[f.id] = (params.get(f.id) ?? "").split(",").filter(Boolean);
    if (fixedRepo) filter.repo = [fixedRepo];
    listLiveApps(filter).then(
      (p) => {
        setPage(p);
        setError(null);
      },
      (e) => setError(String(e?.message ?? e))
    );
  }, [key, fixedRepo]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(load, [load]);

  // Live: rows change in place at once; counts follow with one refetch per
  // burst (ETag'd, so an unchanged list costs no body).
  const refetch = useRef<number>();
  useEffect(() => {
    const soon = () => {
      window.clearTimeout(refetch.current);
      refetch.current = window.setTimeout(load, 1500);
    };
    const stop = subscribeLive(undefined, {
      ready: load,
      app: (row) => {
        setPage((p) => (p ? { ...p, apps: p.apps.map((a) => (a.id === row.id ? row : a)) } : p));
        soon();
      },
      gone: (id) => {
        setPage((p) => (p ? { ...p, apps: p.apps.filter((a) => a.id !== id) } : p));
        soon();
      },
    });
    return () => {
      window.clearTimeout(refetch.current);
      stop();
    };
  }, [load]);

  const set = (changes: Record<string, string>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(changes)) {
      if (v) next.set(k, v);
      else next.delete(k);
    }
    setParams(next, { replace: true });
  };
  useEffect(() => setSearch(query), [query]);
  useEffect(() => {
    if (search === query) return;
    const t = window.setTimeout(() => set({ q: search }), 250);
    return () => window.clearTimeout(t);
  }, [search]); // eslint-disable-line react-hooks/exhaustive-deps

  const toggle = (f: LiveFacet, v: string) => {
    const cur = selected(f);
    set({ [f]: (cur.includes(v) ? cur.filter((x) => x !== v) : [...cur, v]).join(",") });
  };
  const filtered = FACETS.some((f) => selected(f.id).length > 0) || query !== "";
  const facets = fixedRepo ? FACETS.filter((f) => f.id !== "repo") : FACETS;
  const views = (
    <div className="segmented" role="group" aria-label="View">
      <button type="button" className={mode === "tiles" ? "seg-btn seg-active" : "seg-btn"} onClick={() => set({ view: "" })}>Tiles</button>
      <button type="button" className={mode === "list" ? "seg-btn seg-active" : "seg-btn"} onClick={() => set({ view: "list" })}>List</button>
    </div>
  );

  return (
    <div className="lv-apps">
      {fixedRepo ? (
        <div className="lv-repo-bar">{views}</div>
      ) : (
        <div className="page-head">
          <h1>Applications</h1>
          <div className="page-head-actions">{views}</div>
        </div>
      )}
      <div className="lv-apps-layout">
        <aside className="lv-filters card" aria-label="Filters">
          <div className="cr-search lv-filter-search">
            <Icon name="search" size={14} />
            <input placeholder="Search apps…" value={search} onChange={(e) => setSearch(e.target.value)} aria-label="Search applications" />
          </div>
          {filtered && (
            <button type="button" className="link-btn lv-clear" onClick={() => setParams(mode === "list" ? { view: "list" } : {}, { replace: true })}>
              Clear filters
            </button>
          )}
          {page && facets.map((f) => <Facet key={f.id} id={f.id} label={f.label} counts={page.facets[f.id] ?? {}} chosen={selected(f.id)} onToggle={(v) => toggle(f.id, v)} />)}
        </aside>
        <div className="lv-apps-main">
          {error && <div className="error-banner">Failed to load: {error}</div>}
          {!page && !error && <Skeleton h={180} />}
          {page && (
            <div className="lv-count muted">
              {page.total === 1 ? "1 application" : `${page.total} applications`}
              {page.apps.length < page.total && ` · showing ${page.apps.length}`}
            </div>
          )}
          {page && page.total === 0 && (
            <div className="empty card">
              {filtered ? (
                "No applications match."
              ) : fixedRepo ? (
                <>
                  No running applications are linked to {fixedRepo} yet. An application belongs to the repo its
                  cluster or project deploys from — set under <Link to="/admin/live">Settings → Live agents</Link>.
                </>
              ) : (
                <>
                  No applications yet. Connect a cluster or a cloud project under{" "}
                  <Link to="/admin/live">Settings → Live agents</Link>: its agent reports what runs there, and it shows up here.
                </>
              )}
            </div>
          )}
          {page && page.apps.length > 0 && mode === "tiles" && (
            <div className="lv-tiles">
              {page.apps.map((a) => (
                <Tile key={a.id} a={a} />
              ))}
            </div>
          )}
          {page && page.apps.length > 0 && mode === "list" && (
            <div className="card table-card lv-app-table">
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Health</th>
                    <th>Sync</th>
                    <th>Environment</th>
                    <th>Cluster / project</th>
                    <th>Namespace</th>
                    <th>Image</th>
                    <th>Last deployed</th>
                  </tr>
                </thead>
                <tbody>
                  {page.apps.map((a) => (
                    <tr key={a.id} onClick={() => navigate(`/applications/${a.id}`)} className="lv-click">
                      <td><Link to={`/applications/${a.id}`} className="lv-app-name">{a.name}</Link></td>
                      <td><HealthPill health={a.health} title={a.health_message} /></td>
                      <td><SyncPill sync={a.sync} /></td>
                      <td>{a.environment || <span className="muted">—</span>}</td>
                      <td className="muted">{a.target}</td>
                      <td className="muted">{a.namespace}</td>
                      <td className="mono lv-ellipsis" title={a.images.join("\n")}>{shortImage(a.images[0])}</td>
                      <td className="muted">{a.deployed ? `${displayName(a.deployed.by) || "unknown"}, ${relativeTime(a.deployed.at)}` : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function Facet({ id, label, counts, chosen, onToggle }: {
  id: LiveFacet;
  label: string;
  counts: Record<string, number>;
  chosen: string[];
  onToggle: (v: string) => void;
}) {
  const [all, setAll] = useState(false);
  const values = Object.keys(counts);
  for (const c of chosen) if (!values.includes(c)) values.push(c);
  if (id === "health") values.sort((a, b) => HEALTH_ORDER.indexOf(a as never) - HEALTH_ORDER.indexOf(b as never));
  else values.sort((a, b) => (counts[b] ?? 0) - (counts[a] ?? 0) || a.localeCompare(b));
  if (values.length === 0 || (values.length === 1 && values[0] === "" && chosen.length === 0)) return null;
  const shown = all ? values : values.slice(0, FACET_SHOW);
  return (
    <section className="lv-facet">
      <h3>{label}</h3>
      {shown.map((v) => (
        <label key={v} className="lv-facet-row">
          <input type="checkbox" checked={chosen.includes(v)} onChange={() => onToggle(v)} />
          {id === "health" && <HealthIcon health={v as never} size={12} />}
          <span className="lv-facet-v">{v || <span className="muted">none</span>}</span>
          <span className="num">{counts[v] ?? 0}</span>
        </label>
      ))}
      {values.length > FACET_SHOW && (
        <button type="button" className="link-btn" onClick={() => setAll((x) => !x)}>
          {all ? "Show fewer" : `Show all ${values.length}`}
        </button>
      )}
    </section>
  );
}

function Tile({ a }: { a: LiveApp }) {
  return (
    <Link to={`/applications/${a.id}`} className={`card lv-tile lv-tile-${HEALTH_TONE[a.health]}`} title={a.health_message || undefined}>
      <div className="lv-tile-head">
        <KindIcon kind="App" size={16} />
        <span className="lv-tile-name">{a.name}</span>
      </div>
      <div className="lv-tile-status">
        <HealthPill health={a.health} />
        <SyncPill sync={a.sync} />
      </div>
      <dl className="lv-tile-facts">
        <dt>Where</dt>
        <dd>{a.target}{a.namespace && <span className="muted"> / {a.namespace}</span>}</dd>
        {a.environment && (
          <>
            <dt>Env</dt>
            <dd>{a.environment}</dd>
          </>
        )}
        {a.images[0] && (
          <>
            <dt>Image</dt>
            <dd className="mono lv-ellipsis" title={a.images.join("\n")}>{shortImage(a.images[0])}</dd>
          </>
        )}
        <dt>Deployed</dt>
        <dd className="lv-ellipsis">
          {a.deployed ? (
            <span className="actor">
              <Avatar identity={a.deployed.by} size={14} /> {displayName(a.deployed.by) || "unknown"}, {relativeTime(a.deployed.at)}
            </span>
          ) : (
            <span className="muted">not by Forge</span>
          )}
        </dd>
      </dl>
      <div className="lv-tile-foot muted">
        {[podsLabel(a), `${a.resources} resources`].filter(Boolean).join(" · ")}
      </div>
    </Link>
  );
}
