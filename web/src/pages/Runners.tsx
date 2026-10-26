import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  deleteRunner,
  getHistoryJobs,
  getIsAdmin,
  getRollout,
  getRunnerEvents,
  getRunnerHistory,
  listRepos,
  runnerOp,
  setRunnerPaused,
  startRollout,
  stopRollout,
  type HistorySummary,
  type Rollout,
  type RunnerEvent,
  type RunnerHistoryResponse,
} from "../api";
import Icon from "../components/Icon";
import RangePicker, { previousRange, rangeLabel, rangeSpan, toRangeQuery, useTimeRange } from "../components/RangePicker";
import AddRunnerDrawer from "../components/runners/AddRunnerDrawer";
import { toAnnotations } from "../components/runners/events";
import HealthStrip, { type HealthData } from "../components/runners/HealthStrip";
import HistoryPanel from "../components/runners/HistoryPanel";
import {
  actionImpact,
  buildViews,
  capacity,
  fleetUtilisation,
  stateCounts,
  STATE_LABEL,
  STATES,
  type RunnerAction,
  type RunnerState,
  type RunnerView,
} from "../components/runners/model";
import RunnerDrawer from "../components/runners/RunnerDrawer";
import RunnersTable, { RUNNER_COLUMNS, rowKey, type RowGroup, type SortState } from "../components/runners/RunnersTable";
import { useRunnersLive } from "../components/runners/useRunnersLive";
import Combobox from "../components/ui/Combobox";
import ConfirmDialog from "../components/ui/ConfirmDialog";
import type { MenuItem } from "../components/ui/Menu";
import { Skeleton } from "../components/ui/Pill";
import { ToastProvider, useToast } from "../components/ui/Toast";
import { useElementWidth } from "../hooks/useElementWidth";
import { useStoredState } from "../hooks/useStoredState";
import { duration } from "../components/charts/scale";

type GroupBy = "none" | "tag" | "executor";
type Density = "compact" | "comfortable";

const ACTION_VERB: Record<RunnerAction, { title: string; button: string; done: string; danger?: boolean }> = {
  pause: { title: "Pause", button: "Pause runner", done: "paused" },
  resume: { title: "Resume", button: "Resume runner", done: "resumed" },
  drain: { title: "Drain", button: "Drain runner", done: "draining" },
  undrain: { title: "Stop draining", button: "Stop draining", done: "taking jobs again" },
  upgrade: { title: "Upgrade", button: "Upgrade runner", done: "will upgrade when idle" },
  remove: { title: "Remove", button: "Remove runner", done: "removed", danger: true },
};

/** Table width below which runners render as cards. */
const CARDS_BELOW = 820;

export default function Runners() {
  return (
    <ToastProvider>
      <RunnersPage />
    </ToastProvider>
  );
}

