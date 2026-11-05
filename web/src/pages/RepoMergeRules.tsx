import { useCallback, useEffect, useState } from "react";
import { useParams } from "react-router-dom";
import { decodeRepoParam, getIsAdmin, listMergePolicies, putMergePolicy, type MergePolicy } from "../api";
import Icon from "../components/Icon";
import RepoHeader from "../components/RepoHeader";
import RuleCard from "../components/review/RuleCard";
import RuleEditor, { RULE_SWITCHES } from "../components/review/RuleEditor";
import { Skeleton } from "../components/ui/Pill";
import { useToast } from "../components/ui/Toast";

/**
 * A repo's merge rules: what stands between its changes and their merge,
 * one card per target branch. Admins flip a rule's switches in place and
 * edit the rest in a drawer; everyone else sees exactly what applies.
 */
export default function RepoMergeRules() {
  const params = useParams<{ repo: string }>();
  const repo = decodeRepoParam(params.repo ?? "");
  const [rules, setRules] = useState<MergePolicy[] | null>(null);
  const [isAdmin, setIsAdmin] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<MergePolicy | "new" | null>(null);
  const toast = useToast();

  const load = useCallback(() => {
    listMergePolicies()
      .then((all) =>
        setRules(
          all
            .filter((p) => p.repo === repo || p.repo === "")
            .sort((a, b) => (a.repo === b.repo ? a.branch_pattern.localeCompare(b.branch_pattern) : a.repo ? -1 : 1)),
        ),
      )
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, [repo]);
  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
    load();
  }, [load]);

  const flip = async (p: MergePolicy, key: keyof MergePolicy, on: boolean) => {
    try {
      await putMergePolicy({ ...p, [key]: on });
      toast(
        `${p.branch_pattern}: ${RULE_SWITCHES.find((s) => s.key === key)?.label.toLowerCase()} ${on ? "on" : "off"}.`,
        "success",
      );
      load();
    } catch (e) {
      toast(e instanceof Error ? e.message : String(e), "danger");
    }
  };

  return (
    <div className="cr-page">
      <RepoHeader
        repo={repo}
        tab="rules"
        actions={
          isAdmin && (
            <button className="btn btn-primary" onClick={() => setEditing("new")}>
              <Icon name="plus" size={14} /> New rule
            </button>
          )
        }
      />
      <p className="page-lede">
        A change's target branch picks its rule: this repo's own rule beats one for every repo, an exact branch beats a
        pattern. A branch no rule matches has no merge gate.
      </p>
      {error && <div className="error-banner">{error}</div>}
      {rules === null && !error && <Skeleton h={120} />}
      {rules !== null && rules.length === 0 && (
        <div className="card cr-empty">
          <Icon name="shield" size={24} />
          <div className="cr-empty-title">No merge rules for {repo}</div>
          <div>
            Its changes merge as the host allows. Add a rule to require approvals, CI, code owners or the merge queue.
          </div>
          {isAdmin && (
            <button className="btn btn-primary" onClick={() => setEditing("new")}>
              <Icon name="plus" size={14} /> New rule
            </button>
          )}
        </div>
      )}
      {rules !== null && rules.length > 0 && (
        <div className="cr-rules">
          {rules.map((p) => (
            <RuleCard
              key={p.id}
              rule={p}
              scope={p.repo ? "this repo" : "every repo"}
              canEdit={isAdmin}
              onEdit={() => setEditing(p)}
              onFlip={(k, v) => void flip(p, k, v)}
            />
          ))}
        </div>
      )}
      {editing && (
        <RuleEditor
          rule={editing === "new" ? null : editing}
          repo={editing === "new" || editing.repo ? repo : undefined}
          onClose={() => setEditing(null)}
          onSaved={(m) => {
            setEditing(null);
            toast(m, "success");
            load();
          }}
        />
      )}
    </div>
  );
}
