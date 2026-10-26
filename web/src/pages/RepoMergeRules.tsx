import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { decodeRepoParam, getIsAdmin, listMergePolicies, type MergePolicy } from "../api";
import RepoHeader from "../components/RepoHeader";
import { describe } from "./AdminMergeRules";

/**
 * A repo's merge rules: what stands between its changes and their merge,
 * per target branch — this repo's own rules first, then those for every
 * repo. Admins edit them in Admin → Merge rules.
 */
export default function RepoMergeRules() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");
  const [rules, setRules] = useState<MergePolicy[] | null>(null);
  const [isAdmin, setIsAdmin] = useState(false);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    listMergePolicies()
      .then((all) =>
        setRules(
          all
            .filter((p) => p.repo === repo || p.repo === "")
            .sort((a, b) => (a.repo === b.repo ? a.branch_pattern.localeCompare(b.branch_pattern) : a.repo ? -1 : 1))
        )
      )
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [repo]);

  return (
    <div>
      <RepoHeader
        repo={repo}
        tab="rules"
        actions={
          isAdmin && (
            <Link className="btn" to="/admin/merge-rules">
              Edit rules
            </Link>
          )
        }
      />
      <p className="page-lede">
        A change's target branch picks its rule: this repo's own rule beats one for every repo, an exact branch beats
        a pattern. A branch no rule matches has no merge gate.
      </p>
      {error && <div className="error-banner">{error}</div>}
      {rules === null && !error && <div className="muted">Loading…</div>}
      {rules !== null && rules.length === 0 && (
        <div className="card empty">No merge rules apply to {repo}: its changes merge as the host allows.</div>
      )}
      {rules !== null && rules.length > 0 && (
        <div className="card table-card">
          <table className="dense-table">
            <thead>
              <tr>
                <th style={{ width: 180 }}>Target branch</th>
                <th>Rules</th>
                <th style={{ width: 120 }}>Applies to</th>
              </tr>
            </thead>
            <tbody>
              {rules.map((p) => (
                <tr key={p.id}>
                  <td className="mono">{p.branch_pattern}</td>
                  <td>{describe(p)}</td>
                  <td className="muted">{p.repo ? "this repo" : "every repo"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
