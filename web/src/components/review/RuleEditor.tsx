import { useEffect, useState } from "react";
import { deleteMergePolicy, listApproverGroups, putMergePolicy, type ApproverGroup, type MergePolicy } from "../../api";
import Drawer from "../ui/Drawer";

const ROLES = ["admin", "owner", "developer"];

export const BLANK_RULE: MergePolicy = {
  repo: "",
  branch_pattern: "main",
  required_approvals: 1,
  approver_roles: [...ROLES],
  required_group: "",
  required_group_approvals: 0,
  allow_self_approval: false,
  dismiss_on_push: true,
  block_on_changes_requested: true,
  require_ci: true,
  required_jobs: [],
  required_labels: [],
  blocked_labels: [],
  ticket_pattern: "",
  freeze_windows: [],
  timezone: "UTC",
  require_code_owners: false,
  merge_queue: false,
};

const words = (s: string) => s.split(/[\s,]+/).filter(Boolean);

/** "fri 18:00 - mon 09:00", one window per line. */
const parseWindows = (s: string) =>
  s
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => {
      const [start, end] = l.split(/\s*[-–→]\s*/);
      return { start: start ?? "", end: end ?? "" };
    });

/** A labelled on/off switch. */
export function Switch({
  on,
  onChange,
  label,
  disabled,
}: {
  on: boolean;
  onChange: (v: boolean) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      aria-label={label}
      disabled={disabled}
      className={on ? "cr-switch cr-switch-on" : "cr-switch"}
      onClick={() => onChange(!on)}
    />
  );
}

/** The booleans a rule card shows and an admin flips in place. */
export const RULE_SWITCHES: { key: keyof MergePolicy; label: string; hint: string }[] = [
  { key: "require_ci", label: "CI must pass", hint: "The Forge pipeline on the head commit passes" },
  {
    key: "require_code_owners",
    label: "Code owners approve",
    hint: "Owners (CODEOWNERS on the target) of every touched file",
  },
  {
    key: "dismiss_on_push",
    label: "Re-approve after a push",
    hint: "An approval of an earlier revision doesn't count",
  },
  {
    key: "block_on_changes_requested",
    label: "Changes requested block",
    hint: "Until that reviewer approves or withdraws",
  },
  { key: "allow_self_approval", label: "Authors may approve their own", hint: "Usually off" },
  {
    key: "merge_queue",
    label: "Merge through the queue",
    hint: "Each change is tested on top of the latest target first",
  },
];

/**
 * Create or edit one merge rule, in a side drawer: grouped as Reviews,
 * Checks and Merging, with switches for the yes/no rules. repo fixes the
 * repo (a repo's own Merge rules tab).
 */
