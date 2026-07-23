import { Link, useNavigate } from "react-router-dom";
import {
  listRegistry,
  listRepos,
  relativeTime,
  shortSha,
  type RepoSummary,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import StageDots from "../components/StageDots";

const MAX_REF_CHIPS = 3;

export function ProviderChip({ provider }: { provider: string | null }) {
  if (!provider) return null;
  const label =
    provider === "github" ? "GitHub" : provider === "bitbucket" ? "Bitbucket" : "Git";
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
  const extraRefs = refs.length - shownRefs.length;

  return (
    <div
      className="card repo-card"
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

      <div className="repo-counts muted">
        {summary.pipeline_count} pipeline{summary.pipeline_count === 1 ? "" : "s"}
        {" · "}
        {summary.success_count} passed
        {" · "}
        {summary.failed_count} failed
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
  const { data: repos, error, loading } = usePoll(listRepos, 5000);
  const { data: registry } = usePoll(listRegistry, 0, false);

  const providerOf = (repo: string): string | null =>
    registry?.find((r) => r.repo === repo)?.provider ?? null;

  return (
    <div>
      <div className="page-head">
        <h1>Repositories</h1>
        <div className="page-head-actions">
          <Link to="/repos/new" className="btn btn-primary">
            Add repository
          </Link>
        </div>
      </div>

      {error && <div className="error-banner">Failed to load repos: {error}</div>}
      {loading && !repos && <div className="muted">Loading repositories…</div>}

      {repos && repos.length === 0 && (
        <div className="empty card">
          No repositories yet — <Link to="/repos/new">connect one</Link> to get
          started.
        </div>
      )}

      {repos && repos.length > 0 && (
        <div className="repo-grid">
          {repos.map((r) => (
            <RepoCard key={r.repo} summary={r} provider={providerOf(r.repo)} />
          ))}
        </div>
      )}
    </div>
  );
}
