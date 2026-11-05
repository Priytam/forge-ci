import type { ReactElement } from "react";

/**
 * Reusable SVG helpers for recipe pipeline diagrams.
 *
 * Everything is hand-drawn inline SVG (no external images / assets) and themed
 * to the smoky-glass dark UI. The building block is <PipelineFlow>: you hand it
 * a list of columns, each holding one or more nodes, and it lays them out
 * left-to-right with arrows — including fan-out / fan-in for parallel & matrix
 * stages. Add new diagrams just by composing columns.
 */

export type Tone =
  | "commit"
  | "build"
  | "test"
  | "scan"
  | "deploy"
  | "gate"
  | "neutral";

// Tones distinguish kinds of step in a recipe diagram; they resolve through
// the theme tokens so diagrams read in light and dark.
const TONE: Record<Tone, { stroke: string; fill: string; text: string }> = {
  commit: { stroke: "var(--neutral)", fill: "color-mix(in srgb, var(--neutral) 10%, var(--surface))", text: "var(--text)" },
  build: { stroke: "var(--accent)", fill: "color-mix(in srgb, var(--accent) 10%, var(--surface))", text: "var(--text)" },
  test: { stroke: "var(--warning)", fill: "color-mix(in srgb, var(--warning) 10%, var(--surface))", text: "var(--text)" },
  scan: { stroke: "var(--attention)", fill: "color-mix(in srgb, var(--attention) 10%, var(--surface))", text: "var(--text)" },
  deploy: { stroke: "var(--success)", fill: "color-mix(in srgb, var(--success) 10%, var(--surface))", text: "var(--text)" },
  gate: { stroke: "var(--danger)", fill: "color-mix(in srgb, var(--danger) 10%, var(--surface))", text: "var(--text)" },
  neutral: { stroke: "var(--neutral)", fill: "color-mix(in srgb, var(--neutral) 10%, var(--surface))", text: "var(--text)" },
};

export interface NodeSpec {
  label: string;
  sub?: string;
  tone?: Tone;
}

export interface Column {
  /** small uppercase caption above the column (e.g. a stage name) */
  title?: string;
  nodes: NodeSpec[];
}

const BOX_W = 148;
const BOX_H = 48;
const NODE_GAP = 18;
const COL_GAP = 52;
const PAD_X = 12;
const PAD_TOP = 26;
const PAD_BOTTOM = 14;

function nodeCenterY(count: number, index: number, midY: number): number {
  const total = count * BOX_H + (count - 1) * NODE_GAP;
  const top = midY - total / 2;
  return top + index * (BOX_H + NODE_GAP) + BOX_H / 2;
}

/** A left-to-right pipeline flow, with fan-out / fan-in between columns. */
export function PipelineFlow({
  columns,
  caption,
}: {
  columns: Column[];
  caption?: string;
}): ReactElement {
  const maxNodes = Math.max(...columns.map((c) => c.nodes.length));
  const bodyH = maxNodes * BOX_H + (maxNodes - 1) * NODE_GAP;
  const height = PAD_TOP + bodyH + PAD_BOTTOM;
  const midY = PAD_TOP + bodyH / 2;
  const width =
    PAD_X * 2 + columns.length * BOX_W + (columns.length - 1) * COL_GAP;

  const colX = (i: number) => PAD_X + i * (BOX_W + COL_GAP);

  const arrows: ReactElement[] = [];
  for (let i = 0; i < columns.length - 1; i++) {
    const a = columns[i];
    const b = columns[i + 1];
    const x1 = colX(i) + BOX_W;
    const x2 = colX(i + 1);
    const pairs: Array<[number, number]> = [];
    if (a.nodes.length === b.nodes.length) {
      for (let k = 0; k < a.nodes.length; k++) pairs.push([k, k]);
    } else if (a.nodes.length === 1) {
      for (let k = 0; k < b.nodes.length; k++) pairs.push([0, k]);
    } else if (b.nodes.length === 1) {
      for (let k = 0; k < a.nodes.length; k++) pairs.push([k, 0]);
    } else {
      pairs.push([Math.floor(a.nodes.length / 2), Math.floor(b.nodes.length / 2)]);
    }
    for (const [ai, bi] of pairs) {
      const y1 = nodeCenterY(a.nodes.length, ai, midY);
      const y2 = nodeCenterY(b.nodes.length, bi, midY);
      const mx = (x1 + x2) / 2;
      arrows.push(
        <path
          key={`${i}-${ai}-${bi}`}
          d={`M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2 - 7} ${y2}`}
          fill="none"
          style={{ stroke: "var(--border-strong)" }}
          strokeWidth={1.5}
          markerEnd="url(#rc-arrow)"
        />
      );
    }
  }

  return (
    <div className="recipe-diagram">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        role="img"
        aria-label={caption ?? "Pipeline diagram"}
        preserveAspectRatio="xMidYMid meet"
      >
        <defs>
          <marker
            id="rc-arrow"
            viewBox="0 0 10 10"
            refX="8"
            refY="5"
            markerWidth="7"
            markerHeight="7"
            orient="auto-start-reverse"
          >
            <path d="M 0 0 L 10 5 L 0 10 z" style={{ fill: "var(--text-subtle)" }} />
          </marker>
        </defs>
        {arrows}
        {columns.map((col, i) => (
          <g key={i}>
            {col.title && (
              <text
                x={colX(i) + BOX_W / 2}
                y={12}
                textAnchor="middle"
                fontSize={10}
                fontWeight={700}
                letterSpacing="0.5"
                style={{ fill: "var(--text-muted)", textTransform: "uppercase" }}
              >
                {col.title.toUpperCase()}
              </text>
            )}
            {col.nodes.map((n, k) => {
              const cy = nodeCenterY(col.nodes.length, k, midY);
              const t = TONE[n.tone ?? "neutral"];
              return (
                <g key={k}>
                  <rect
                    x={colX(i)}
                    y={cy - BOX_H / 2}
                    width={BOX_W}
                    height={BOX_H}
                    rx={6}
                    style={{ fill: t.fill, stroke: t.stroke }}
                    strokeWidth={1.25}
                  />
                  <text
                    x={colX(i) + BOX_W / 2}
                    y={n.sub ? cy - 3 : cy + 4}
                    textAnchor="middle"
                    fontSize={12.5}
                    fontWeight={600}
                    style={{ fill: t.text }}
                  >
                    {n.label}
                  </text>
                  {n.sub && (
                    <text
                      x={colX(i) + BOX_W / 2}
                      y={cy + 13}
                      textAnchor="middle"
                      fontSize={9.5}
                      style={{ fill: "var(--text-muted)" }}
                    >
                      {n.sub}
                    </text>
                  )}
                </g>
              );
            })}
          </g>
        ))}
      </svg>
    </div>
  );
}

/** Convenience: a straight single-node-per-stage flow. */
export function LinearFlow({
  steps,
  caption,
}: {
  steps: NodeSpec[];
  caption?: string;
}): ReactElement {
  return (
    <PipelineFlow columns={steps.map((s) => ({ nodes: [s] }))} caption={caption} />
  );
}
