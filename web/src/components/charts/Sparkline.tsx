/**
 * A tiny step sparkline for stat tiles: fills its box, breaks at nulls, with
 * an optional threshold line. No axes; the tile carries the number.
 */
export default function Sparkline({
  values,
  color = "var(--accent)",
  threshold,
  max,
  label,
}: {
  values: (number | null)[];
  color?: string;
  threshold?: number;
  /** y top; defaults to the data (and threshold) max */
  max?: number;
  label: string;
}) {
  const W = 100;
  const H = 28;
  const n = values.length;
  const top = max ?? Math.max(1e-9, threshold ?? 0, ...values.map((v) => v ?? 0));
  const y = (v: number) => H - 1 - (Math.min(v, top) / top) * (H - 3);
  let d = "";
  let fill = "";
  let open = -1;
  for (let i = 0; i < n; i++) {
    const v = values[i];
    const xa = (i / n) * W;
    const xb = ((i + 1) / n) * W;
    if (v == null) {
      if (open >= 0) fill += `L${((i / n) * W).toFixed(2)},${H}L${((open / n) * W).toFixed(2)},${H}Z`;
      open = -1;
      continue;
    }
    const seg = `${open < 0 ? "M" : "L"}${xa.toFixed(2)},${y(v).toFixed(2)}L${xb.toFixed(2)},${y(v).toFixed(2)}`;
    d += seg;
    fill += seg;
    if (open < 0) open = i;
  }
  if (open >= 0) fill += `L${W},${H}L${((open / n) * W).toFixed(2)},${H}Z`;
  return (
    <svg className="spark" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img" aria-label={label}>
      <path d={fill} fill={color} opacity={0.12} />
      <path d={d} fill="none" stroke={color} strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
      {threshold != null && (
        <line x1={0} x2={W} y1={y(threshold)} y2={y(threshold)} stroke="var(--warning)" strokeWidth={1} strokeDasharray="2 2" vectorEffect="non-scaling-stroke" />
      )}
    </svg>
  );
}
