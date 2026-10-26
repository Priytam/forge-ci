import type { IconName } from "./Icon";

export interface NavItem {
  to: string;
  label: string;
  icon: IconName;
  /** extra words the command palette matches on */
  keywords?: string;
}

export interface NavSection {
  title: string;
  adminOnly?: boolean;
  items: NavItem[];
}

/**
 * The app's navigation, grouped. The sidebar renders it and the command
 * palette searches it, so a new page is added in one place.
 */
export const NAV: NavSection[] = [
  {
    title: "Work",
    items: [
      { to: "/", label: "Repos", icon: "repos", keywords: "home repositories" },
      { to: "/changes", label: "Changes", icon: "changes", keywords: "pull requests merge requests pr mr review inbox" },
      { to: "/pipelines", label: "Pipelines", icon: "pipelines", keywords: "runs builds" },
      { to: "/jobs", label: "Jobs", icon: "jobs", keywords: "running queued failed" },
      { to: "/environments", label: "Environments", icon: "environments", keywords: "deployments dev staging prod board" },
    ],
  },
  {
    title: "Infrastructure",
    items: [{ to: "/runners", label: "Runners", icon: "runners", keywords: "agents executors" }],
  },
  {
    title: "Learn",
    items: [
      { to: "/templates", label: "Template catalog", icon: "recipes", keywords: "include reuse shared versioned inputs" },
      { to: "/recipes", label: "Recipes", icon: "recipes", keywords: "examples templates" },
      { to: "/docs", label: "Docs", icon: "docs", keywords: "help guide" },
    ],
  },
  {
    title: "Admin",
    adminOnly: true,
    items: [
      { to: "/admin/environments", label: "Environments & branches", icon: "sliders", keywords: "registry aliases branch rules" },
      { to: "/admin/notifications", label: "Notifications", icon: "bell", keywords: "slack email approval alert" },
      { to: "/admin/approver-groups", label: "Approver groups", icon: "shield", keywords: "release managers approval group" },
      { to: "/admin/merge-rules", label: "Merge rules", icon: "changes", keywords: "merge gate pull request approvals branch protection code review freeze" },
      { to: "/admin/service-accounts", label: "Service accounts", icon: "key", keywords: "robot bot automation token" },
      { to: "/admin/runner-tokens", label: "Runner tokens", icon: "key", keywords: "register runner" },
      { to: "/admin/sso", label: "Sign-in & identity", icon: "lock", keywords: "sso login oidc okta keycloak ldap active directory groups roles session google microsoft github" },
      { to: "/admin/audit", label: "Audit log", icon: "audit", keywords: "history who" },
      { to: "/admin/about", label: "About & maintenance", icon: "sliders", keywords: "version upgrade maintenance mode migrations" },
      { to: "/tokens#policy", label: "API token policy", icon: "shield", keywords: "expiry" },
    ],
  },
];

/** Pages reachable from the user menu / palette but not the sidebar. */
export const EXTRA_PAGES: NavItem[] = [
  { to: "/new", label: "Run pipeline", icon: "play", keywords: "start trigger new" },
  { to: "/repos/new", label: "Add repository", icon: "plus", keywords: "connect register" },
  { to: "/tokens", label: "API tokens", icon: "key", keywords: "personal access token" },
];

const ADMIN_TITLES: Record<string, string> = {
  sso: "Sign-in & identity",
  audit: "Audit log",
  "runner-tokens": "Runner tokens",
  environments: "Environments & branches",
  notifications: "Notifications",
  "approver-groups": "Approver groups",
  "merge-rules": "Merge rules",
  "service-accounts": "Service accounts",
  about: "About & maintenance",
};

export interface Crumb {
  label: string;
  to?: string;
}

/** Breadcrumbs for the top bar, derived from the URL alone. */
export function crumbsFor(pathname: string, search: string): Crumb[] {
  const parts = pathname.split("/").filter(Boolean).map(decodeURIComponent);
  const q = new URLSearchParams(search);
  const [a, b, c] = parts;
  switch (a) {
    case undefined:
      return [{ label: "Repos" }];
    case "repos": {
      if (b === "new") return [{ label: "Repos", to: "/" }, { label: "Add repository" }];
      const repoTo = `/repos/${encodeURIComponent(b ?? "")}`;
      const sub: Record<string, string> = { settings: "Settings", environments: "Environments", tests: "Tests" };
      return c
        ? [{ label: "Repos", to: "/" }, { label: b, to: repoTo }, { label: sub[c] ?? c }]
        : [{ label: "Repos", to: "/" }, { label: b }];
    }
    case "pipelines":
      return b ? [{ label: "Pipelines", to: "/pipelines" }, { label: `#${b}` }] : [{ label: "Pipelines" }];
    case "changes":
      return b ? [{ label: "Changes", to: "/changes" }, { label: `Change ${b}` }] : [{ label: "Changes" }];
    case "jobs": {
      if (!b) return [{ label: "Jobs" }];
      const p = q.get("pipeline");
      return p
        ? [{ label: "Pipelines", to: "/pipelines" }, { label: `#${p}`, to: `/pipelines/${p}` }, { label: `Job ${b}` }]
        : [{ label: "Jobs", to: "/jobs" }, { label: `Job ${b}` }];
    }
    case "admin":
      return [{ label: "Admin" }, { label: ADMIN_TITLES[b ?? ""] ?? b ?? "" }];
    case "tokens":
      return [{ label: "API tokens" }];
    case "templates":
      return [{ label: "Template catalog" }];
    case "new":
      return [{ label: "Run pipeline" }];
    case "docs":
    case "recipes": {
      const title = a === "docs" ? "Docs" : "Recipes";
      return b ? [{ label: title, to: `/${a}` }, { label: b }] : [{ label: title }];
    }
    default:
      return [{ label: a.charAt(0).toUpperCase() + a.slice(1) }];
  }
}
