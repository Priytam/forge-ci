import { relativeTime } from "../../api";
import { Pill, type Tone } from "../ui/Pill";
import type { IconName } from "../Icon";
import { STATE_LABEL, type RunnerState, type RunnerView } from "./model";

const TONE: Record<RunnerState, Tone> = { online: "success", busy: "accent", paused: "warning", draining: "attention", offline: "neutral" };
const ICON: Record<RunnerState, IconName> = { online: "dot", busy: "activity", paused: "pause", draining: "drain", offline: "offline" };

/** One pill per runner state; draining says how many jobs are left, offline how long. */
export default function StatePill({ view }: { view: RunnerView }) {
  const { state, runner, running } = view;
  let label = STATE_LABEL[state];
  let title: string | undefined;
  if (state === "draining") {
    label = runner.update_requested ? "Updating" : "Draining";
    if (running > 0) label += ` (${running} left)`;
    title = runner.update_requested ? "Will self-update when idle, then resume" : "Taking no new jobs";
  } else if (state === "offline") {
    title = `Last seen ${relativeTime(runner.last_contact_at)}${view.stale ? " — stale, not counted in capacity" : ""}`;
    if (view.stale) label = `Offline · ${relativeTime(runner.last_contact_at).replace(" ago", "")}`;
  } else if (state === "paused") {
    title = running > 0 ? `Paused — finishing ${running} job(s)` : "Paused: takes no jobs until resumed";
  } else if (state === "busy") {
    label = `Busy ${running}/${view.slots}`;
  }
  return (
    <Pill tone={TONE[state]} icon={ICON[state]} title={title}>
      {label}
    </Pill>
  );
}
