import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import {
  listRegistry,
  listReposPage,
  relativeTime,
  shortSha,
  type RepoSort,
  type RepoSummary,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import Avatar from "../components/Avatar";
import StageDots from "../components/StageDots";
import Dashboard from "../components/Dashboard";

const MAX_REF_CHIPS = 3;
// Cards per page: divides evenly into 2, 3 and 4 grid columns.
const PAGE = 24;

export function ProviderChip({ provider }: { provider: string | null }) {
  if (!provider) return null;
  const label =
    provider === "github"
      ? "GitHub"
      : provider === "bitbucket"
        ? "Bitbucket"
        : provider === "codecommit"
          ? "CodeCommit"
          : "Git";
  return <span className={`provider-chip provider-${provider}`}>{label}</span>;
}

function RepoCard({
  summary,
  provider,
}: {
  summary: RepoSummary;
  provider: string | null;
}) {
  const navigate = useNavigate();
  const { last_pipeline: last } = summary;
  const refs = summary.refs ?? [];
  const shownRefs = refs.slice(0, MAX_REF_CHIPS);
  const extraRefs = (summary.ref_count ?? refs.length) - shownRefs.length;

  // Runs that neither passed nor failed: cancelled (superseded), blocked, or
  // still going. Counting them keeps the tally honest.
  const other = Math.max(
    summary.pipeline_count - summary.success_count - summary.failed_count,
    0
  );

  const lastStatus = last?.status;
  const tint =
    lastStatus === "success"
      ? "green"
      : lastStatus === "failed"
        ? "red"
        : lastStatus === "blocked"
          ? "orange"
          : "neutral";

  return (
    <div
      className={`card repo-card glow glow-${tint}`}
      onClick={() => navigate(`/repos/${encodeURIComponent(summary.repo)}`)}
    >
      <div className="repo-card-top">
        <span className="repo-name">
          {summary.repo} <ProviderChip provider={provider} />
        </span>
        <span className="repo-card-side">
          {summary.last_activity_at && (
            <span className="muted repo-activity">
              {relativeTime(summary.last_activity_at)}
            </span>
          )}
          <Link
            to={`/repos/${encodeURIComponent(summary.repo)}/environments`}
            className="repo-settings-link"
            title="Environments"
            onClick={(e) => e.stopPropagation()}
          >
            ⛴
          </Link>
          <Link
            to={`/repos/${encodeURIComponent(summary.repo)}/settings`}
            className="repo-settings-link"
            title="CI/CD Settings"
            onClick={(e) => e.stopPropagation()}
          >
            ⚙
          </Link>
        </span>
      </div>

      {last ? (
        <div className="repo-last-run">
          {/* whose push the latest run belongs to, at a glance */}
          {last.triggered_by && (
            <Avatar identity={last.triggered_by} size={18} title={last.triggered_by} />
          )}
          <span className="ref-tag">{last.ref}</span>
          <span className="mono sha">{shortSha(last.sha)}</span>
          <StatusBadge status={last.status} />
          <StageDots stages={last.stages ?? []} />
        </div>
      ) : (
        <div className="repo-last-run muted">No pipelines yet</div>
      )}

      {summary.recent_statuses && summary.recent_statuses.length > 0 && (
        <div className="repo-recent" title="Recent runs (newest first)">
          {summary.recent_statuses.slice(0, 5).map((s, i) => (
            <span key={i} className={`mini-dot mini-${s}`} title={s} />
          ))}
        </div>
      )}

      {/* Every run is accounted for. The old tally read
          "3 pipelines · 0 passed · 0 failed", leaving a reader to wonder where
          the other three went — they were cancelled or are still running. */}
      <div className="repo-counts muted">
        {summary.pipeline_count} run{summary.pipeline_count === 1 ? "" : "s"}
        {summary.success_count > 0 && ` · ${summary.success_count} passed`}
        {summary.failed_count > 0 && ` · ${summary.failed_count} failed`}
        {other > 0 && ` · ${other} other`}
      </div>

      {shownRefs.length > 0 && (
        <div className="repo-refs">
          {shownRefs.map((r) => (
            <span key={r} className="ref-tag">
              {r}
            </span>
          ))}
          {extraRefs > 0 && <span className="ref-tag muted">+{extraRefs}</span>}
        </div>
      )}
    </div>
  );
}

export default function Repos() {
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const sort: RepoSort = params.get("sort") === "name" ? "name" : "activity";
  const page = Math.max(0, Number(params.get("page") ?? 0) || 0);

  // Update the URL (so a search can be bookmarked or shared); any change
  // other than paging goes back to page 1.
  const setParam = (key: string, value: string, replace = false) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    if (key !== "page") next.delete("page");
    setParams(next, { replace });
  };

  // The box updates as you type; the URL (and the request) follow a beat later.
  const [draft, setDraft] = useState(q);
  useEffect(() => setDraft(q), [q]);
  useEffect(() => {
    if (draft.trim() === q) return;
    const t = window.setTimeout(() => setParam("q", draft.trim(), true), 250);
    return () => window.clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft]);

  // "/" jumps to the search box, as on most code hosts.
  const searchRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement;
      if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
      if (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName)) return;
      e.preventDefault();
      searchRef.current?.focus();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const key = `${q}|${sort}|${page}`;
  const fetcher = useCallback(
    () => listReposPage(q, sort, page * PAGE, PAGE),
    // key captures q + sort + page
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key]
  );
  const { data, error, loading, refresh } = usePoll(fetcher, 5000);
  useEffect(() => refresh(), [key, refresh]);
  const { data: registry } = usePoll(listRegistry, 0, false);

  const providerOf = (repo: string): string | null =>
    registry?.find((r) => r.repo === repo)?.provider ?? null;

  const repos = data?.items;
  const total = data?.total ?? 0;
  const searching = q !== "";

  return (
    <div>
      <Dashboard />

      <div className="page-head">
        <h1>Repositories</h1>
        <div className="page-head-actions">
          <Link to="/repos/new" className="btn btn-primary">
            Add repository
          </Link>
        </div>
      </div>

      {(total > 0 || searching) && (
        <div className="filter-bar repo-toolbar">
          <input
            ref={searchRef}
            type="search"
            placeholder="Find a repository…  ( / )"
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Escape") {
                setDraft("");
                e.currentTarget.blur();
              }
            }}
            className="repo-search"
            aria-label="Find a repository"
          />
          <select
            value={sort}
            onChange={(e) => setParam("sort", e.target.value === "name" ? "name" : "")}
            aria-label="Sort repositories"
          >
            <option value="activity">Last activity</option>
            <option value="name">Name</option>
          </select>
        </div>
      )}

      {error && <div className="error-banner">Failed to load repos: {error}</div>}
      {loading && !data && <div className="muted">Loading repositories…</div>}

      {repos && repos.length === 0 && !searching && page === 0 && (
        <div className="empty card">
          No repositories yet — <Link to="/repos/new">connect one</Link> to get
          started.
        </div>
      )}
      {repos && repos.length === 0 && (searching || page > 0) && (
        <div className="empty card">
          {searching ? <>No repositories match “{q}”.</> : "Nothing on this page."}{" "}
          <button
            type="button"
            className="btn"
            onClick={() => setParams(new URLSearchParams())}
          >
            Show all
          </button>
        </div>
      )}

      {repos && repos.length > 0 && (
        <div className="repo-grid">
          {repos.map((r) => (
            <RepoCard key={r.repo} summary={r} provider={providerOf(r.repo)} />
          ))}
        </div>
      )}

      {data && total > PAGE && (
        <div className="filter-pager">
          <span className="muted">
            {page * PAGE + 1}–{page * PAGE + data.items.length} of {total}
          </span>
          <button
            type="button"
            className="btn"
            disabled={page === 0}
            onClick={() => setParam("page", String(page - 1))}
          >
            ← Previous
          </button>
          <button
            type="button"
            className="btn"
            disabled={!data.hasMore}
            onClick={() => setParam("page", String(page + 1))}
          >
            Next →
          </button>
        </div>
      )}
    </div>
  );
}
