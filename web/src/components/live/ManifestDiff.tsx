import { useEffect, useMemo, useState } from "react";
import { getDesiredManifest, getLiveManifest, resourceKey, type LiveResource } from "../../api";
import { Skeleton } from "../ui/Pill";
import { compact, lineDiff, project, stats, type DiffLine } from "./diff";
import { toYaml } from "./yaml";

export interface DiffOptions {
  split: boolean;
  compact: boolean;
  /** compare only the fields Git sets (defaults and status are not drift) */
  gitFields: boolean;
}

export const DEFAULT_DIFF: DiffOptions = { split: true, compact: true, gitFields: true };

/** The diff's display toggles. */
export function DiffToggles({ opts, onChange }: { opts: DiffOptions; onChange: (o: DiffOptions) => void }) {
  return (
    <div className="lv-diff-toggles">
      <div className="segmented" role="group" aria-label="Layout">
        <button type="button" className={opts.split ? "seg-btn seg-active" : "seg-btn"} onClick={() => onChange({ ...opts, split: true })}>Side by side</button>
        <button type="button" className={!opts.split ? "seg-btn seg-active" : "seg-btn"} onClick={() => onChange({ ...opts, split: false })}>Inline</button>
      </div>
      <label className="check-row">
        <input type="checkbox" checked={opts.compact} onChange={(e) => onChange({ ...opts, compact: e.target.checked })} />
        Changed lines only
      </label>
      <label className="check-row" title="Leave out what the cluster fills in — defaults, status, ids">
        <input type="checkbox" checked={opts.gitFields} onChange={(e) => onChange({ ...opts, gitFields: e.target.checked })} />
        Only fields Git sets
      </label>
    </div>
  );
}

/** Live (left) against desired (right), as YAML. */
export function ManifestDiff({ live, desired, opts }: { live: unknown; desired: unknown; opts: DiffOptions }) {
  const lines = useMemo(() => {
    const left = live == null ? "" : toYaml(opts.gitFields && desired != null ? project(live, desired) : live);
    const right = desired == null ? "" : toYaml(desired);
    return lineDiff(left, right);
  }, [live, desired, opts.gitFields]);
  const { added, removed } = stats(lines);
  const shown = opts.compact ? compact(lines) : lines;
  if (added === 0 && removed === 0) {
    return <div className="lv-diff-same muted">No differences{opts.gitFields ? " in the fields Git sets" : ""}.</div>;
  }
  return (
    <div className="lv-diff">
      <div className="lv-diff-head">
        <span>Live{live == null && " — not in the cluster"}</span>
        <span className="lv-diff-stats">
          <span className="lv-diff-del">−{removed}</span> <span className="lv-diff-add">+{added}</span>
        </span>
        <span>Desired (Git){desired == null && " — not in Git"}</span>
      </div>
      {opts.split ? <Split lines={shown} /> : <Inline lines={shown} />}
    </div>
  );
}

function Gap() {
  return <div className="lv-diff-gap">⋯</div>;
}

function Inline({ lines }: { lines: (DiffLine | null)[] }) {
  return (
    <pre className="lv-diff-body">
      {lines.map((l, i) =>
        l === null ? (
          <Gap key={i} />
        ) : (
          <div key={i} className={`lv-diff-line lv-diff-${l.kind}`}>
            <span className="lv-diff-no">{l.a ?? ""}</span>
            <span className="lv-diff-no">{l.b ?? ""}</span>
            <span className="lv-diff-sign">{l.kind === "add" ? "+" : l.kind === "del" ? "−" : " "}</span>
            {l.text}
          </div>
        )
      )}
    </pre>
  );
}

/** Side by side: a run of deletions sits next to the additions that replace it. */
function Split({ lines }: { lines: (DiffLine | null)[] }) {
  const rows: ([DiffLine | null, DiffLine | null] | null)[] = [];
  for (let i = 0; i < lines.length; ) {
    const l = lines[i];
    if (l === null) {
      rows.push(null);
      i++;
    } else if (l.kind === "same") {
      rows.push([l, l]);
      i++;
    } else {
      const dels: DiffLine[] = [];
      const adds: DiffLine[] = [];
      while (i < lines.length && lines[i]?.kind === "del") dels.push(lines[i++]!);
      while (i < lines.length && lines[i]?.kind === "add") adds.push(lines[i++]!);
      for (let k = 0; k < Math.max(dels.length, adds.length); k++) rows.push([dels[k] ?? null, adds[k] ?? null]);
    }
  }
  const cell = (l: DiffLine | null, side: "a" | "b") => (
    <div className={`lv-diff-line ${l ? `lv-diff-${l.kind}` : "lv-diff-empty"}`}>
      <span className="lv-diff-no">{l?.[side] ?? ""}</span>
      {l?.text ?? ""}
    </div>
  );
  return (
    <div className="lv-diff-split">
      {rows.map((r, i) =>
        r === null ? (
          <Gap key={i} />
        ) : (
          <div key={i} className="lv-diff-row">
            {cell(r[0], "a")}
            {cell(r[1], "b")}
          </div>
        )
      )}
    </div>
  );
}

/** Loads a resource's live and desired manifests and diffs them. */
export function ResourceDiff({ appId, resource, opts }: { appId: number; resource: LiveResource; opts: DiffOptions }) {
  const [pair, setPair] = useState<{ live: unknown; desired: unknown } | null>(null);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    setPair(null);
    const missing = resource.uid.startsWith("desired:");
    Promise.all([missing ? Promise.resolve(null) : getLiveManifest(appId, resource.uid), getDesiredManifest(appId, resourceKey(resource))])
      .then(([live, desired]) => setPair({ live, desired }))
      .catch((e) => setError(String(e?.message ?? e)));
  }, [appId, resource]);
  if (error) return <div className="error-banner">{error}</div>;
  if (!pair) return <Skeleton h={120} />;
  return <ManifestDiff live={pair.live} desired={pair.desired} opts={opts} />;
}
