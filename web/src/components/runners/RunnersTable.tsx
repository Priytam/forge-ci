import type { ReactNode } from "react";
import { Link } from "react-router-dom";
import { relativeTime } from "../../api";
import { compact, duration, percent } from "../charts/scale";
import Menu, { type MenuItem } from "../ui/Menu";
import { Badge, Meter, TagChips } from "../ui/Pill";
import { STATES, type RunnerView } from "./model";
import StatePill from "./StatePill";

/*
 * The runners table. Columns are declared once — header and cell together —
 * so their order can't drift apart (the old table had Status over the
 * version and Version over the status).
 */

export interface Column {
  key: string;
  label: string;
  /** sort comparator, ascending */
  sort?: (a: RunnerView, b: RunnerView) => number;
  className?: string;
  cell: (v: RunnerView, now: number) => ReactNode;
}

const stateRank = (v: RunnerView) => STATES.indexOf(v.state);

function NowCell({ v, now }: { v: RunnerView; now: number }) {
  const jobs = v.load?.running_jobs ?? [];
  if (!jobs.length) return <span className="muted">{v.state === "offline" ? "—" : "idle"}</span>;
  const j = jobs[0];
  return (
    <span className="now-cell">
      <Link to={`/jobs/${j.job_id}?pipeline=${j.pipeline_id}`} onClick={(e) => e.stopPropagation()} className="now-job">
        {j.name}
      </Link>
      <span className="muted now-meta">
        <span className="mono">{j.repo}</span> · <span className="num">{duration((now - Date.parse(j.started_at)) / 1000)}</span>
        {jobs.length > 1 && ` · +${jobs.length - 1}`}
      </span>
    </span>
  );
}

export const RUNNER_COLUMNS: Column[] = [
  {
    key: "runner",
    label: "Runner",
    sort: (a, b) => a.id.localeCompare(b.id),
    className: "col-runner",
    cell: (v) => (
      <span className="runner-cell" title={v.runner.description || undefined}>
        <span className="mono runner-id">{v.id}</span>
        <span className="runner-sub">
          <span className="exec-chip">{v.runner.executor}</span>
          {v.slots > 1 && <span className="muted num">×{v.slots}</span>}
          {v.runner.description && <span className="muted runner-desc">{v.runner.description}</span>}
        </span>
      </span>
    ),
  },
  { key: "state", label: "State", sort: (a, b) => stateRank(a) - stateRank(b), className: "col-state", cell: (v) => <StatePill view={v} /> },
  { key: "tags", label: "Tags", sort: (a, b) => (a.runner.tags ?? []).join().localeCompare((b.runner.tags ?? []).join()), className: "col-tags", cell: (v) => <TagChips tags={v.runner.tags ?? []} /> },
  { key: "now", label: "Now", sort: (a, b) => a.running - b.running, className: "col-now", cell: (v, now) => <NowCell v={v} now={now} /> },
  {
    key: "utilisation",
    label: "Utilisation",
    sort: (a, b) => (a.utilisation ?? -1) - (b.utilisation ?? -1),
    className: "col-util",
    cell: (v) => (
      <Meter
        ratio={v.utilisation}
        label={
          <>
            <span className="num">{percent(v.utilisation)}</span>
            <span className="muted num"> · {compact(v.jobs, 0)} jobs</span>
          </>
        }
        title={`Busy ${percent(v.utilisation)} of its ${v.slots} slot(s) over the range; ${v.jobs} jobs started`}
      />
    ),
  },
  {
    key: "version",
    label: "Version",
    sort: (a, b) => (a.runner.version ?? "").localeCompare(b.runner.version ?? ""),
    className: "col-version",
    cell: (v) =>
      v.runner.version ? (
        <Badge mono warn={v.runner.outdated} title={v.runner.outdated ? "Differs from the runner this server bundles" : "Current"}>
          {v.runner.version}
          {v.runner.outdated && " · outdated"}
        </Badge>
      ) : (
        <span className="muted">—</span>
      ),
  },
  {
    key: "contact",
    label: "Last contact",
    sort: (a, b) => Date.parse(a.runner.last_contact_at) - Date.parse(b.runner.last_contact_at),
    className: "col-contact",
    cell: (v) => (
      <span className="muted num" title={new Date(v.runner.last_contact_at).toLocaleString()}>
        {relativeTime(v.runner.last_contact_at)}
      </span>
    ),
  },
];

