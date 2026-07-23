import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { GUIDES, GUIDE_GROUPS } from "../docs/guides";

function DocsIndex() {
  return (
    <>
      <h1>Forge CI Docs</h1>
      <p>
        Step-by-step guides for wiring repos, deploying runners, writing
        pipeline YAML, and locking down deployments. Pick a guide from the
        sidebar, or start here:
      </p>
      {GUIDE_GROUPS.map((group) => (
        <div key={group}>
          <h2>{group}</h2>
          <ul className="docs-index-list">
            {GUIDES.filter((g) => g.group === group).map((g) => (
              <li key={g.slug}>
                <Link to={`/docs/${g.slug}`}>{g.title}</Link>
              </li>
            ))}
          </ul>
        </div>
      ))}
    </>
  );
}

export default function Docs() {
  const { guide: slug } = useParams<{ guide: string }>();
  const [filter, setFilter] = useState("");

  const active = GUIDES.find((g) => g.slug === slug) ?? null;
  const q = filter.trim().toLowerCase();

  return (
    <div className="docs-layout">
      <aside className="docs-sidebar">
        <input
          className="docs-filter"
          placeholder="Filter guides…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
        {GUIDE_GROUPS.map((group) => {
          const items = GUIDES.filter(
            (g) =>
              g.group === group &&
              (q === "" || g.title.toLowerCase().includes(q))
          );
          if (items.length === 0) return null;
          return (
            <div key={group} className="docs-group">
              <div className="docs-group-title">{group}</div>
              {items.map((g) => (
                <Link
                  key={g.slug}
                  to={`/docs/${g.slug}`}
                  className={
                    g.slug === slug ? "docs-link docs-link-active" : "docs-link"
                  }
                >
                  {g.title}
                </Link>
              ))}
            </div>
          );
        })}
      </aside>
      <article className="docs-content">
        {active ? (
          <>
            <div className="docs-crumb muted">
              {active.group} <span className="crumb-sep">/</span>
            </div>
            <h1>{active.title}</h1>
            {active.render()}
          </>
        ) : (
          <DocsIndex />
        )}
      </article>
    </div>
  );
}
