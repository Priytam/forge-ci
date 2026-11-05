import { describe, expect, it } from "vitest";
import { edgePath } from "./PipelineDetail";

// Columns: build at x 0–200, checks at x 300–500 (rows of 32px with 8px gaps), deploy at x 600.
const checks = [0, 1, 2, 3, 4].map((i) => ({ left: 300, right: 500, top: 40 + i * 40, bottom: 72 + i * 40 }));

describe("pipeline graph edges", () => {
  it("draws a plain curve between neighbouring columns", () => {
    const d = edgePath(500, 56, 600, 56, checks);
    expect(d).not.toContain(" L ");
  });

  it("routes an edge that skips a column through a free gap, not through a card", () => {
    // build (row 1) -> deploy (row 1) passes straight through the first check.
    const d = edgePath(200, 56, 600, 56, checks);
    expect(d).toContain(" L ");
    const lane = Number(d.split(" L ")[1].split(" ")[1]);
    for (const b of checks) {
      expect(lane > b.top - 3 && lane < b.bottom + 3).toBe(false);
    }
  });
});
