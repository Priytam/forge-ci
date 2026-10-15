import { Link, useParams } from "react-router-dom";
import { decodeRepoParam, listRegistry } from "../api";
import { usePoll } from "../hooks/usePoll";
import PipelineBrowser from "../components/PipelineBrowser";
import { ProviderChip } from "./Repos";

export default function PipelineList() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");

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
            to={`/repos/${encodeURIComponent(repo)}/tests`}
            className="btn"
            title="Test history"
          >
            Tests
          </Link>
          <Link
            to={`/repos/${encodeURIComponent(repo)}/settings`}
            className="btn"
            title="CI/CD Settings"
          >
            ⚙ Settings
          </Link>
        </div>
      </div>

      <PipelineBrowser fixedRepo={repo} />
    </div>
  );
}
