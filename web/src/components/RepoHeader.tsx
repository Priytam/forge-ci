import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { getLiveSummary, listChanges, listMergeQueue, listRegistry } from "../api";
import { usePoll } from "../hooks/usePoll";
import { useChangeStream } from "../hooks/useChangeStream";
import { ProviderChip } from "../pages/Repos";
import Icon, { type IconName } from "./Icon";

export type RepoTab = "pipelines" | "changes" | "queue" | "environments" | "applications" | "tests" | "rules" | "settings";

const TABS: { id: RepoTab; label: string; path: string; icon: IconName }[] = [
  { id: "pipelines", label: "Pipelines", path: "", icon: "pipelines" },
  { id: "changes", label: "Changes", path: "/changes", icon: "changes" },
  { id: "queue", label: "Merge queue", path: "/queue", icon: "layers" },
  { id: "environments", label: "Environments", path: "/environments", icon: "environments" },
  { id: "applications", label: "Applications", path: "/applications", icon: "apps" },
  { id: "tests", label: "Tests", path: "/tests", icon: "activity" },
  { id: "rules", label: "Merge rules", path: "/merge-rules", icon: "shield" },
  { id: "settings", label: "Settings", path: "/settings", icon: "sliders" },
];

/** The tab's path under a repo ("" for its pipelines). */
export function repoTabPath(repo: string, tab: RepoTab): string {
  return `/repos/${encodeURIComponent(repo)}${TABS.find((t) => t.id === tab)?.path ?? ""}`;
}

/**
 * Every repo page starts with this: the repo's name and host, and one tab
 * per thing a repo has — its pipelines, changes, merge queue, environments,
 * the applications it runs as, tests, merge rules and settings — so
 * everything about a repo, from change to running app, is reached from the
 * repo. Counts are read from Forge (never the host or a cluster).
 */
export default function RepoHeader({ repo, tab, actions }: { repo: string; tab: RepoTab; actions?: ReactNode }) {
  const { data: registry } = usePoll(listRegistry, 0, false);
  const provider = registry?.find((r) => r.repo === repo)?.provider ?? null;
  const [counts, setCounts] = useState<{ changes?: number; queue?: number; applications?: number }>({});
  const [tick, setTick] = useState(0);
  useChangeStream(() => setTick((t) => t + 1));
  useEffect(() => {
    let live = true;
    Promise.all([listChanges({ repo, state: "open", limit: 1 }), listMergeQueue(repo), getLiveSummary(repo).catch(() => ({}))])
      .then(([c, q, l]) =>
        live && setCounts({ changes: c.total, queue: q.length, applications: Object.values(l).reduce((n, s) => n + s.apps, 0) })
      )
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [repo, tick]);
  const count = (id: RepoTab) =>
    id === "changes" ? counts.changes : id === "queue" ? counts.queue : id === "applications" ? counts.applications : undefined;

  return (
    <div className="repo-bar">
      <div className="repo-head">
        <span className="repo-head-ic">
          <Icon name="repos" size={16} />
        </span>
        <h1>{repo}</h1>
        <ProviderChip provider={provider} />
        {actions && <div className="page-head-actions repo-head-actions">{actions}</div>}
      </div>
      <nav className="repo-tabs" aria-label="Repository">
        {TABS.map((t) => {
          const n = count(t.id);
          return (
            <Link
              key={t.id}
              to={repoTabPath(repo, t.id)}
              className={t.id === tab ? "repo-tab repo-tab-active" : "repo-tab"}
              aria-current={t.id === tab ? "page" : undefined}
            >
              <Icon name={t.icon} size={14} />
              {t.label}
              {n !== undefined && n > 0 && <span className="repo-tab-count">{n}</span>}
            </Link>
          );
        })}
      </nav>
    </div>
  );
}
