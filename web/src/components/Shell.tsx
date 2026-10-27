import { useEffect, useState, type ReactNode } from "react";
import SessionWatch from "./SessionWatch";
import MaintenanceBanner from "./MaintenanceBanner";
import { Link, useLocation } from "react-router-dom";
import { getIsAdmin } from "../api";
import AuthMenu from "./AuthMenu";
import CommandPalette from "./CommandPalette";
import Icon from "./Icon";
import { crumbsFor, NAV, navActive } from "./nav";
import { useTheme, type ThemePref } from "./theme";

const COLLAPSE_KEY = "forge-sidebar-collapsed";

function readCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSE_KEY) === "1";
  } catch {
    return false;
  }
}

const NEXT_THEME: Record<ThemePref, ThemePref> = { system: "light", light: "dark", dark: "system" };
const THEME_ICON = { system: "monitor", light: "sun", dark: "moon" } as const;

/**
 * App frame: grouped, collapsible left sidebar (admin section only for
 * admins), a top bar with breadcrumbs, ⌘K jump-to, Run pipeline and the user
 * menu. Pages render inside unchanged.
 */
export default function Shell({ children }: { children: ReactNode }) {
  const location = useLocation();
  const [isAdmin, setIsAdmin] = useState(false);
  const [collapsed, setCollapsed] = useState(readCollapsed);
  const [palette, setPalette] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [theme, setTheme] = useTheme();

  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
  }, []);

  useEffect(() => {
    try {
      localStorage.setItem(COLLAPSE_KEY, collapsed ? "1" : "0");
    } catch {
      // per-viewer convenience only
    }
  }, [collapsed]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // The phone menu closes itself once you pick a page.
  useEffect(() => setMenuOpen(false), [location.pathname]);

  const crumbs = crumbsFor(location.pathname, location.search);
  const isMac = typeof navigator !== "undefined" && /Mac/i.test(navigator.platform);

  return (
    <div className={["shell", collapsed && "shell-collapsed", menuOpen && "shell-menu-open"].filter(Boolean).join(" ")}>
      {menuOpen && <div className="sidebar-backdrop" onClick={() => setMenuOpen(false)} aria-hidden="true" />}
      <aside className="sidebar" aria-label="Main navigation">
        <Link to="/" className="sidebar-brand">
          <span className="brand-logo">F</span>
          <span className="sidebar-brand-name">Forge CI</span>
        </Link>
        <nav className="sidebar-nav">
          {NAV.filter((s) => !s.adminOnly || isAdmin).map((s) => (
            <div key={s.title} className="nav-section">
              <div className="nav-section-title">{s.title}</div>
              {s.items.map((it) => (
                <Link
                  key={it.to}
                  to={it.to}
                  title={collapsed ? it.label : undefined}
                  aria-current={navActive(it, location.pathname) ? "page" : undefined}
                  className={navActive(it, location.pathname) ? "nav-item active" : "nav-item"}
                >
                  <Icon name={it.icon} />
                  <span className="nav-label">{it.label}</span>
                </Link>
              ))}
            </div>
          ))}
        </nav>
        <div className="sidebar-foot">
          <button
            type="button"
            className="nav-item nav-button"
            onClick={() => setTheme(NEXT_THEME[theme])}
            title={`Theme: ${theme} (click to change)`}
          >
            <Icon name={THEME_ICON[theme]} />
            <span className="nav-label">Theme: {theme}</span>
          </button>
          <button
            type="button"
            className="nav-item nav-button"
            onClick={() => setCollapsed((c) => !c)}
            title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          >
            <Icon name={collapsed ? "expand" : "collapse"} />
            <span className="nav-label">Collapse</span>
          </button>
        </div>
      </aside>

      <div className="main-col">
        <header className="topbar">
          <button
            type="button"
            className="btn btn-icon menu-trigger"
            onClick={() => setMenuOpen((o) => !o)}
            aria-label="Open menu"
            aria-expanded={menuOpen}
          >
            <Icon name="menu" />
          </button>
          <nav className="topbar-crumbs" aria-label="Breadcrumb">
            {crumbs.map((c, i) => (
              <span key={i} style={{ display: "contents" }}>
                {i > 0 && <span className="crumb-sep">/</span>}
                {c.to && i < crumbs.length - 1 ? (
                  <Link to={c.to}>{c.label}</Link>
                ) : (
                  <span className={i === crumbs.length - 1 ? "crumb-current" : undefined}>{c.label}</span>
                )}
              </span>
            ))}
          </nav>
          <div className="topbar-actions">
            <button type="button" className="search-trigger" onClick={() => setPalette(true)}>
              <Icon name="search" size={14} />
              <span className="search-label">Jump to…</span>
              <kbd>{isMac ? "⌘K" : "Ctrl K"}</kbd>
            </button>
            <Link to="/new" className="btn btn-primary">
              Run pipeline
            </Link>
            <AuthMenu />
          </div>
        </header>
        <MaintenanceBanner />
        <main className="content">{children}</main>
        <SessionWatch />
      </div>
      <CommandPalette open={palette} onClose={() => setPalette(false)} />
    </div>
  );
}
