/**
 * Placeholder rows shown while a page's first data loads, in the shape of the
 * content to come, so the page never flashes an empty "Loading…" line.
 */
export default function Skeleton({ rows = 6, label = "Loading" }: { rows?: number; label?: string }) {
  return (
    <div className="skeleton card" role="status" aria-live="polite" aria-label={label}>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="skeleton-row">
          <span className="skeleton-bar skeleton-bar-sm" />
          <span className="skeleton-bar" style={{ width: `${55 - ((i * 13) % 25)}%` }} />
          <span className="skeleton-bar skeleton-bar-xs" />
        </div>
      ))}
    </div>
  );
}
