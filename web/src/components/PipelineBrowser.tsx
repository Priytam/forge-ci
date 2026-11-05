import Skeleton from "./ui/Skeleton";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  createPipelineView,
  deletePipelineView,
  filterToParams,
  getMe,
  getPipelineFacets,
  listPipelinesPage,
  listPipelineViews,
  paramsToFilter,
  PIPELINE_STATUSES,
  updatePipelineView,
  type PipelineFacets,
  type PipelineFilter,
  type PipelineView,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import PipelineTable from "./PipelineTable";
import EnvTabs from "./EnvTabs";
import { STATUS_LABEL } from "./StatusBadge";

const PAGE = 50;

/** Checkbox popover for picking several repos. */
function RepoPicker({
  all,
  selected,
  onChange,
}: {
  all: string[];
  selected: string[];
  onChange: (repos: string[]) => void;
}) {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [open]);

  const toggle = (r: string) =>
    onChange(selected.includes(r) ? selected.filter((x) => x !== r) : [...selected, r]);
  const shown = all.filter((r) => r.toLowerCase().includes(q.toLowerCase()));
  const label =
    selected.length === 0
      ? "All repos"
      : selected.length === 1
        ? selected[0]
        : `${selected.length} repos`;

  return (
    <div className="filter-picker" ref={ref}>
      <button type="button" className="btn" onClick={() => setOpen((o) => !o)}>
        {label} ▾
      </button>
      {open && (
        <div className="filter-popover">
          <input
            placeholder="Find a repo"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            autoFocus
          />
          <div className="filter-popover-list">
            {shown.map((r) => (
              <label key={r} className="check-row">
                <input
                  type="checkbox"
                  checked={selected.includes(r)}
                  onChange={() => toggle(r)}
                />
                {r}
              </label>
            ))}
            {shown.length === 0 && <div className="muted">No match</div>}
          </div>
          {selected.length > 0 && (
            <button type="button" className="btn" onClick={() => onChange([])}>
              Clear
            </button>
          )}
        </div>
      )}
    </div>
  );
}

/**
 * Filterable, paginated pipeline list. Filter state lives in the URL
 * (?repo=&ref=&status=&runner=&environment=&page=), so a filtered list can be
 * bookmarked or shared; filtering happens on the server. With fixedRepo the
 * list is scoped to that repo and the repo picker and saved views are hidden.
 */
