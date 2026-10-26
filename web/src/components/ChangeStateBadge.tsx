import type { Change } from "../api";

/** open / draft / merged / closed, in the same badge style as run statuses. */
export default function ChangeStateBadge({ change }: { change: Pick<Change, "state" | "draft"> }) {
  const label = change.state === "open" && change.draft ? "draft" : change.state;
  const cls = { open: "running", draft: "created", merged: "success", closed: "canceled" }[label] ?? "created";
  return <span className={`badge badge-${cls}`}>{label}</span>;
}
