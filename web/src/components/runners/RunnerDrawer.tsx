import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { getHistoryJobs, getRunnerEvents, getRunnerHistory, relativeTime, type JobRow, type RangeQuery, type RunnerEvent, type RunnerHistoryResponse } from "../../api";
import StatusBadge from "../StatusBadge";
import { duration, percent } from "../charts/scale";
import Drawer from "../ui/Drawer";
import type { MenuItem } from "../ui/Menu";
import { Badge, Meter, Skeleton, TagChips } from "../ui/Pill";
import Icon from "../Icon";
import { describeEvent, toAnnotations } from "./events";
import HistoryPanel from "./HistoryPanel";
import type { RunnerView } from "./model";
import StatePill from "./StatePill";

/** One runner: what it's doing, its charts for the range, recent jobs and what happened to it. */
export default function RunnerDrawer({
  view,
  range,
  rangeText,
  actions,
  onClose,
}: {
  view: RunnerView;
  range: RangeQuery;
  rangeText: string;
  actions: MenuItem[];
  onClose: () => void;
}) {
  const { runner, id } = view;
  const [history, setHistory] = useState<RunnerHistoryResponse | null>(null);
  const [events, setEvents] = useState<RunnerEvent[] | null>(null);
  const [jobs, setJobs] = useState<JobRow[] | null>(null);
  const rangeKey = `${range.range}|${range.from ?? ""}|${range.to ?? ""}`;
  useEffect(() => {
    let off = false;
    setHistory(null);
    setEvents(null);
    setJobs(null);
    void getRunnerHistory(range, { runner: id }).then((h) => {
      if (off) return;
      setHistory(h);
      void getHistoryJobs(h.range.from, h.range.to, { runner: id }, 15).then((j) => !off && setJobs(j)).catch(() => !off && setJobs([]));
    }).catch(() => !off && setJobs([]));
    void getRunnerEvents(range, id).then((e) => !off && setEvents(e)).catch(() => !off && setEvents([]));
    return () => {
      off = true;
    };
  }, [id, rangeKey]);

  const running = view.load?.running_jobs ?? [];
  const now = Date.now();
  return (
    <Drawer
      onClose={onClose}
      wide
      title={<span className="mono">{id}</span>}
      subtitle={
        <>
          <StatePill view={view} />
          <span className="exec-chip">{runner.executor}</span>
          <span className="muted num">
            {view.slots} slot{view.slots === 1 ? "" : "s"}
          </span>
          {runner.version && (
            <Badge mono warn={runner.outdated}>
              {runner.version}
              {runner.outdated && " · outdated"}
            </Badge>
          )}
        </>
      }
    >
      {actions.length > 0 && (
        <div className="drawer-actions">
          {actions.map((a) => (
            <button key={a.label} type="button" className={a.danger ? "btn btn-danger-quiet" : "btn"} disabled={a.disabled} title={a.hint} onClick={a.onSelect}>
              {a.icon && <Icon name={a.icon} size={14} />} {a.label}
            </button>
          ))}
        </div>
      )}

      <dl className="facts">
        <div>
          <dt>Tags</dt>
          <dd>
            <TagChips tags={runner.tags ?? []} max={8} />
          </dd>
        </div>
        <div>
          <dt>Utilisation · {rangeText}</dt>
          <dd>
            <Meter ratio={view.utilisation} label={<span className="num">{percent(view.utilisation)} · {view.jobs} jobs</span>} />
          </dd>
        </div>
        <div>
          <dt>Last contact</dt>
          <dd className="num" title={new Date(runner.last_contact_at).toLocaleString()}>
            {relativeTime(runner.last_contact_at)}
          </dd>
        </div>
        <div>
          <dt>Registered</dt>
          <dd className="num" title={new Date(runner.created_at).toLocaleString()}>
            {relativeTime(runner.created_at)}
          </dd>
        </div>
        {runner.description && (
          <div className="facts-wide">
            <dt>Description</dt>
            <dd>{runner.description}</dd>
          </div>
        )}
      </dl>

      <h3 className="drawer-h">Running now</h3>
      {running.length === 0 ? (
        <p className="muted one-line">{view.state === "offline" ? "Offline." : "Idle."}</p>
      ) : (
        <ul className="plain-list">
          {running.map((j) => (
            <li key={j.job_id}>
              <Link to={`/jobs/${j.job_id}?pipeline=${j.pipeline_id}`}>{j.name}</Link> <span className="mono muted">{j.repo}</span>{" "}
              <span className="muted num">· {duration((now - Date.parse(j.started_at)) / 1000)}</span>
            </li>
          ))}
        </ul>
      )}

      <h3 className="drawer-h">History · {rangeText}</h3>
      <HistoryPanel history={history} filter={{ runner: id }} capacity={view.slots} annotations={toAnnotations(events ?? [])} compactCharts />

      <h3 className="drawer-h">Events</h3>
      {!events && <Skeleton h={40} />}
      {events && events.length === 0 && <p className="muted one-line">No pauses, drains, upgrades or deploys in this range.</p>}
      {events && events.length > 0 && (
        <ol className="timeline">
          {[...events].reverse().map((e, i) => {
            const d = describeEvent(e);
            return (
              <li key={i} className={`tl-${d.tone}`}>
                <span className="tl-time num" title={new Date(e.at).toLocaleString()}>
                  {new Date(e.at).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}
                </span>
                <span>{d.text}</span>
              </li>
            );
          })}
        </ol>
      )}

      <h3 className="drawer-h">Recent jobs</h3>
      {!jobs && <Skeleton h={60} />}
      {jobs && jobs.length === 0 && <p className="muted one-line">No jobs in this range.</p>}
      {jobs && jobs.length > 0 && (
        <table className="dense-table">
          <tbody>
            {jobs.map((j) => (
              <tr key={j.id}>
                <td>
                  <Link to={`/jobs/${j.id}?pipeline=${j.pipeline_id}`}>{j.name}</Link>
                  <div className="mono muted small">{j.repo}</div>
                </td>
                <td>
                  <StatusBadge status={j.status} />
                </td>
                <td className="muted num">{j.started_at ? relativeTime(j.started_at) : "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <p className="one-line">
        <Link to={`/jobs?runner=${encodeURIComponent(id)}`}>All jobs on {id} →</Link>
      </p>
    </Drawer>
  );
}
