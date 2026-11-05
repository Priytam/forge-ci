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
  "skipped",
  "manual",
]);

/** A run status in plain words. */
export const STATUS_LABEL: Record<string, string> = {
  success: "Passed",
  failed: "Failed",
  running: "Running",
  pending: "Queued",
  blocked: "Waiting",
  created: "Created",
  canceled: "Canceled",
  skipped: "Skipped",
  manual: "Manual",
};

/** A pipeline or job status as a pill: a coloured dot and its word. */
export default function StatusBadge({ status }: StatusBadgeProps) {
  const cls = KNOWN.has(status) ? status : "created";
  return (
    <span className={`badge badge-status badge-${cls}`} title={status}>
      {STATUS_LABEL[status] ?? status}
    </span>
  );
}
