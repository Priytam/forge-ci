import { useNavigate } from "react-router-dom";
import {
  commitSubject,
  relativeTime,
  shortSha,
  sourceLabel,
  type Pipeline,
} from "../api";
import StatusBadge from "./StatusBadge";
import StageDots from "./StageDots";
import Avatar from "./Avatar";

/**
 * Pipeline rows shared by the per-repo list and the all-pipelines page: one
 * line per run, status first, identifiers (branch, sha) in monospace.
 */
export default function PipelineTable({
  pipelines,
  showRepo = true,
}: {
  pipelines: Pipeline[];
  showRepo?: boolean;
}) {
  const navigate = useNavigate();
  return (
    <div className="card table-card">
      <table className="dense-table">
        <thead>
          <tr>
            <th style={{ width: 96 }}>Status</th>
            <th>Pipeline</th>
            {showRepo && <th>Repo</th>}
            <th>Branch</th>
            <th>Commit</th>
            <th>Stages</th>
            <th style={{ textAlign: "right" }}>Created</th>
          </tr>
        </thead>
        <tbody>
          {pipelines.map((p) => {
            const subject = commitSubject(p.commit_message);
            const source = sourceLabel(p.source);
            return (
              <tr key={p.id} className="row-link" onClick={() => navigate(`/pipelines/${p.id}`)}>
                <td>
                  <StatusBadge status={p.status} />
                </td>
                <td className="cell-title">
                  <span className="mono muted">#{p.id}</span>{" "}
                  <span title={p.commit_message}>{subject || <span className="muted">(no message)</span>}</span>
                  {source && <span className="cell-meta"> · {source}</span>}
                  {p.config_version != null && <span className="cell-meta"> · config v{p.config_version}</span>}
                </td>
                {showRepo && <td className="cell-repo">{p.repo}</td>}
                <td>
                  <span className="mono cell-branch" title={p.ref}>
                    {p.ref}
                  </span>
                </td>
                <td>
                  <span className="cell-commit">
                    <Avatar identity={p.triggered_by ?? ""} size={16} />
                    <span className="mono">{shortSha(p.sha)}</span>
                  </span>
                </td>
                <td>
                  <StageDots stages={p.stages ?? []} />
                </td>
                <td className="muted" style={{ textAlign: "right" }} title={new Date(p.created_at).toLocaleString()}>
                  {relativeTime(p.created_at)}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
