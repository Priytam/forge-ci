import { useParams } from "react-router-dom";
import { decodeRepoParam } from "../api";
import RepoHeader from "../components/RepoHeader";
import { ApplicationList } from "./Applications";

/** A repo's Applications tab: what it runs as, live (Forge Live). */
export default function RepoApplications() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");
  return (
    <div>
      <RepoHeader repo={repo} tab="applications" />
      <ApplicationList fixedRepo={repo} />
    </div>
  );
}
