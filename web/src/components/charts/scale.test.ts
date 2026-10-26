import { describe, expect, it } from "vitest";
import { duration, durationAxis, durationTicks, linearTicks, logDurationTicks, niceStep, timeTicks } from "./scale";

describe("ticks", () => {
  it("nice steps", () => {
    expect([0.3, 1, 1.5, 3, 7, 12].map(niceStep)).toEqual([0.5, 1, 2, 5, 10, 20]);
  });
  it("count ticks are integers from 0 and cover the max", () => {
    const t = linearTicks(6, 4, true);
    expect(t.values[0]).toBe(0);
    expect(t.max).toBeGreaterThanOrEqual(6);
    expect(t.values.every(Number.isInteger)).toBe(true);
  });
  it("fractional slots keep fractional ticks", () => {
    expect(linearTicks(0.75).values).toEqual([0, 0.2, 0.4, 0.6, 0.8]);
  });
  it("duration ticks land on whole minutes", () => {
    expect(durationTicks(250)).toEqual({ max: 360, values: [0, 120, 240, 360] });
    expect(durationTicks(45)).toEqual({ max: 45, values: [0, 15, 30, 45] });
  });
  it("log ticks", () => {
    expect(logDurationTicks(500).values).toEqual([1, 10, 60, 600]);
  });
});

describe("time axis", () => {
  it("24h at 800px gets hour ticks on round hours", () => {
    const from = new Date(2026, 9, 25, 12, 30).getTime();
    const ticks = timeTicks(from, from + 24 * 3600_000, 800);
    expect(ticks.length).toBeGreaterThanOrEqual(6);
    expect(ticks.length).toBeLessThanOrEqual(12);
    for (const t of ticks) expect(new Date(t.at).getMinutes()).toBe(0);
    expect(ticks.some((t) => /Oct/.test(t.label))).toBe(true); // midnight shows the date
  });
  it("30d at 600px gets day-aligned ticks, few enough to read", () => {
    const from = new Date(2026, 8, 26).getTime();
    const ticks = timeTicks(from, from + 30 * 86400_000, 600);
    expect(ticks.length).toBeLessThanOrEqual(9);
    for (const t of ticks) expect(new Date(t.at).getHours()).toBe(0);
  });
  it("2-day steps skip days", () => {
    const from = new Date(2026, 9, 1).getTime();
    const ticks = timeTicks(from, from + 14 * 86400_000, 600);
    const gaps = ticks.slice(1).map((t, i) => Math.round((t.at - ticks[i].at) / 86400_000));
    expect(new Set(gaps).size).toBe(1);
    expect(gaps[0]).toBeGreaterThan(1);
  });
});

describe("formatters", () => {
  it("durations", () => {
    expect(duration(null)).toBe("—");
    expect(duration(42)).toBe("42s");
    expect(duration(190)).toBe("3m 10s");
    expect(duration(3900)).toBe("1h 05m");
    expect(durationAxis(90)).toBe("1.5m");
    expect(durationAxis(0)).toBe("0");
  });
});
