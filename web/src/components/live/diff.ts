/**
 * Live vs desired, the way people want to read it: the live object cut down
 * to what Git sets (so defaults, status and server-filled fields are not
 * "differences"), both as YAML, compared line by line.
 */

/** live, keeping only what desired sets (status never; metadata's identity, labels, annotations). */
export function project(live: unknown, desired: unknown, top = true): unknown {
  if (Array.isArray(desired)) {
    if (!Array.isArray(live)) return live;
    return live.map((v, i) => (i < desired.length ? project(v, desired[i], false) : v));
  }
  if (desired && typeof desired === "object") {
    if (!live || typeof live !== "object" || Array.isArray(live)) return live;
    const d = desired as Record<string, unknown>;
    const l = live as Record<string, unknown>;
    const out: Record<string, unknown> = {};
    for (const k of Object.keys(d)) {
      if (top && k === "status") continue;
      if (k in l) out[k] = project(l[k], d[k], false);
    }
    return out;
  }
  return live;
}

export interface DiffLine {
  kind: "same" | "del" | "add";
  text: string;
  /** line numbers in the left (live) and right (desired) text */
  a?: number;
  b?: number;
}

/** Line diff by longest common subsequence (manifests are small). */
export function lineDiff(left: string, right: string): DiffLine[] {
  const a = left.split("\n");
  const b = right.split("\n");
  const n = a.length;
  const m = b.length;
  const w = m + 1;
  const lcs = new Int32Array((n + 1) * w);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i * w + j] = a[i] === b[j] ? lcs[(i + 1) * w + j + 1] + 1 : Math.max(lcs[(i + 1) * w + j], lcs[i * w + j + 1]);
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < n || j < m) {
    if (i < n && j < m && a[i] === b[j]) {
      out.push({ kind: "same", text: a[i], a: i + 1, b: j + 1 });
      i++;
      j++;
    } else if (j < m && (i === n || lcs[i * w + j + 1] > lcs[(i + 1) * w + j])) {
      out.push({ kind: "add", text: b[j], b: j + 1 });
      j++;
    } else {
      out.push({ kind: "del", text: a[i], a: i + 1 });
      i++;
    }
  }
  return out;
}

/** Compact: changed lines with `context` lines around them; gaps become null. */
export function compact(lines: DiffLine[], context = 3): (DiffLine | null)[] {
  const keep = new Array(lines.length).fill(false);
  lines.forEach((l, i) => {
    if (l.kind !== "same") {
      for (let k = Math.max(0, i - context); k <= Math.min(lines.length - 1, i + context); k++) keep[k] = true;
    }
  });
  const out: (DiffLine | null)[] = [];
  lines.forEach((l, i) => {
    if (keep[i]) out.push(l);
    else if (out.length === 0 || out[out.length - 1] !== null) out.push(null);
  });
  if (out[out.length - 1] === null) out.pop();
  return out;
}

/** Changed line counts. */
export function stats(lines: DiffLine[]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const l of lines) {
    if (l.kind === "add") added++;
    if (l.kind === "del") removed++;
  }
  return { added, removed };
}
