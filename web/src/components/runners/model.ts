import type { Runner, RunnerLoad, RunnerLoadRow } from "../../api";

/* The runners page's view of the fleet: one state per runner, slots and
   capacity, tag counts and utilisation. Pure functions, unit-tested. */

export type RunnerState = "online" | "busy" | "paused" | "draining" | "offline";

export const STATES: RunnerState[] = ["online", "busy", "paused", "draining", "offline"];

export const STATE_LABEL: Record<RunnerState, string> = {
  online: "Online",
  busy: "Busy",
  paused: "Paused",
  draining: "Draining",
  offline: "Offline",
};

export interface RunnerView {
  id: string;
  runner: Runner;
  load?: RunnerLoadRow;
  state: RunnerState;
  /** job slots (concurrency) */
  slots: number;
  /** jobs running now */
  running: number;
  stale: boolean;
  /** busy time / slot time available in the range; null when it wasn't around */
  utilisation: number | null;
  /** jobs started in the range */
  jobs: number;
}

/** One state per runner. Offline wins, then draining (incl. updating), paused, busy. */
export function runnerState(r: Pick<Runner, "online" | "paused" | "draining" | "running_jobs">): RunnerState {
  if (!r.online) return "offline";
  if (r.draining) return "draining";
  if (r.paused) return "paused";
  if ((r.running_jobs ?? 0) > 0) return "busy";
  return "online";
}

/** Seconds of [from, to] the runner could have worked: from registration to now (or last contact once stale). */
export function availableSeconds(r: Runner, from: number, to: number): number {
  const start = Math.max(from, Date.parse(r.created_at) || from);
  const end = r.stale ? Math.min(to, Date.parse(r.last_contact_at) || to) : to;
  return Math.max(0, (end - start) / 1000);
}

export function buildViews(runners: Runner[], load: RunnerLoad | null): RunnerView[] {
  const byId = new Map<string, RunnerLoadRow>();
  for (const l of load?.runners ?? []) byId.set(String(l.id), l);
  const from = load ? Date.parse(load.from) : NaN;
  const to = load ? Date.parse(load.to) : NaN;
  return runners.map((runner) => {
    const id = String(runner.id);
    const l = byId.get(id);
    const slots = Math.max(1, runner.concurrency ?? l?.concurrency ?? 1);
    const running = l?.running ?? runner.running_jobs ?? 0;
    const avail = Number.isFinite(from) ? availableSeconds(runner, from, to) * slots : 0;
    return {
      id,
      runner,
      load: l,
      state: runnerState({ ...runner, running_jobs: running }),
      slots,
      running,
      stale: !!runner.stale,
      utilisation: l && avail > 0 ? Math.min(1, l.busy_seconds / avail) : null,
      jobs: l?.jobs ?? 0,
    };
  });
}

export interface TagCapacity {
  tag: string;
  /** slots on non-stale runners */
  total: number;
  /** slots that can take a job now: online, not paused or draining */
  available: number;
  online: number;
  runners: number;
}

export interface Capacity {
  total: number;
  available: number;
  /** slots running a job now */
  busy: number;
  /** slots out of service, by why */
  paused: number;
  draining: number;
  offline: number;
  byTag: TagCapacity[];
  /** tags with runners but no available slot */
  starved: string[];
}

const schedulable = (v: RunnerView) => v.state === "online" || v.state === "busy";

/** Capacity over non-stale runners: online × concurrency, minus paused and draining. */
export function capacity(views: RunnerView[]): Capacity {
  const c: Capacity = { total: 0, available: 0, busy: 0, paused: 0, draining: 0, offline: 0, byTag: [], starved: [] };
  const tags = new Map<string, TagCapacity>();
  for (const v of views) {
    if (v.stale) continue;
    c.total += v.slots;
    if (schedulable(v)) {
      c.available += v.slots;
      c.busy += Math.min(v.running, v.slots);
    } else if (v.state === "paused") c.paused += v.slots;
    else if (v.state === "draining") c.draining += v.slots;
    else c.offline += v.slots;
    for (const tag of v.runner.tags ?? []) {
      let t = tags.get(tag);
      if (!t) tags.set(tag, (t = { tag, total: 0, available: 0, online: 0, runners: 0 }));
      t.runners += 1;
      t.total += v.slots;
      if (v.state !== "offline") t.online += 1;
      if (schedulable(v)) t.available += v.slots;
    }
  }
  c.byTag = [...tags.values()].sort((a, b) => a.tag.localeCompare(b.tag));
  c.starved = c.byTag.filter((t) => t.available === 0).map((t) => t.tag);
  return c;
}

/** Runners per state; stale runners count as offline. */
export function stateCounts(views: RunnerView[]): Record<RunnerState, number> {
  const out: Record<RunnerState, number> = { online: 0, busy: 0, paused: 0, draining: 0, offline: 0 };
  for (const v of views) out[v.state] += 1;
  return out;
}

/** Fleet utilisation: busy slot-seconds over the slot-seconds runners were around for. */
export function fleetUtilisation(runners: Runner[], busySeconds: number, from: number, to: number): number | null {
  let avail = 0;
  for (const r of runners) avail += availableSeconds(r, from, to) * Math.max(1, r.concurrency ?? 1);
  return avail > 0 ? Math.min(1, busySeconds / avail) : null;
}

export type RunnerAction = "pause" | "resume" | "drain" | "undrain" | "upgrade" | "remove";

export interface Impact {
  text: string;
  /** danger when it leaves a tag with no slots */
  danger: boolean;
}

/** What an action does to capacity, said before it is confirmed. */
export function actionImpact(v: RunnerView, views: RunnerView[], action: RunnerAction): Impact {
  const cap = capacity(views);
  const slots = `${v.slots} slot${v.slots === 1 ? "" : "s"}`;
  if (action === "remove") {
    return { text: `Removes the registration of ${v.id}. Its jobs and history stay; if it comes back it registers again.`, danger: false };
  }
  if (action === "resume" || action === "undrain") {
    return { text: `Puts ${slots} back into service.`, danger: false };
  }
  const takesOut = schedulable(v);
  const tags = (v.runner.tags ?? []).map((t) => cap.byTag.find((b) => b.tag === t)).filter((t): t is TagCapacity => !!t);
  const parts = takesOut ? tags.map((t) => `${v.slots} of ${t.available} ${t.tag} slots`) : [];
  const emptied = takesOut ? tags.filter((t) => t.available - v.slots <= 0).map((t) => t.tag) : [];
  let text =
    action === "pause"
      ? "Takes no new jobs until resumed; running jobs finish."
      : action === "drain"
        ? "Finishes running jobs, then takes no new ones."
        : "Drains it, self-updates when idle, then resumes.";
  if (takesOut) text += parts.length ? ` This removes ${parts.join(" and ")}.` : ` This removes ${slots} of ${cap.available} available.`;
  if (emptied.length) text += ` Nothing will run ${emptied.map((t) => `“${t}”`).join(", ")} jobs.`;
  return { text, danger: emptied.length > 0 };
}