export interface RowGroup {
  key: string;
  label?: ReactNode;
  views: RunnerView[];
}

export interface SortState {
  key: string;
  dir: 1 | -1;
}

interface Props {
  groups: RowGroup[];
  sort: SortState;
  onSort: (s: SortState) => void;
  /** row key (group:id) of the keyboard selection */
  selected: string | null;
  onOpen: (v: RunnerView) => void;
  onSelect: (key: string) => void;
  actions?: (v: RunnerView) => MenuItem[];
  cards?: boolean;
  now: number;
}

export const rowKey = (group: string, id: string) => `${group}:${id}`;

const rowClass = (v: RunnerView, selected: boolean) =>
  ["runner-row", v.state === "paused" || v.state === "draining" ? "row-paused" : "", v.state === "offline" ? "row-offline" : "", selected ? "row-sel" : ""]
    .filter(Boolean)
    .join(" ");

export default function RunnersTable({ groups, sort, onSort, selected, onOpen, onSelect, actions, cards, now }: Props) {
  if (cards) {
    return (
      <div className="runner-cards">
        {groups.map((g) => (
          <section key={g.key} className="runner-card-group">
            {g.label && <div className="group-head">{g.label}</div>}
            {g.views.map((v) => {
              const key = rowKey(g.key, v.id);
              return (
                <article
                  key={key}
                  data-row={key}
                  className={`card runner-card ${rowClass(v, key === selected)}`}
                  tabIndex={key === selected ? 0 : -1}
                  onClick={() => {
                    onSelect(key);
                    onOpen(v);
                  }}
                >
                  <div className="runner-card-head">
                    {RUNNER_COLUMNS[0].cell(v, now)}
                    <StatePill view={v} />
                    {actions && <Menu label={`Actions for ${v.id}`} items={actions(v)} />}
                  </div>
                  <dl className="runner-card-grid">
                    {RUNNER_COLUMNS.slice(2).map((c) => (
                      <div key={c.key} data-col={c.key}>
                        <dt>{c.label}</dt>
                        <dd>{c.cell(v, now)}</dd>
                      </div>
                    ))}
                  </dl>
                </article>
              );
            })}
          </section>
        ))}
      </div>
    );
  }
  const cols = RUNNER_COLUMNS;
  const span = cols.length + (actions ? 1 : 0);
  return (
    <table className="runners-table">
      <thead>
        <tr>
          {cols.map((c) => (
            <th key={c.key} className={c.className} aria-sort={sort.key === c.key ? (sort.dir === 1 ? "ascending" : "descending") : undefined}>
              {c.sort ? (
                <button type="button" className="th-sort" onClick={() => onSort({ key: c.key, dir: sort.key === c.key ? (-sort.dir as 1 | -1) : 1 })}>
                  {c.label}
                  <span className="th-arrow" aria-hidden="true">
                    {sort.key === c.key ? (sort.dir === 1 ? "↑" : "↓") : ""}
                  </span>
                </button>
              ) : (
                c.label
              )}
            </th>
          ))}
          {actions && (
            <th className="col-actions">
              <span className="sr-only">Actions</span>
            </th>
          )}
        </tr>
      </thead>
      {groups.map((g) => (
        <tbody key={g.key}>
          {g.label && (
            <tr className="group-row">
              <td colSpan={span}>{g.label}</td>
            </tr>
          )}
          {g.views.map((v) => {
            const key = rowKey(g.key, v.id);
            return (
              <tr
                key={key}
                data-row={key}
                className={rowClass(v, key === selected)}
                tabIndex={key === selected ? 0 : -1}
                aria-selected={key === selected}
                onClick={() => {
                  onSelect(key);
                  onOpen(v);
                }}
              >
                {cols.map((c) => (
                  <td key={c.key} data-col={c.key} className={c.className}>
                    {c.cell(v, now)}
                  </td>
                ))}
                {actions && (
                  <td className="col-actions" data-col="actions">
                    <Menu label={`Actions for ${v.id}`} items={actions(v)} />
                  </td>
                )}
              </tr>
            );
          })}
        </tbody>
      ))}
    </table>
  );
}
