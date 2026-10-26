import { useEffect, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { listChanges, listMergeQueue, listRegistry } from "../api";
import { usePoll } from "../hooks/usePoll";
import { useChangeStream } from "../hooks/useChangeStream";
import { ProviderChip } from "../pages/Repos";

export type RepoTab = "pipelines" | "changes" | "queue" | "environments" | "tests" | "rules" | "settings";

const TABS: { id: RepoTab; label: string; path: string }[] = [
  { id: "pipelines", label: "Pipelines", path: "" },
  { id: "changes", label: "Changes", path: "/changes" },
  { id: "queue", label: "Merge queue", path: "/queue" },
  { id: "environments", label: "Environments", path: "/environments" },
  { id: "tests", label: "Tests", path: "/tests" },
  { id: "rules", label: "Merge rules", path: "/merge-rules" },
  { id: "settings", label: "Settings", path: "/settings" },
];

/** The tab's path under a repo ("" for its pipelines). */
export function repoTabPath(repo: string, tab: RepoTab): string {
  return `/repos/${encodeURIComponent(repo)}${TABS.find((t) => t.id === tab)?.path ?? ""}`;
}

/**
 * Every repo page starts with this: the repo's name and host, and one tab
 * per thing a repo has — its pipelines, changes, merge queue, environments,
 * tests, merge rules and settings — so everything about a repo is reached
 * from the repo. Counts are read from Forge (never the host).
 */
export default function RepoHeader({ repo, tab, actions }: { repo: string; tab: RepoTab; actions?: ReactNode }) {
  const { data: registry } = usePoll(listRegistry, 0, false);
  const provider = registry?.find((r) => r.repo === repo)?.provider ?? null;
  const [counts, setCounts] = useState<{ changes?: number; queue?: number }>({});
  const [tick, setTick] = useState(0);
  useChangeStream(() => setTick((t) => t + 1));
  useEffect(() => {
    let live = true;
    Promise.all([listChanges({ repo, state: "open", limit: 1 }), listMergeQueue(repo)])
      .then(([c, q]) => live && setCounts({ changes: c.total, queue: q.length }))
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, [repo, tick]);
  const count = (id: RepoTab) => (id === "changes" ? counts.changes : id === "queue" ? counts.queue : undefined);

  return (
    <>
      <div className="page-head repo-head">
        <h1>
          {repo} <ProviderChip provider={provider} />
        </h1>
        {actions && <div className="page-head-actions">{actions}</div>}
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
              {t.label}
              {n !== undefined && n > 0 && <span className="repo-tab-count">{n}</span>}
            </Link>
          );
        })}
      </nav>
    </>
  );
}
