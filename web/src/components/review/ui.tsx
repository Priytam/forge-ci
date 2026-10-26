import type { ReactNode } from "react";
import type { Change, GateResult, GateRule, QueueEntry } from "../../api";
import Icon, { type IconName } from "../Icon";
import { Pill, type Tone } from "../ui/Pill";

/** A change's state as one pill: open, draft, merged or closed. */
export function ChangeStatePill({ change }: { change: Pick<Change, "state" | "draft"> }) {
  if (change.state === "open" && change.draft)
    return (
      <Pill tone="neutral" icon="changes">
        Draft
      </Pill>
    );
  const [tone, icon, label]: [Tone, IconName, string] = {
    open: ["success", "changes", "Open"] as [Tone, IconName, string],
    merged: ["accent", "merge", "Merged"] as [Tone, IconName, string],
    closed: ["danger", "closed", "Closed"] as [Tone, IconName, string],
  }[change.state];
  return (
    <Pill tone={tone} icon={icon}>
      {label}
    </Pill>
  );
}

/** The state icon an inbox row leads with. */
export function ChangeStateIcon({ change }: { change: Pick<Change, "state" | "draft"> }) {
  const [cls, icon]: [string, IconName] =
    change.state === "merged"
      ? ["cr-ic-merged", "merge"]
      : change.state === "closed"
        ? ["cr-ic-closed", "closed"]
        : change.draft
          ? ["cr-ic-draft", "changes"]
          : ["cr-ic-open", "changes"];
  return (
    <span className={`cr-state-ic ${cls}`} title={change.draft && change.state === "open" ? "draft" : change.state}>
      <Icon name={icon} size={16} />
    </span>
  );
}

const GATE: Record<string, [Tone, IconName, string]> = {
  pass: ["success", "ok", "Ready"],
  pending: ["warning", "clock", "Waiting"],
  fail: ["danger", "blocked", "Blocked"],
};

/** The merge gate in one word, with the reason on hover. */
export function GatePill({ state, summary }: { state?: string; summary?: string }) {
  if (!state) return null;
  const [tone, icon, label] = GATE[state] ?? GATE.pending;
  return (
    <Pill tone={tone} icon={icon} title={summary}>
      {label}
    </Pill>
  );
}

/** A rule's pass / waiting / blocked mark. */
export function RuleMark({ state }: { state: GateRule["state"] }) {
  const icon: IconName = state === "pass" ? "check" : state === "fail" ? "close" : "clock";
  return (
    <span className={`cr-mark cr-mark-${state}`}>
      <Icon name={icon} size={12} />
    </span>
  );
}

/** The status bar under a change's title: what is true now, in one line,
 *  and how far the rules have got. */
export function StatusBar({
  tone,
  icon,
  title,
  detail,
  progress,
  children,
}: {
  tone: Tone;
  icon: IconName;
  title: ReactNode;
  detail?: ReactNode;
  progress?: { done: number; total: number };
  children?: ReactNode;
}) {
  return (
    <div className={`cr-status cr-status-${tone}`} role="status">
      <span className="cr-status-ic">
        <Icon name={icon} size={18} />
      </span>
      <div className="cr-status-text">
        <div className="cr-status-title">{title}</div>
        {detail && <div className="cr-status-detail">{detail}</div>}
      </div>
      {progress && progress.total > 0 && (
        <div className="cr-progress" title={`${progress.done} of ${progress.total} merge rules pass`}>
          <span className="cr-progress-label">
            {progress.done}/{progress.total}
          </span>
          <span className="cr-progress-track">
            <span
              className="cr-progress-fill"
              style={{ width: `${Math.round((progress.done / progress.total) * 100)}%` }}
            />
          </span>
        </div>
      )}
      {children}
    </div>
  );
}

/** What the status bar says about a gate. */
export function gateStatus(gate: GateResult | null, target: string, queued: boolean) {
  if (!gate)
    return {
      tone: "neutral" as Tone,
      icon: "shield" as IconName,
      title: `No merge rules for ${target}`,
      detail: "It merges as the host allows.",
    };
  const done = gate.rules.filter((r) => r.state === "pass").length;
  const progress = { done, total: gate.rules.length };
  const others = gate.rules.filter((r) => r.id !== "merge_queue");
  const blocker = gate.rules.find((r) => r.state === "fail");
  const waiting = gate.rules.find((r) => r.state === "pending");
  if (gate.state === "pass")
    return { tone: "success" as Tone, icon: "ok" as IconName, title: "Ready to merge", detail: gate.summary, progress };
  if (blocker)
    return {
      tone: "danger" as Tone,
      icon: "blocked" as IconName,
      title: `Blocked: ${blocker.label}`,
      detail: blocker.detail,
      progress,
    };
  if (waiting?.id === "merge_queue" && others.every((r) => r.state === "pass"))
    return {
      tone: queued ? ("accent" as Tone) : ("success" as Tone),
      icon: queued ? ("layers" as IconName) : ("ok" as IconName),
      title: queued ? "In the merge queue" : "Ready for the merge queue",
      detail: waiting.detail,
      progress,
    };
  return {
    tone: "warning" as Tone,
    icon: "clock" as IconName,
    title: `Waiting: ${waiting?.label ?? "merge rules"}`,
    detail: waiting?.detail,
    progress,
  };
}

const STEPS: { id: QueueEntry["state"]; label: string }[] = [
  { id: "queued", label: "Waiting" },
  { id: "testing", label: "Testing" },
  { id: "merging", label: "Merging" },
  { id: "merged", label: "Merged" },
];

/** Where a merge queue entry is: waiting → testing → merging → merged. */
export function QueueSteps({ entry }: { entry: QueueEntry }) {
  const failed = entry.state === "failed" || entry.state === "removed";
  const at = STEPS.findIndex((s) => s.id === entry.state);
  return (
    <ol className={`cr-steps${failed ? " cr-steps-failed" : ""}`}>
      {STEPS.map((s, i) => {
        const cls = failed ? "" : i < at || entry.state === "merged" ? "cr-step-done" : i === at ? "cr-step-now" : "";
        return (
          <li key={s.id} className={`cr-step ${cls}`}>
            <span className="cr-step-dot" />
            {s.label}
          </li>
        );
      })}
    </ol>
  );
}

const CI: Record<string, [Tone, IconName, string]> = {
  success: ["success", "check", "CI passed"],
  failed: ["danger", "close", "CI failed"],
  canceled: ["neutral", "close", "CI canceled"],
  running: ["accent", "activity", "CI running"],
  pending: ["accent", "clock", "CI queued"],
  created: ["neutral", "clock", "CI queued"],
  blocked: ["warning", "pause", "CI needs approval"],
};

/** The head commit's Forge pipeline in a few words. */
export function CIPill({ status, short }: { status?: string; short?: boolean }) {
  if (!status) return null;
  const [tone, icon, label] = CI[status] ?? CI.created;
  return (
    <Pill tone={tone} icon={icon} title={label}>
      {short ? "CI" : label}
    </Pill>
  );
}
