import type { MergePolicy } from "../../api";
import Icon from "../Icon";
import { RULE_SWITCHES, Switch } from "./RuleEditor";

/** One merge rule as a card: the branch it governs, its approvals, and a
 *  switch per yes/no rule (live for admins, read-only for everyone else). */
export default function RuleCard({
  rule: p,
  scope,
  canEdit,
  onEdit,
  onFlip,
}: {
  rule: MergePolicy;
  /** what the badge says about the repo ("this repo", a repo name, "every repo") */
  scope: string;
  canEdit: boolean;
  onEdit: () => void;
  onFlip: (key: keyof MergePolicy, on: boolean) => void;
}) {
  const extras = [
    p.required_jobs.length > 0 && `jobs: ${p.required_jobs.join(", ")}`,
    p.required_labels.length > 0 && `needs label ${p.required_labels.join(", ")}`,
    p.blocked_labels.length > 0 && `blocked by ${p.blocked_labels.join(", ")}`,
    p.ticket_pattern && `ticket ${p.ticket_pattern}`,
    p.freeze_windows.length > 0 && `${p.freeze_windows.length} freeze window(s), ${p.timezone}`,
  ].filter(Boolean);
  return (
    <article className="cr-rule">
      <header className="cr-rule-head">
        <Icon name="shield" size={14} />
        <span className="cr-ref">{p.branch_pattern}</span>
        <span className="badge2" title={scope}>
          {scope}
        </span>
        {canEdit && (
          <button className="btn" onClick={onEdit}>
            Edit
          </button>
        )}
      </header>
      <div className="cr-rule-body">
        <div className="cr-rule-line">
          <span className="cr-mark cr-mark-pass">
            <Icon name="check" size={12} />
          </span>
          <span className="cr-rule-label">
            {p.required_approvals} approval{p.required_approvals === 1 ? "" : "s"}
            {p.required_group &&
              p.required_group_approvals > 0 &&
              ` · ${p.required_group_approvals} from ${p.required_group}`}
            <span className="cr-rule-detail">by {p.approver_roles.join(", ") || "nobody"}</span>
          </span>
        </div>
        {RULE_SWITCHES.map((s) => (
          <div key={s.key} className={p[s.key] ? "cr-rule-line" : "cr-rule-line cr-rule-off"}>
            <Switch on={!!p[s.key]} label={s.label} disabled={!canEdit} onChange={(v) => onFlip(s.key, v)} />
            <span className="cr-rule-label">
              {s.label}
              <span className="cr-rule-detail">{s.hint}</span>
            </span>
          </div>
        ))}
        {extras.length > 0 && (
          <div className="cr-rule-detail" style={{ marginTop: 6 }}>
            {extras.join(" · ")}
          </div>
        )}
      </div>
    </article>
  );
}
