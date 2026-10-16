import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { getCoverageTrend, type CoveragePoint } from "../api";

/** Recent pipelines' coverage for a repo as a small line chart. Renders
 *  nothing until some job reports coverage. */
export default function CoverageTrend({ repo }: { repo: string }) {
  const [points, setPoints] = useState<CoveragePoint[] | null>(null);
  useEffect(() => {
    void getCoverageTrend(repo)
      .then(setPoints)
      .catch(() => setPoints([]));
  }, [repo]);
  if (!points || points.length === 0) return null;

  const latest = points[points.length - 1];
  const prev = points.length > 1 ? points[points.length - 2] : null;
  const delta = prev ? latest.coverage - prev.coverage : 0;
  const W = 320;
  const H = 48;
  const min = Math.max(0, Math.min(...points.map((p) => p.coverage)) - 5);
  const max = Math.min(100, Math.max(...points.map((p) => p.coverage)) + 5);
  const x = (i: number) => (points.length === 1 ? W / 2 : (i / (points.length - 1)) * W);
  const y = (v: number) => H - ((v - min) / Math.max(1, max - min)) * H;
  const path = points.map((p, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(p.coverage).toFixed(1)}`).join(" ");

  return (
    <div className="card coverage-trend">
      <div>
        <div className="muted">Coverage</div>
        <div className="coverage-value">
          {latest.coverage.toFixed(1)}%
          {prev && Math.abs(delta) >= 0.05 && (
            <span className={delta > 0 ? "coverage-up" : "coverage-down"}>
              {delta > 0 ? "▲" : "▼"} {Math.abs(delta).toFixed(1)}
            </span>
          )}
        </div>
        <Link className="muted" to={`/pipelines/${latest.pipeline_id}`}>
          #{latest.pipeline_id} · {latest.ref}
        </Link>
      </div>
      <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="coverage-spark" role="img"
        aria-label={`Coverage over the last ${points.length} pipelines`}>
        <path d={path} />
        {points.map((p, i) => (
          <circle key={p.pipeline_id} cx={x(i)} cy={y(p.coverage)} r={2.5}>
            <title>{`#${p.pipeline_id} ${p.ref}: ${p.coverage}%`}</title>
          </circle>
        ))}
      </svg>
    </div>
  );
}
