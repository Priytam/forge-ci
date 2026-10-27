import { Link, useLocation } from "react-router-dom";
import Icon from "./Icon";
import { DOCS_TABS, navActive } from "./nav";

/** One tab strip over the guides, the template catalog and the recipes. */
export default function DocsTabs() {
  const { pathname } = useLocation();
  return (
    <nav className="repo-tabs docs-tabs" aria-label="Docs">
      {DOCS_TABS.map((t) => {
        const active = navActive(t, pathname);
        return (
          <Link
            key={t.to}
            to={t.to}
            className={active ? "repo-tab repo-tab-active" : "repo-tab"}
            aria-current={active ? "page" : undefined}
          >
            <Icon name={t.icon} size={14} />
            {t.label}
          </Link>
        );
      })}
    </nav>
  );
}
