import { Link, Outlet, useLocation } from "react-router-dom";
import Icon from "./Icon";
import { navActive, SETTINGS_NAV } from "./nav";

/**
 * Every admin page renders inside this: a grouped settings sub-navigation on
 * the left (a scrolling pill row on narrow screens) and the page on the right.
 * The main sidebar then needs only one "Settings" entry, however many
 * settings pages there are.
 */
export default function SettingsLayout() {
  const { pathname } = useLocation();
  return (
    <div className="settings-shell">
      <nav className="settings-nav" aria-label="Settings">
        {SETTINGS_NAV.map((s) => (
          <div key={s.title} className="settings-nav-group">
            <div className="settings-nav-title">{s.title}</div>
            {s.items.map((it) => {
              const active = navActive(it, pathname);
              return (
                <Link
                  key={it.to}
                  to={it.to}
                  className={active ? "settings-nav-item active" : "settings-nav-item"}
                  aria-current={active ? "page" : undefined}
                >
                  <Icon name={it.icon} size={14} />
                  <span>{it.label}</span>
                </Link>
              );
            })}
          </div>
        ))}
      </nav>
      <div className="settings-main">
        <Outlet />
      </div>
    </div>
  );
}
