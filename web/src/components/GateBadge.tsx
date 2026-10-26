import type { Change } from "../api";

/** The merge gate in one word: ready, waiting or blocked. */
export default function GateBadge({ change }: { change: Pick<Change, "gate_state" | "gate_summary"> }) {
  if (!change.gate_state) return <span className="muted">—</span>;
  const [cls, label] = { pass: ["success", "ready"], pending: ["running", "waiting"], fail: ["failed", "blocked"] }[
    change.gate_state
  ];
  return (
    <span className={`badge badge-${cls}`} title={change.gate_summary}>
      {label}
    </span>
  );
}