export default function RuleEditor({
  rule,
  repo,
  onClose,
  onSaved,
}: {
  rule: MergePolicy | null;
  repo?: string;
  onClose: () => void;
  onSaved: (msg: string) => void;
}) {
  const [form, setForm] = useState<MergePolicy>(() => rule ?? { ...BLANK_RULE, repo: repo ?? "" });
  const [text, setText] = useState(() => ({
    jobs: (rule?.required_jobs ?? []).join(", "),
    required: (rule?.required_labels ?? []).join(", "),
    blocked: (rule?.blocked_labels ?? []).join(", "),
    windows: (rule?.freeze_windows ?? []).map((w) => `${w.start} - ${w.end}`).join("\n"),
  }));
  const [groups, setGroups] = useState<ApproverGroup[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => {
    void listApproverGroups()
      .then(setGroups)
      .catch(() => undefined);
  }, []);
  const set = <K extends keyof MergePolicy>(k: K, v: MergePolicy[K]) => setForm((f) => ({ ...f, [k]: v }));

  const save = async () => {
    setBusy(true);
    setErr(null);
    try {
      const saved = await putMergePolicy({
        ...form,
        required_jobs: words(text.jobs),
        required_labels: words(text.required),
        blocked_labels: words(text.blocked),
        freeze_windows: parseWindows(text.windows),
      });
      onSaved(`Saved the rule for ${saved.branch_pattern}. Open changes are checked against it now.`);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    if (!rule?.id || !window.confirm(`Delete the merge rule for ${rule.branch_pattern}?`)) return;
    setBusy(true);
    try {
      await deleteMergePolicy(rule.id);
      onSaved(`Deleted the rule for ${rule.branch_pattern}.`);
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  const row = (k: keyof MergePolicy, label: string, hint: string) => (
    <div className="cr-form-row" key={k}>
      <label>
        {label}
        <small>{hint}</small>
      </label>
      <Switch on={!!form[k]} onChange={(v) => set(k, v as never)} label={label} />
    </div>
  );

  return (
    <Drawer
      title={rule ? `Rule for ${rule.branch_pattern}` : "New merge rule"}
      subtitle={form.repo ? `in ${form.repo}` : "for every repo"}
      onClose={onClose}
    >
      <div className="cr-form-group">
        <h3>Where it applies</h3>
        {!repo && (
          <label className="field">
            <span>Repo (empty = every repo)</span>
            <input
              value={form.repo}
              onChange={(e) => set("repo", e.target.value)}
              placeholder="Letztrip/cos"
              disabled={!!rule}
            />
          </label>
        )}
        <label className="field">
          <span>Target branch (a name or a glob like release/*)</span>
          <input
            value={form.branch_pattern}
            onChange={(e) => set("branch_pattern", e.target.value)}
            disabled={!!rule}
          />
        </label>
      </div>

      <div className="cr-form-group">
        <h3>Reviews</h3>
        <div className="cr-form-row">
          <label>
            Approvals needed
            <small>People with one of the roles below, or in the group</small>
          </label>
          <input
            type="number"
            min={0}
            max={10}
            value={form.required_approvals}
            onChange={(e) => set("required_approvals", Number(e.target.value))}
          />
        </div>
        <div className="cr-form-row">
          <label>
            Who may approve
            <small>Repo roles</small>
          </label>
          <span className="chips">
            {ROLES.map((r) => (
              <button
                key={r}
                type="button"
                className={form.approver_roles.includes(r) ? "fchip fchip-on" : "fchip"}
                aria-pressed={form.approver_roles.includes(r)}
                onClick={() =>
                  set(
                    "approver_roles",
                    form.approver_roles.includes(r)
                      ? form.approver_roles.filter((x) => x !== r)
                      : [...form.approver_roles, r],
                  )
                }
              >
                {r}
              </button>
            ))}
          </span>
        </div>
        <div className="cr-form-row">
          <label>
            At least
            <small>approvals from an approver group</small>
          </label>
          <input
            type="number"
            min={0}
            max={10}
            value={form.required_group_approvals}
            onChange={(e) => set("required_group_approvals", Number(e.target.value))}
          />
          <select
            value={form.required_group}
            onChange={(e) => set("required_group", e.target.value)}
            aria-label="Approver group"
          >
            <option value="">(no group)</option>
            {groups.map((g) => (
              <option key={g.name} value={g.name}>
                {g.name}
              </option>
            ))}
          </select>
        </div>
        {RULE_SWITCHES.filter((s) =>
          ["require_code_owners", "dismiss_on_push", "block_on_changes_requested", "allow_self_approval"].includes(
            s.key,
          ),
        ).map((s) => row(s.key, s.label, s.hint))}
      </div>

      <div className="cr-form-group">
        <h3>Checks</h3>
        {row("require_ci", "CI must pass", "The Forge pipeline on the head commit passes")}
        <label className="field">
          <span>Required jobs (must pass on the head)</span>
          <input
            value={text.jobs}
            onChange={(e) => setText({ ...text, jobs: e.target.value })}
            placeholder="ci-build, ci-test"
          />
        </label>
        <label className="field">
          <span>Required labels</span>
          <input
            value={text.required}
            onChange={(e) => setText({ ...text, required: e.target.value })}
            placeholder="qa-ok"
          />
        </label>
        <label className="field">
          <span>Blocking labels</span>
          <input
            value={text.blocked}
            onChange={(e) => setText({ ...text, blocked: e.target.value })}
            placeholder="do-not-merge"
          />
        </label>
        <label className="field">
          <span>Ticket pattern (in the title, description or branch)</span>
          <input
            value={form.ticket_pattern}
            onChange={(e) => set("ticket_pattern", e.target.value)}
            placeholder="[A-Z]+-[0-9]+"
          />
        </label>
      </div>

      <div className="cr-form-group">
        <h3>Merging</h3>
        {row("merge_queue", "Merge through the queue", "Each change is tested on top of the latest target first")}
        <label className="field">
          <span>Freeze windows (one per line: fri 18:00 - mon 09:00)</span>
          <textarea rows={2} value={text.windows} onChange={(e) => setText({ ...text, windows: e.target.value })} />
        </label>
        <label className="field">
          <span>Timezone</span>
          <input value={form.timezone} onChange={(e) => set("timezone", e.target.value)} placeholder="Asia/Kolkata" />
        </label>
      </div>

      {err && <div className="error-banner">{err}</div>}
      <div className="form-actions">
        <button className="btn btn-primary" disabled={busy || !form.branch_pattern.trim()} onClick={() => void save()}>
          {rule ? "Save rule" : "Create rule"}
        </button>
        <button className="btn" disabled={busy} onClick={onClose}>
          Cancel
        </button>
        {rule?.id && (
          <button
            className="btn btn-danger"
            style={{ marginLeft: "auto" }}
            disabled={busy}
            onClick={() => void remove()}
          >
            Delete
          </button>
        )}
      </div>
    </Drawer>
  );
}