function RunnersPage() {
  const toast = useToast();
  const [params, setParams] = useSearchParams();
  const [range, setRange] = useTimeRange();
  const rq = useMemo(() => toRangeQuery(range), [range.key, range.from, range.to]);
  const rangeText = rangeLabel(range);
  const runnerF = params.get("runner") ?? "";
  const repoF = params.get("repo") ?? "";
  const jobF = params.get("job") ?? "";
  const stateF = (params.get("state") as RunnerState | null) ?? null;
  const setParam = useCallback(
    (k: string, v: string) => {
      const next = new URLSearchParams(params);
      if (v) next.set(k, v);
      else next.delete(k);
      setParams(next, { replace: true });
    },
    [params, setParams],
  );
  const filter = useMemo(() => ({ runner: runnerF || undefined, repo: repoF || undefined, job: jobF || undefined }), [runnerF, repoF, jobF]);

  const { runners, load, error, live, refresh } = useRunnersLive(rq);
  const [isAdmin, setIsAdmin] = useState(false);
  const [rollout, setRollout] = useState<Rollout | null>(null);
  useEffect(() => {
    void getIsAdmin().then((a) => {
      setIsAdmin(a);
      if (a) void getRollout().then(setRollout).catch(() => undefined);
    });
  }, []);
  useEffect(() => {
    if (!rollout?.active) return;
    const t = window.setInterval(() => void getRollout().then(setRollout).catch(() => undefined), 5000);
    return () => window.clearInterval(t);
  }, [rollout?.active]);

  // History, the previous period (for deltas) and events follow range + filters.
  const [history, setHistory] = useState<RunnerHistoryResponse | null>(null);
  const [prev, setPrev] = useState<HistorySummary | null>(null);
  const [events, setEvents] = useState<RunnerEvent[]>([]);
  const [histError, setHistError] = useState<string | null>(null);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let off = false;
    void getRunnerHistory(rq, filter)
      .then((h) => !off && (setHistory(h), setHistError(null)))
      .catch((e) => !off && setHistError(e instanceof Error ? e.message : String(e)));
    void getRunnerHistory(previousRange(range), filter)
      .then((h) => !off && setPrev(h.summary))
      .catch(() => !off && setPrev(null));
    void getRunnerEvents(rq, filter.runner).then((e) => !off && setEvents(e)).catch(() => undefined);
    return () => {
      off = true;
    };
  }, [rq, filter, tick]);
  useEffect(() => {
    setHistory(null);
  }, [rq, filter]);
  // History is per bucket: refresh it each minute while the page is visible.
  useEffect(() => {
    const t = window.setInterval(() => document.visibilityState === "visible" && setTick((n) => n + 1), 60_000);
    return () => window.clearInterval(t);
  }, []);

  // Autocomplete values for the filters.
  const [repos, setRepos] = useState<string[]>([]);
  const [jobNames, setJobNames] = useState<string[]>([]);
  useEffect(() => {
    void listRepos().then((r) => setRepos(r.map((x) => x.repo))).catch(() => undefined);
  }, []);
  useEffect(() => {
    if (!history) return;
    void getHistoryJobs(history.range.from, history.range.to, {}, 500)
      .then((jobs) => {
        setJobNames([...new Set(jobs.map((j) => j.name))]);
        setRepos((r) => [...new Set([...r, ...jobs.map((j) => j.repo)])]);
      })
      .catch(() => undefined);
  }, [history?.range.from]);

  const views = useMemo(() => buildViews(runners ?? [], load), [runners, load]);
  const cap = useMemo(() => capacity(views), [views]);
  const counts = useMemo(() => stateCounts(views), [views]);
  const span = rangeSpan(range);
  const health: HealthData | null = useMemo(() => {
    if (!runners) return null;
    const scoped = filter.runner ? runners.filter((r) => String(r.id) === filter.runner) : runners;
    const pspan = { from: span.from - (span.to - span.from), to: span.from };
    return {
      cap,
      counts,
      load,
      buckets: history?.buckets ?? null,
      summary: history?.summary ?? null,
      prev,
      utilisation: history ? fleetUtilisation(scoped, history.summary.busy_seconds, span.from, span.to) : null,
      prevUtilisation: prev ? fleetUtilisation(scoped, prev.busy_seconds, pspan.from, pspan.to) : null,
    };
  }, [runners, cap, counts, load, history, prev, filter.runner, span.from, span.to]);

  // ---- table: filter, sort, group ----
  const [groupBy, setGroupBy] = useStoredState<GroupBy>("forge.runners.group", "none", ["none", "tag", "executor"]);
  const [density, setDensity] = useStoredState<Density>("forge.runners.density", "compact", ["compact", "comfortable"]);
  const [historyOpen, setHistoryOpen] = useStoredState<"open" | "closed">("forge.runners.history", "open", ["open", "closed"]);
  const [sort, setSort] = useState<SortState>({ key: "state", dir: 1 });
  const groups: RowGroup[] = useMemo(() => {
    const col = RUNNER_COLUMNS.find((c) => c.key === sort.key);
    let rows = views.filter((v) => (!stateF || v.state === stateF) && (!runnerF || v.id === runnerF));
    if (col?.sort) rows = [...rows].sort((a, b) => col.sort!(a, b) * sort.dir || a.id.localeCompare(b.id));
    if (groupBy === "none") return [{ key: "all", views: rows }];
    const by = new Map<string, RunnerView[]>();
    for (const v of rows) {
      const keys = groupBy === "tag" ? (v.runner.tags?.length ? v.runner.tags : ["(no tags)"]) : [v.runner.executor];
      for (const k of keys) by.set(k, [...(by.get(k) ?? []), v]);
    }
    return [...by.entries()]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, vs]) => {
        const t = cap.byTag.find((x) => x.tag === k);
        return {
          key: k,
          views: vs,
          label: (
            <>
              <span className="mono">{k}</span>
              <span className="muted">
                {" "}
                · {vs.length} runner{vs.length === 1 ? "" : "s"}
                {t && (
                  <>
                    {" "}
                    · <span className={t.available === 0 ? "text-danger" : ""}>{t.available}</span> / {t.total} slots available
                  </>
                )}
              </span>
            </>
          ),
        };
      });
  }, [views, stateF, runnerF, sort, groupBy, cap]);
  const flat = useMemo(() => groups.flatMap((g) => g.views.map((v) => ({ key: rowKey(g.key, v.id), view: v }))), [groups]);

  // ---- selection, drawers, keyboard ----
  const [selected, setSelected] = useState<string | null>(null);
  const [openId, setOpenId] = useState<string | null>(null);
  const [adding, setAdding] = useState(false);
  const openView = openId ? views.find((v) => v.id === openId) ?? null : null;
  const [confirm, setConfirm] = useState<{ view: RunnerView; action: RunnerAction } | null>(null);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement;
      if (e.metaKey || e.ctrlKey || e.altKey || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable) return;
      if (openId || adding || confirm || !flat.length) return;
      const at = flat.findIndex((r) => r.key === selected);
      if (e.key === "j" || e.key === "k") {
        e.preventDefault();
        const next = flat[Math.max(0, Math.min(flat.length - 1, at < 0 ? 0 : at + (e.key === "j" ? 1 : -1)))];
        setSelected(next.key);
        document.querySelector<HTMLElement>(`[data-row="${CSS.escape(next.key)}"]`)?.focus();
      } else if (e.key === "Enter" && at >= 0 && t.closest("[data-row]")) {
        e.preventDefault();
        setOpenId(flat[at].view.id);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [flat, selected, openId, adding, confirm]);

  const actionsFor = (v: RunnerView): MenuItem[] => {
    if (!isAdmin) return [];
    const r = v.runner;
    const ask = (action: RunnerAction) => () => setConfirm({ view: v, action });
    const items: MenuItem[] = [];
    if (r.paused) items.push({ label: "Resume", icon: "play", onSelect: ask("resume") });
    else items.push({ label: "Pause", icon: "pause", onSelect: ask("pause"), disabled: v.state === "offline", hint: v.state === "offline" ? "Offline" : undefined });
    if (r.draining) items.push({ label: r.update_requested ? "Cancel upgrade" : "Stop draining", icon: "play", onSelect: ask("undrain") });
    else items.push({ label: "Drain", icon: "drain", onSelect: ask("drain"), disabled: v.state === "offline" });
    if (r.outdated && !r.update_requested)
      items.push({ label: "Upgrade", icon: "upgrade", onSelect: ask("upgrade"), disabled: v.state === "offline", hint: v.state === "offline" ? "Offline: it can't update until it reconnects" : undefined });
    if (v.state === "offline" && v.running === 0) items.push({ label: "Remove", icon: "trash", danger: true, onSelect: ask("remove") });
    return items;
  };

  const run = async () => {
    if (!confirm) return;
    const { view, action } = confirm;
    setBusy(true);
    try {
      if (action === "pause" || action === "resume") await setRunnerPaused(view.id, action === "pause");
      else if (action === "drain") await runnerOp(view.id, "drain");
      else if (action === "undrain") await runnerOp(view.id, "resume");
      else if (action === "upgrade") await runnerOp(view.id, "upgrade");
      else {
        await deleteRunner(view.id);
        if (openId === view.id) setOpenId(null);
      }
      toast(`${view.id} ${ACTION_VERB[action].done}`, "success");
      refresh();
      setTick((n) => n + 1);
    } catch (e) {
      toast(`${ACTION_VERB[action].title} failed: ${e instanceof Error ? e.message : String(e)}`, "danger");
    } finally {
      setBusy(false);
      setConfirm(null);
    }
  };
  const upgradeAll = async () => {
    try {
      setRollout(await startRollout());
      toast("Rolling upgrade started", "success");
    } catch (e) {
      toast(`Upgrade all failed: ${e instanceof Error ? e.message : String(e)}`, "danger");
    }
  };

  // Sticky offsets: the toolbar sits under the top bar, the table header under the toolbar.
  const toolbar = useRef<HTMLDivElement>(null);
  const [toolbarH, setToolbarH] = useState(52);
  useEffect(() => {
    const el = toolbar.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => setToolbarH(el.offsetHeight));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  const [tableBox, tableW] = useElementWidth<HTMLDivElement>();
  const cards = tableW > 0 && tableW < CARDS_BELOW;
  const outdated = (runners ?? []).filter((r) => r.outdated).length;
  const queues = load?.queues ?? [];
  const annotations = useMemo(() => toAnnotations(events), [events]);
  const now = Date.now();

  return (
    <div className={`runners-page density-${density}`} style={{ "--toolbar-h": `${toolbarH}px` } as React.CSSProperties}>
      <div className="runners-toolbar" ref={toolbar}>
        <h1>Runners</h1>
        <span className={live ? "live-dot live-on" : "live-dot"} title={live ? "Live: updates are pushed as they happen" : "Polling every 10 s"}>
          {live ? "Live" : "Polling"}
        </span>
        <div className="toolbar-filters">
          <Combobox label="Runner" allLabel="All runners" placeholder="Runner…" value={runnerF} onChange={(v) => setParam("runner", v)} options={views.map((v) => v.id)} />
          <Combobox label="Repo" allLabel="All repos" placeholder="Repo…" value={repoF} onChange={(v) => setParam("repo", v)} options={repos} />
          <Combobox label="Job" allLabel="All jobs" placeholder="Job name…" value={jobF} onChange={(v) => setParam("job", v)} options={jobNames} />
          <RangePicker
            value={range.key}
            onChange={(k) => setRange({ key: k })}
            custom={range.key === "custom" ? { from: range.from, to: range.to } : undefined}
            onCustom={(from, to) => setRange({ key: "custom", from, to })}
          />
        </div>
        <button type="button" className="btn btn-primary toolbar-add" onClick={() => setAdding(true)}>
          <Icon name="plus" size={14} /> Add runner
        </button>
      </div>

      {error && <div className="error-banner">Failed to load runners: {error}</div>}

      <HealthStrip data={health} onState={(s) => setParam("state", s === "all" ? "" : s)} />

      {queues.length > 0 && (
        <div className="queue-line" role="status">
          <span className="queue-line-label">Waiting</span>
          {queues.map((q) => (
            <span key={q.tags.join(",") || "any"} className={q.eligible_online === 0 || q.oldest_wait_seconds > 120 ? "queue-chip queue-bad" : "queue-chip"}>
              <span className="mono">{q.tags.length ? q.tags.join("+") : "any runner"}</span> <span className="num">{q.pending}</span>
              <span className="muted">
                {" "}
                · oldest <span className="num">{duration(q.oldest_wait_seconds)}</span> ·{" "}
                {q.eligible_online === 0 ? <strong className="text-danger">no runner online</strong> : `${q.eligible_busy}/${q.eligible_online} eligible busy`}
              </span>
            </span>
          ))}
        </div>
      )}

      {rollout?.active && (
        <div className="notice-line">
          <Icon name="upgrade" size={14} /> Rolling upgrade to <span className="mono">{rollout.target}</span>:{" "}
          {rollout.current ? (
            <>
              upgrading <span className="mono">{rollout.current}</span>
            </>
          ) : (
            "finding the next runner"
          )}{" "}
          · {rollout.done.length} done
          {rollout.skipped.length > 0 && `, ${rollout.skipped.length} skipped (${rollout.skipped.join(", ")})`}
          <button type="button" className="btn btn-icon" onClick={() => void stopRollout().then(() => setRollout(null))}>
            Stop
          </button>
        </div>
      )}

      <div className="table-toolbar">
        <div className="state-chips" role="group" aria-label="Filter by state">
          <button type="button" className={!stateF ? "fchip fchip-on" : "fchip"} aria-pressed={!stateF} onClick={() => setParam("state", "")}>
            All <span className="num">{views.length}</span>
          </button>
          {STATES.map((s) => (
            <button
              key={s}
              type="button"
              className={`fchip state-${s}${stateF === s ? " fchip-on" : ""}`}
              aria-pressed={stateF === s}
              disabled={counts[s] === 0 && stateF !== s}
              onClick={() => setParam("state", stateF === s ? "" : s)}
            >
              {STATE_LABEL[s]} <span className="num">{counts[s]}</span>
            </button>
          ))}
        </div>
        <div className="table-toolbar-right">
          {isAdmin && outdated > 0 && !rollout?.active && (
            <button type="button" className="btn btn-icon" onClick={() => void upgradeAll()} title="Drain, update and resume each outdated runner, one at a time">
              <Icon name="upgrade" size={14} /> Upgrade {outdated} outdated
            </button>
          )}
          <label className="inline-select">
            <span className="muted">Group</span>
            <select value={groupBy} onChange={(e) => setGroupBy(e.target.value as GroupBy)} aria-label="Group runners by">
              <option value="none">None</option>
              <option value="tag">Tag</option>
              <option value="executor">Executor</option>
            </select>
          </label>
          <div className="segmented" role="radiogroup" aria-label="Row density">
            {(["compact", "comfortable"] as Density[]).map((d) => (
              <button key={d} type="button" role="radio" aria-checked={density === d} className={density === d ? "seg-btn seg-active" : "seg-btn"} onClick={() => setDensity(d)}>
                {d === "compact" ? "Compact" : "Comfortable"}
              </button>
            ))}
          </div>
        </div>
      </div>

      <div ref={tableBox} className={cards ? "runners-table-box is-cards" : "card runners-table-box"}>
        {!runners && !error && (
          <div className="table-skeleton" aria-busy="true">
            {Array.from({ length: 6 }, (_, i) => (
              <Skeleton key={i} h={28} />
            ))}
          </div>
        )}
        {runners && runners.length === 0 && (
          <p className="one-line muted">
            No runners registered yet.{" "}
            <button type="button" className="link-btn" onClick={() => setAdding(true)}>
              Add one
            </button>
          </p>
        )}
        {runners && runners.length > 0 && flat.length === 0 && (
          <p className="one-line muted">
            No runners match.{" "}
            <button
              type="button"
              className="link-btn"
              onClick={() => {
                const next = new URLSearchParams(params);
                next.delete("state");
                next.delete("runner");
                setParams(next, { replace: true });
              }}
            >
              Clear filters
            </button>
          </p>
        )}
        {flat.length > 0 && (
          <RunnersTable
            groups={groups}
            sort={sort}
            onSort={setSort}
            selected={selected}
            onSelect={setSelected}
            onOpen={(v) => setOpenId(v.id)}
            actions={isAdmin ? actionsFor : undefined}
            cards={cards}
            now={now}
          />
        )}
      </div>
      {flat.length > 0 && !cards && (
        <p className="kbd-hint muted">
          <kbd>j</kbd>/<kbd>k</kbd> move · <kbd>Enter</kbd> opens · click a column to sort
        </p>
      )}

      <section className="history-section">
        <button type="button" className="history-toggle" aria-expanded={historyOpen === "open"} onClick={() => setHistoryOpen(historyOpen === "open" ? "closed" : "open")}>
          <span className={historyOpen === "open" ? "chev chev-open" : "chev"}>
            <Icon name="chevron" size={14} />
          </span>
          <h2>History</h2>
          <span className="muted">
            {rangeText}
            {history && ` · ${history.range.bucket_seconds >= 86400 ? "daily" : history.range.bucket_seconds >= 21600 ? "6-hour" : "hourly"} buckets`}
            {(runnerF || repoF || jobF) && ` · ${[runnerF, repoF, jobF].filter(Boolean).join(" · ")}`}
          </span>
        </button>
        {historyOpen === "open" && (
          <>
            {histError && <div className="error-banner">{histError}</div>}
            <HistoryPanel history={history} filter={filter} capacity={filter.runner ? views.find((v) => v.id === filter.runner)?.slots : cap.available} annotations={annotations} />
          </>
        )}
      </section>

      {openView && (
        <RunnerDrawer
          view={openView}
          range={rq}
          rangeText={rangeText}
          actions={actionsFor(openView)}
          onClose={() => setOpenId(null)}
        />
      )}
      {adding && <AddRunnerDrawer isAdmin={isAdmin} onClose={() => setAdding(false)} />}
      {confirm && (
        <ConfirmDialog
          title={`${ACTION_VERB[confirm.action].title} ${confirm.view.id}?`}
          confirmLabel={ACTION_VERB[confirm.action].button}
          danger={ACTION_VERB[confirm.action].danger || actionImpact(confirm.view, views, confirm.action).danger}
          busy={busy}
          onConfirm={() => void run()}
          onCancel={() => setConfirm(null)}
        >
          <p>{actionImpact(confirm.view, views, confirm.action).text}</p>
        </ConfirmDialog>
      )}
    </div>
  );
}
