import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { listChanges, relativeTime, type Change } from "../api";
import Avatar, { displayName } from "../components/Avatar";
import Icon from "../components/Icon";
import NewChange from "../components/NewChange";
import Combobox from "../components/ui/Combobox";
import { Skeleton } from "../components/ui/Pill";
import { CIPill, ChangeStateIcon, GatePill } from "../components/review/ui";
import { useChangeStream } from "../hooks/useChangeStream";

/**
 * The inbox: open pull/merge requests across every repo and host, in one
 * list. It renders from Forge's own copy (kept current by webhooks), so
 * opening it never calls GitHub/GitLab/…; updates arrive live.
 */
export default function Changes() {
  return <ChangeList />;
}

/** Quick views of the list. Ready/Waiting/Blocked/Needs review narrow the
 *  open changes by their merge gate; the rest are states. */
type View = "open" | "review" | "ready" | "waiting" | "blocked" | "merged" | "closed";

const VIEWS: { id: View; label: string; dot?: string }[] = [
  { id: "open", label: "Open" },
  { id: "review", label: "Needs review", dot: "accent" },
  { id: "ready", label: "Ready", dot: "success" },
  { id: "waiting", label: "Waiting", dot: "warning" },
  { id: "blocked", label: "Blocked", dot: "danger" },
  { id: "merged", label: "Merged" },
  { id: "closed", label: "Closed" },
];

const narrow: Record<View, (c: Change) => boolean> = {
  open: () => true,
  review: (c) => !c.draft && c.approvals === 0 && c.changes_requested === 0,
  ready: (c) => c.gate_state === "pass",
  waiting: (c) => c.gate_state === "pending",
  blocked: (c) => c.gate_state === "fail",
  merged: () => true,
  closed: () => true,
};

