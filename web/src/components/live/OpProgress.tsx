import { useEffect, useState } from "react";
import { approveLiveOp, cancelLiveOp, getLiveOp, relativeTime, shortSha, type LiveOp } from "../../api";
import { Pill, type Tone } from "../ui/Pill";

const STATE: Record<LiveOp["state"], [Tone, string]> = {
  approval: ["warning", "Waiting for approval"],
  pending: ["accent", "Queued for the agent"],
  running: ["accent", "Running"],
  succeeded: ["success", "Succeeded"],
  failed: ["danger", "Failed"],
  canceled: ["neutral", "Canceled"],
};

const ACTION_TONE: Record<string, Tone> = {
  created: "success", configured: "accent", unchanged: "neutral", pruned: "warning", failed: "danger",
};

/** An operation as it goes: approvals while it waits, then what it did to each resource. */
export default function OpProgress({ op: initial, onDone }: { op: LiveOp; onDone?: (op: LiveOp) => void }) {
  const [op, setOp] = useState(initial);
  const [error, setError] = useState<string | null>(null);
  const finished = op.state === "succeeded" || op.state === "failed" || op.state === "canceled";
  useEffect(() => {
    if (finished) {
      onDone?.(op);
      return;
    }
    const t = window.setTimeout(() => getLiveOp(op.id).then(setOp, () => undefined), 800);
    return () => window.clearTimeout(t);
  }, [op, finished]); // eslint-disable-line react-hooks/exhaustive-deps

  const vote = (v: "approved" | "rejected") =>
    approveLiveOp(op.id, v).then(setOp, (e) => setError(String(e?.message ?? e)));
  const [tone, label] = STATE[op.state];
  const need = op.rule?.required_approvals ?? 1;
  const approved = op.approvals.filter((a) => a.verdict === "approved").length;
  return (
    <div className="lv-op">
      <div className="lv-op-head">
        <Pill tone={tone} icon={finished ? (op.state === "succeeded" ? "ok" : "closed") : "clock"}>{label}</Pill>
        <span className="muted">
          {op.kind}
          {op.params.dry_run && " (dry run)"}
          {op.revision && <> to <span className="mono">{shortSha(op.revision)}</span></>} · {op.requested_by || "someone"},{" "}
          {relativeTime(op.created_at)}
        </span>
        {!finished && (
          <button type="button" className="btn btn-sm" onClick={() => cancelLiveOp(op.id).then(() => getLiveOp(op.id)).then(setOp)}>Cancel</button>
        )}
      </div>
      {op.state === "approval" && (
        <div className="lv-op-approval">
          <span>
            {approved} of {need} approval{need === 1 ? "" : "s"}
            {op.rule?.approver_roles?.length ? ` (${op.rule.approver_roles.join(", ")})` : ""}
          </span>
          <button type="button" className="btn btn-primary btn-sm" onClick={() => void vote("approved")}>Approve</button>
          <button type="button" className="btn btn-sm" onClick={() => void vote("rejected")}>Reject</button>
        </div>
      )}
      {op.approvals.length > 0 && (
        <ul className="lv-op-votes">
          {op.approvals.map((a) => (
            <li key={a.approver}>{a.verdict === "approved" ? "✓" : "✗"} {a.approver} {a.comment && `— ${a.comment}`}</li>
          ))}
        </ul>
      )}
      {(op.result?.message || op.message) && <div className={`lv-op-msg${op.state === "failed" ? " lv-op-msg-bad" : ""}`}>{op.result?.message || op.message}</div>}
      {(op.result?.resources?.length ?? 0) > 0 && (
        <table className="lv-op-table">
          <tbody>
            {op.result!.resources!.map((r) => (
              <tr key={r.key}>
                <td className="muted">{r.kind}</td>
                <td>{r.name}</td>
                <td><Pill tone={ACTION_TONE[r.action.replace("would-", "")] ?? "neutral"}>{r.action.replace("-", " ")}</Pill></td>
                <td className="lv-op-err">{r.message}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {error && <div className="error-banner">{error}</div>}
    </div>
  );
}
