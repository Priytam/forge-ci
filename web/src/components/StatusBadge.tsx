interface StatusBadgeProps {
  status: string;
}

const KNOWN = new Set([
  "success",
  "failed",
  "running",
  "pending",
  "blocked",
  "created",
  "canceled",
]);

export default function StatusBadge({ status }: StatusBadgeProps) {
  const cls = KNOWN.has(status) ? status : "created";
  return (
    <span className={`badge badge-${cls}`}>
      {status}
      {status === "blocked" && <span className="badge-hint">⏸ approval</span>}
    </span>
  );
}