/** The change list: every repo's (the inbox), or one repo's (its Changes tab). */
export function ChangeList({ fixedRepo }: { fixedRepo?: string }) {
  const navigate = useNavigate();
  const [params, setParams] = useSearchParams();
  // ?view= picks the quick view; older links say ?state=merged|closed.
  const asked = params.get("view") ?? params.get("state") ?? "";
  const view: View = VIEWS.some((v) => v.id === asked) ? (asked as View) : "open";
  const repo = fixedRepo ?? params.get("repo") ?? "";
  const query = params.get("q") ?? "";
  const [q, setQ] = useState(query);
  const [open, setOpen] = useState<Change[] | null>(null);
  const [done, setDone] = useState<Change[] | null>(null);
  const [counts, setCounts] = useState<{ merged?: number; closed?: number }>({});
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);
  const state = view === "merged" || view === "closed" ? view : "open";

  const load = useCallback(() => {
    const fail = (e: unknown) => setError(e instanceof Error ? e.message : String(e));
    listChanges({ state: "open", repo, q: query })
      .then((r) => {
        setOpen(r.changes);
        if (state === "open") setTotal(r.total);
        setError(null);
      })
      .catch(fail);
    Promise.all([
      listChanges({ state: "merged", repo, q: query, limit: 1 }),
      listChanges({ state: "closed", repo, q: query, limit: 1 }),
    ])
      .then(([m, c]) => setCounts({ merged: m.total, closed: c.total }))
      .catch(() => undefined);
    if (state !== "open") {
      listChanges({ state, repo, q: query })
        .then((r) => {
          setDone(r.changes);
          setTotal(r.total);
        })
        .catch(fail);
    }
  }, [state, repo, query]);
  useEffect(load, [load]);
  useChangeStream(load);

  const set = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };
  const repos = useMemo(() => Array.from(new Set((open ?? []).map((c) => c.repo))).sort(), [open]);
  const count = (v: View) => (v === "merged" || v === "closed" ? counts[v] : open?.filter(narrow[v]).length);
  const rows = state === "open" ? (open?.filter(narrow[view]) ?? null) : done;

  return (
    <div className="cr-page">
      {!fixedRepo && (
        <div className="page-head">
          <h1>Changes</h1>
          <div className="page-head-actions">
            <button className="btn btn-primary" onClick={() => setCreating(true)} disabled={creating}>
              <Icon name="plus" size={14} /> New change
            </button>
          </div>
        </div>
      )}
      {creating && <NewChange repo={fixedRepo} onClose={() => setCreating(false)} />}

      <div className="table-toolbar cr-toolbar">
        <div className="state-chips" role="group" aria-label="Show">
          {VIEWS.map((v) => {
            const n = count(v.id);
            return (
              <button
                key={v.id}
                type="button"
                className={`fchip${v.dot ? ` cr-dot-${v.dot}` : ""}${view === v.id ? " fchip-on" : ""}`}
                aria-pressed={view === v.id}
                onClick={() => set("view", v.id === "open" ? "" : v.id)}
              >
                {v.label} {n !== undefined && <span className="num">{n}</span>}
              </button>
            );
          })}
        </div>
        <div className="table-toolbar-right">
          {!fixedRepo && (
            <Combobox
              label="Repo"
              allLabel="All repos"
              placeholder="Repo…"
              value={repo}
              onChange={(v) => set("repo", v)}
              options={repos}
            />
          )}
          <form
            className="cr-search"
            onSubmit={(e) => {
              e.preventDefault();
              set("q", q.trim());
            }}
          >
            <Icon name="search" size={14} />
            <input
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Title or #number"
              aria-label="Search changes"
            />
          </form>
          {fixedRepo && (
            <button className="btn btn-primary" onClick={() => setCreating(true)} disabled={creating}>
              <Icon name="plus" size={14} /> New change
            </button>
          )}
        </div>
      </div>

      {error && <div className="error-banner">{error}</div>}
      <div className="card cr-list">
        {rows === null ? (
          <div className="table-skeleton" aria-busy="true">
            {Array.from({ length: 4 }, (_, i) => (
              <Skeleton key={i} h={36} />
            ))}
          </div>
        ) : rows.length === 0 ? (
          <div className="cr-empty">
            <Icon name="changes" size={24} />
            <div className="cr-empty-title">
              {view === "open" ? "No open changes" : `Nothing ${VIEWS.find((v) => v.id === view)?.label.toLowerCase()}`}
            </div>
            <div className="muted">
              Pull and merge requests appear here by themselves, from every connected host. On a plain git repo, open
              one with New change.
            </div>
          </div>
        ) : (
          rows.map((c) => (
            <div
              key={c.id}
              className="cr-row"
              role="link"
              tabIndex={0}
              onClick={() => navigate(`/changes/${c.id}`)}
              onKeyDown={(e) => e.key === "Enter" && navigate(`/changes/${c.id}`)}
            >
              <ChangeStateIcon change={c} />
              <div className="cr-row-main">
                <div className="cr-row-title">
                  {c.title || <span className="muted">(untitled)</span>}
                  {c.draft && c.state === "open" && <span className="badge2">draft</span>}
                  {c.labels.map((l) => (
                    <span key={l} className="chip">
                      {l}
                    </span>
                  ))}
                </div>
                <div className="cr-row-meta">
                  <span className="mono">#{c.number}</span>
                  {!fixedRepo && <span>{c.repo}</span>}
                  <span className="cr-who">
                    <Avatar identity={c.author} size={14} /> {displayName(c.author)}
                  </span>
                  <span className="mono cr-branch" title={`${c.source_branch} → ${c.target_branch}`}>
                    {c.source_branch} → {c.target_branch}
                  </span>
                  {c.updated_at && <span>updated {relativeTime(c.updated_at)}</span>}
                </div>
              </div>
              <div className="cr-row-side">
                <CIPill status={c.pipeline_status} short />
                {c.state === "open" && <GatePill state={c.gate_state} summary={c.gate_summary} />}
                <span className="cr-votes" title={`${c.approvals} approved, ${c.changes_requested} want changes`}>
                  {c.approvals > 0 && (
                    <span className="cr-vote-ok">
                      <Icon name="check" size={12} /> {c.approvals}
                    </span>
                  )}
                  {c.changes_requested > 0 && (
                    <span className="cr-vote-no">
                      <Icon name="close" size={12} /> {c.changes_requested}
                    </span>
                  )}
                </span>
              </div>
            </div>
          ))
        )}
      </div>
      {rows && total > rows.length && view === state && (
        <p className="muted">
          Showing {rows.length} of {total}. Search or pick a repo to narrow it down.
        </p>
      )}
    </div>
  );
}
