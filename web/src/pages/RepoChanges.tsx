import { useParams } from "react-router-dom";
import { decodeRepoParam } from "../api";
import RepoHeader from "../components/RepoHeader";
import { ChangeList } from "./Changes";

/** A repo's Changes tab: its pull/merge requests (or Forge-native changes). */
export default function RepoChanges() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");
  return (
    <div>
      <RepoHeader repo={repo} tab="changes" />
      <ChangeList fixedRepo={repo} />
    </div>
  );
}