export default function PipelineBrowser({ fixedRepo }: { fixedRepo?: string }) {
  const [params, setParams] = useSearchParams();
  const urlFilter = useMemo(() => paramsToFilter(params), [params]);
  const filter: PipelineFilter = useMemo(
    () => (fixedRepo ? { ...urlFilter, repos: [fixedRepo] } : urlFilter),
    [fixedRepo, urlFilter]
  );
  const page = Math.max(0, Number(params.get("page") ?? 0) || 0);
  const viewId = Number(params.get("view") ?? 0) || 0;
  const key = filterToParams(filter).toString() + `&page=${page}`;

  // Replace the filter (and drop paging) — every change starts at page 1.
  const setFilter = (f: PipelineFilter, keepView = false) => {
    const next = filterToParams(fixedRepo ? { ...f, repos: [] } : f);
    if (keepView && viewId) next.set("view", String(viewId));
    setParams(next);
  };
  // Editing filters keeps the open view selected so it can be updated.
  const patch = (p: Partial<PipelineFilter>) => setFilter({ ...urlFilter, ...p }, true);

  const fetcher = useCallback(
    () => listPipelinesPage(filter, page * PAGE, PAGE),
    // key captures filter + page
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key]
  );
  const { data, error, loading, refresh } = usePoll(fetcher, 3000);
  useEffect(() => refresh(), [key, refresh]);

  const [facets, setFacets] = useState<PipelineFacets | null>(null);
  const facetRepos = filter.repos ?? [];
  const facetKey = facetRepos.join(",");
  useEffect(() => {
    void getPipelineFacets(facetRepos)
      .then(setFacets)
      .catch(() => setFacets(null));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [facetKey]);

  // ---- saved views ----
  const [views, setViews] = useState<PipelineView[]>([]);
  const [me, setMe] = useState<string>("");
  const [viewError, setViewError] = useState<string | null>(null);
  const reloadViews = useCallback(() => {
    void listPipelineViews()
      .then(setViews)
      .catch(() => setViews([]));
  }, []);
  useEffect(() => {
    if (fixedRepo) return;
    reloadViews();
    void getMe().then((u) => setMe(u?.email ?? ""));
  }, [fixedRepo, reloadViews]);
  const current = views.find((v) => v.id === viewId) ?? null;
  const mine = (v: PipelineView) => v.owner.toLowerCase() === me.toLowerCase();

  const openView = (id: number) => {
    const v = views.find((x) => x.id === id);
    if (!v) {
      setParams(new URLSearchParams());
      return;
    }
    const next = filterToParams(v.filters);
    next.set("view", String(v.id));
    setParams(next);
  };

  const saveAsNew = async () => {
    const name = window.prompt("Name this view", current ? `${current.name} copy` : "");
    if (!name) return;
    const shared = window.confirm(
      "Share this view with everyone in your org? (OK = shared, Cancel = only you)"
    );
    setViewError(null);
    try {
      const v = await createPipelineView({ name, filters: urlFilter, shared });
      reloadViews();
      const next = filterToParams(v.filters);
      next.set("view", String(v.id));
      setParams(next);
    } catch (err) {
      setViewError(err instanceof Error ? err.message : String(err));
    }
  };

  const updateCurrent = async () => {
    if (!current) return;
    setViewError(null);
    try {
      await updatePipelineView(current.id, {
        name: current.name,
        filters: urlFilter,
        shared: current.shared,
      });
      reloadViews();
    } catch (err) {
      setViewError(err instanceof Error ? err.message : String(err));
    }
  };

  const toggleShare = async () => {
    if (!current) return;
    try {
      await updatePipelineView(current.id, {
        name: current.name,
        filters: current.filters,
        shared: !current.shared,
      });
      reloadViews();
    } catch (err) {
      setViewError(err instanceof Error ? err.message : String(err));
    }
  };

  const removeCurrent = async () => {
    if (!current || !window.confirm(`Delete the view "${current.name}"?`)) return;
    try {
      await deletePipelineView(current.id);
      reloadViews();
      setParams(new URLSearchParams());
    } catch (err) {
      setViewError(err instanceof Error ? err.message : String(err));
    }
  };

  const dirty =
    current !== null &&
    filterToParams(current.filters).toString() !== filterToParams(urlFilter).toString();
  const statuses = urlFilter.statuses ?? [];
  const anyFilter = filterToParams(urlFilter).toString() !== "";
  const total = data?.total ?? 0;

  return (
    <div>
      <EnvTabs
        envs={facets?.environments ?? []}
        value={urlFilter.environment ?? ""}
        onChange={(environment) => patch({ environment })}
      />
      <div className="card filter-panel">
      <div className="filter-bar">
        {!fixedRepo && (
          <RepoPicker
            all={facets?.repos ?? []}
            selected={urlFilter.repos ?? []}
            onChange={(repos) => patch({ repos })}
          />
        )}
        <select
          value={urlFilter.ref ?? ""}
          onChange={(e) => patch({ ref: e.target.value })}
          aria-label="Branch"
        >
          <option value="">All branches</option>
          {urlFilter.ref && !facets?.refs.includes(urlFilter.ref) && (
            <option value={urlFilter.ref}>{urlFilter.ref}</option>
          )}
          {facets?.refs.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
        <select
          value={urlFilter.runner ?? ""}
          onChange={(e) => patch({ runner: e.target.value })}
          aria-label="Runner"
        >
          <option value="">All runners</option>
          {urlFilter.runner && !facets?.runners.includes(urlFilter.runner) && (
            <option value={urlFilter.runner}>{urlFilter.runner}</option>
          )}
          {facets?.runners.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
        <div className="state-chips" role="group" aria-label="Status">
          {PIPELINE_STATUSES.map((st) => (
            <button
              key={st}
              type="button"
              className={`fchip fdot-${st}${statuses.includes(st) ? " fchip-on" : ""}`}
              aria-pressed={statuses.includes(st)}
              onClick={() =>
                patch({
                  statuses: statuses.includes(st)
                    ? statuses.filter((x) => x !== st)
                    : [...statuses, st],
                })
              }
            >
              {STATUS_LABEL[st] ?? st}
            </button>
          ))}
        </div>
        {anyFilter && (
          <button type="button" className="btn btn-ghost" onClick={() => setFilter({})}>
            Clear filters
          </button>
        )}
      {!fixedRepo && (
        <div className="filter-views-inline">
          <select
            aria-label="Saved view"
            value={viewId || ""}
            onChange={(e) => openView(Number(e.target.value))}
          >
            <option value="">{views.length ? "Saved views…" : "No saved views"}</option>
            {views.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
                {v.shared ? " (shared)" : ""}
                {!mine(v) ? ` — ${v.owner || "anonymous"}` : ""}
              </option>
            ))}
          </select>
          <button type="button" className="btn" onClick={() => void saveAsNew()}>
            Save view
          </button>
          {current && mine(current) && (
            <>
              {dirty && (
                <button type="button" className="btn btn-primary" onClick={() => void updateCurrent()}>
                  Update “{current.name}”
                </button>
              )}
              <button type="button" className="btn" onClick={() => void toggleShare()}>
                {current.shared ? "Make private" : "Share with org"}
              </button>
              <button type="button" className="btn" onClick={() => void removeCurrent()}>
                Delete view
              </button>
            </>
          )}
          {viewError && <span className="approval-error">{viewError}</span>}
        </div>
      )}
      </div>
      </div>

      {error && <div className="error-banner">Failed to load pipelines: {error}</div>}
      {loading && !data && <Skeleton />}
      {data && data.items.length === 0 && (
        <div className="empty card">
          {anyFilter ? "No pipelines match these filters." : "No pipelines yet."}
        </div>
      )}
      {data && data.items.length > 0 && <PipelineTable pipelines={data.items} showRepo={!fixedRepo} />}
      {data && total > 0 && (
        <div className="filter-pager">
          <span className="muted">
            {page * PAGE + 1}–{page * PAGE + data.items.length} of {total}
          </span>
          <button
            type="button"
            className="btn"
            disabled={page === 0}
            onClick={() => {
              const next = new URLSearchParams(params);
              next.set("page", String(page - 1));
              setParams(next);
            }}
          >
            ← Newer
          </button>
          <button
            type="button"
            className="btn"
            disabled={!data.hasMore}
            onClick={() => {
              const next = new URLSearchParams(params);
              next.set("page", String(page + 1));
              setParams(next);
            }}
          >
            Older →
          </button>
        </div>
      )}
    </div>
  );
}
