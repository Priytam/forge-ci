import { useCallback } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import {
  decodeRepoParam,
  listPipelines,
  listRegistry,
  relativeTime,
  shortSha,
  sourceLabel,
} from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";
import StageDots from "../components/StageDots";
import ConfigChip from "../components/ConfigChip";
import { ProviderChip } from "./Repos";

export default function PipelineList() {
  const navigate = useNavigate();
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");

  const fetchPipelines = useCallback(() => listPipelines(repo), [repo]);
  const { data: pipelines, error, loading } = usePoll(fetchPipelines, 2000);
  const { data: registry } = usePoll(listRegistry, 0, false);
  const provider = registry?.find((r) => r.repo === repo)?.provider ?? null;

  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>{repo}</span>
      </div>

      <div className="page-head">
        <h1>
          {repo} <ProviderChip provider={provider} />
        </h1>
        <div className="page-head-actions">
          <Link
            to={`/repos/${encodeURIComponent(repo)}/environments`}
            className="btn"
            title="Environments"
          >
            Environments
          </Link>
          <Link
            to={`/repos/${encodeURIComponent(repo)}/settings`}
            className="btn"
            title="CI/CD Settings"
          >
            ⚙ Settings
          </Link>
          <Link to="/new" className="btn btn-primary">
            Run pipeline
          </Link>
        </div>
      </div>

      {error && <div className="error-banner">Failed to load pipelines: {error}</div>}
      {loading && !pipelines && <div className="muted">Loading pipelines…</div>}

      {pipelines && pipelines.length === 0 && (
        <div className="empty card">
          No pipelines for this repo yet. <Link to="/new">Create one</Link> to get
          started.
        </div>
      )}

      {pipelines && pipelines.length > 0 && (
        <div className="card table-card">
          <table>
            <thead>
              <tr>
                <th>ID</th>
                <th>Repo</th>
                <th>Ref</th>
                <th>SHA</th>
                <th>Stages</th>
                <th>Status</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {pipelines.map((p) => (
                <tr
                  key={p.id}
                  className="row-link"
                  onClick={() => navigate(`/pipelines/${p.id}`)}
                >
                  <td className="mono">#{p.id}</td>
                  <td>{p.repo}</td>
                  <td>{p.ref}</td>
                  <td className="mono">
                    {shortSha(p.sha)}{" "}
                    <ConfigChip version={p.config_version ?? null} />
                    {sourceLabel(p.source) && (
                      <span className="source-chip">{sourceLabel(p.source)}</span>
                    )}
                  </td>
                  <td>
                    <StageDots stages={p.stages ?? []} />
                  </td>
                  <td>
                    <StatusBadge status={p.status} />
                  </td>
                  <td className="muted">{relativeTime(p.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
