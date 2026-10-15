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
import ConfigChip from "./ConfigChip";
import Avatar from "./Avatar";

/** The pipeline rows shared by the per-repo list and the all-pipelines page. */
export default function PipelineTable({ pipelines }: { pipelines: Pipeline[] }) {
  const navigate = useNavigate();
  return (
    <div className="card table-card">
      <table>
        <thead>
          <tr>
            <th>ID</th>
            <th>Repo</th>
            <th>Ref</th>
            <th>Commit</th>
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
              <td>
                {/* The subject line identifies a run; a column of shas does
                    not. It leads, with the sha kept for reference. */}
                {commitSubject(p.commit_message) && (
                  <div className="row-subject" title={p.commit_message}>
                    {commitSubject(p.commit_message)}
                  </div>
                )}
                <div className="row-sha">
                  <Avatar identity={p.triggered_by ?? ""} size={18} />
                  <span className="mono">{shortSha(p.sha)}</span>{" "}
                  <ConfigChip version={p.config_version ?? null} />
                  {sourceLabel(p.source) && (
                    <span className="source-chip">{sourceLabel(p.source)}</span>
                  )}
                </div>
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
  );
}
