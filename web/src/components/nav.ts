import type { IconName } from "./Icon";

export interface NavItem {
  to: string;
  label: string;
  icon: IconName;
  /** extra words the command palette matches on */
  keywords?: string;
  /** other path prefixes that also light this item up (e.g. Docs ← /recipes) */
  match?: string[];
}

export interface NavSection {
  title: string;
  adminOnly?: boolean;
  items: NavItem[];
}

/**
 * The sidebar: the everyday pages, then help and (for admins) one Settings
 * entry. Every admin page lives under Settings with its own sub-navigation
 * (SETTINGS_NAV), so the sidebar stays short as settings grow.
 */
export const NAV: NavSection[] = [
  {
    title: "Work",
    items: [
      { to: "/", label: "Overview", icon: "repos", keywords: "home repos repositories dashboard activity" },
      { to: "/changes", label: "Changes", icon: "changes", keywords: "pull requests merge requests pr mr review inbox" },
      { to: "/pipelines", label: "Pipelines", icon: "pipelines", keywords: "runs builds" },
      { to: "/jobs", label: "Jobs", icon: "jobs", keywords: "running queued failed" },
      { to: "/environments", label: "Environments", icon: "environments", keywords: "deployments dev staging prod board" },
      { to: "/applications", label: "Applications", icon: "apps", keywords: "live kubernetes cloud run health pods tree argo gitops cd" },
      { to: "/runners", label: "Runners", icon: "runners", keywords: "agents executors capacity queue" },
    ],
  },
  {
    title: "Help",
    items: [
      { to: "/docs", label: "Docs", icon: "docs", keywords: "help guide templates recipes examples", match: ["/templates", "/recipes"] },
    ],
  },
  {
    title: "Admin",
    adminOnly: true,
    items: [
      { to: "/admin", label: "Settings", icon: "sliders", keywords: "admin configuration" },
    ],
  },
];

/** Settings sub-navigation, grouped. Each entry is also a command-palette target. */
export const SETTINGS_NAV: NavSection[] = [
  {
    title: "Delivery",
    adminOnly: true,
    items: [
      { to: "/admin/environments", label: "Environments & branches", icon: "environments", keywords: "registry aliases branch rules" },
      { to: "/admin/merge-rules", label: "Merge rules", icon: "changes", keywords: "merge gate pull request approvals branch protection code review freeze" },
      { to: "/admin/approver-groups", label: "Approver groups", icon: "shield", keywords: "release managers approval group" },
      { to: "/admin/notifications", label: "Notifications", icon: "bell", keywords: "slack email approval alert smtp" },
      { to: "/admin/live", label: "Live agents", icon: "apps", keywords: "forge-agent cluster kubernetes cloud run token applications live" },
    ],
  },
  {
    title: "Access",
    adminOnly: true,
    items: [
      { to: "/admin/sso", label: "Sign-in & identity", icon: "lock", keywords: "sso login oidc okta keycloak ldap active directory groups roles session google microsoft github" },
      { to: "/admin/service-accounts", label: "Service accounts", icon: "key", keywords: "robot bot automation token" },
      { to: "/admin/runner-tokens", label: "Runner tokens", icon: "key", keywords: "register runner" },
      { to: "/admin/api-tokens", label: "API tokens", icon: "shield", keywords: "token policy expiry personal access token revoke" },
    ],
  },
  {
    title: "System",
    adminOnly: true,
    items: [
      { to: "/admin/about", label: "About & maintenance", icon: "sliders", keywords: "version upgrade maintenance mode migrations webhook health" },
      { to: "/admin/audit", label: "Audit log", icon: "audit", keywords: "history who" },
    ],
  },
];

/** The first settings page, where /admin lands. */
export const SETTINGS_HOME = "/admin/environments";

/** Docs, template catalog and recipes share one tab strip. */
export const DOCS_TABS: NavItem[] = [
  { to: "/docs", label: "Guides", icon: "docs" },
  { to: "/templates", label: "Template catalog", icon: "recipes", keywords: "include reuse shared versioned inputs" },
  { to: "/recipes", label: "Recipes", icon: "recipes", keywords: "examples templates" },
];

/** Whether a nav item is the current page (its own path or one of its match prefixes). */
export function navActive(item: NavItem, pathname: string): boolean {
  const path = item.to.split("#")[0];
  const under = (p: string) => (p === "/" ? pathname === "/" : pathname === p || pathname.startsWith(p + "/"));
  return under(path) || (item.match ?? []).some(under);
}

/** Pages reachable from the user menu / palette but not the sidebar. */
export const EXTRA_PAGES: NavItem[] = [
  { to: "/new", label: "Run pipeline", icon: "play", keywords: "start trigger new" },
  { to: "/repos/new", label: "Add repository", icon: "plus", keywords: "connect register" },
  { to: "/tokens", label: "API tokens", icon: "key", keywords: "personal access token" },
  { to: "/account", label: "Linked accounts", icon: "key", keywords: "github link oauth account comment as me" },
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
  "api-tokens": "API tokens",
  live: "Live agents",
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
      return [{ label: "Overview" }];
    case "repos": {
      if (b === "new") return [{ label: "Overview", to: "/" }, { label: "Add repository" }];
      const repoTo = `/repos/${encodeURIComponent(b ?? "")}`;
      const sub: Record<string, string> = {
        settings: "Settings",
        environments: "Environments",
        applications: "Applications",
        tests: "Tests",
        changes: "Changes",
        queue: "Merge queue",
        "merge-rules": "Merge rules",
      };
      return c
        ? [{ label: b, to: repoTo }, { label: sub[c] ?? c }]
        : [{ label: b }];
    }
    case "pipelines":
      return b ? [{ label: "Pipelines", to: "/pipelines" }, { label: `#${b}` }] : [{ label: "Pipelines" }];
    case "changes":
      return b ? [{ label: "Changes", to: "/changes" }, { label: `Change ${b}` }] : [{ label: "Changes" }];
    case "applications":
      return b ? [{ label: "Applications", to: "/applications" }, { label: "Application" }] : [{ label: "Applications" }];
    case "jobs": {
      if (!b) return [{ label: "Jobs" }];
      const p = q.get("pipeline");
      return p
        ? [{ label: "Pipelines", to: "/pipelines" }, { label: `#${p}`, to: `/pipelines/${p}` }, { label: `Job ${b}` }]
        : [{ label: "Jobs", to: "/jobs" }, { label: `Job ${b}` }];
    }
    case "admin":
      return b ? [{ label: "Settings", to: "/admin" }, { label: ADMIN_TITLES[b] ?? b }] : [{ label: "Settings" }];
    case "tokens":
      return [{ label: "API tokens" }];
    case "account":
      return [{ label: "Linked accounts" }];
    case "templates":
      return [{ label: "Docs", to: "/docs" }, { label: "Template catalog" }];
    case "new":
      return [{ label: "Run pipeline" }];
    case "docs":
    case "recipes": {
      if (a === "docs") return b ? [{ label: "Docs", to: "/docs" }, { label: b }] : [{ label: "Docs" }];
      return b
        ? [{ label: "Docs", to: "/docs" }, { label: "Recipes", to: "/recipes" }, { label: b }]
        : [{ label: "Docs", to: "/docs" }, { label: "Recipes" }];
    }
    default:
      return [{ label: a.charAt(0).toUpperCase() + a.slice(1) }];
  }
}
