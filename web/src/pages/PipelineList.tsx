import { Link, useNavigate } from "react-router-dom";
import { listPipelines, relativeTime, shortSha } from "../api";
import { usePoll } from "../hooks/usePoll";
import StatusBadge from "../components/StatusBadge";

export default function PipelineList() {
  const navigate = useNavigate();
  const { data: pipelines, error, loading } = usePoll(listPipelines, 2000);

  return (
    <div>
      <div className="page-head">
        <h1>Pipelines</h1>
        <Link to="/new" className="btn btn-primary">
          New Pipeline
        </Link>
      </div>

      {error && <div className="error-banner">Failed to load pipelines: {error}</div>}
      {loading && !pipelines && <div className="muted">Loading pipelines…</div>}

      {pipelines && pipelines.length === 0 && (
        <div className="empty card">
          No pipelines yet. <Link to="/new">Create one</Link> to get started.
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
                  <td className="mono">{shortSha(p.sha)}</td>
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
