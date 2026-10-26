import { useParams } from "react-router-dom";
import { decodeRepoParam } from "../api";
import PipelineBrowser from "../components/PipelineBrowser";
import RepoHeader from "../components/RepoHeader";

export default function PipelineList() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");

  return (
    <div>
      <RepoHeader repo={repo} tab="pipelines" />

      <PipelineBrowser fixedRepo={repo} />
    </div>
  );
}
