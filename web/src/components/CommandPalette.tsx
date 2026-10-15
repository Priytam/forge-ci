import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { getIsAdmin, listReposPage } from "../api";
import Icon, { type IconName } from "./Icon";
import { EXTRA_PAGES, NAV } from "./nav";

interface Entry {
  group: string;
  label: string;
  hint?: string;
  icon: IconName;
  to: string;
}

/**
 * ⌘K / Ctrl+K jump-to: pages, repos (searched on the server, so it scales to
 * any number of repos) and "#123" for a pipeline by number.
 */
export default function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const [q, setQ] = useState("");
  const [sel, setSel] = useState(0);
  const [repos, setRepos] = useState<string[]>([]);
  const [searching, setSearching] = useState(false);
  const [isAdmin, setIsAdmin] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
  }, []);

  useEffect(() => {
    if (!open) return;
    setQ("");
    setSel(0);
    window.setTimeout(() => inputRef.current?.focus(), 0);
  }, [open]);

  // Repo search runs on the server, debounced.
  useEffect(() => {
    if (!open) return;
    setSearching(true);
    const t = window.setTimeout(() => {
      void listReposPage(q.trim(), "activity", 0, 8)
        .then((p) => setRepos(p.items.map((r) => r.repo)))
        .catch(() => setRepos([]))
        .finally(() => setSearching(false));
    }, 150);
    return () => window.clearTimeout(t);
  }, [q, open]);

  const entries = useMemo(() => {
    const term = q.trim().toLowerCase();
    const match = (s: string) => !term || s.toLowerCase().includes(term);
    const out: Entry[] = [];
    const num = term.replace(/^#/, "");
    if (/^\d+$/.test(num)) {
      out.push({ group: "Go to", label: `Pipeline #${num}`, icon: "pipelines", to: `/pipelines/${num}` });
    }
    for (const r of repos) {
      out.push({ group: "Repos", label: r, icon: "repos", to: `/repos/${encodeURIComponent(r)}` });
    }
    for (const s of NAV) {
      if (s.adminOnly && !isAdmin) continue;
      for (const it of s.items) {
        if (match(`${it.label} ${it.keywords ?? ""} ${s.title}`)) {
          out.push({ group: "Pages", label: it.label, hint: s.title, icon: it.icon, to: it.to });
        }
      }
    }
    for (const it of EXTRA_PAGES) {
      if (match(`${it.label} ${it.keywords ?? ""}`)) {
        out.push({ group: "Actions", label: it.label, icon: it.icon, to: it.to });
      }
    }
    return out;
  }, [q, repos, isAdmin]);

  useEffect(() => setSel(0), [q]);

  if (!open) return null;

  const go = (e: Entry | undefined) => {
    if (!e) return;
    onClose();
    navigate(e.to);
  };

  const onKey = (ev: React.KeyboardEvent) => {
    if (ev.key === "ArrowDown") {
      ev.preventDefault();
      setSel((s) => Math.min(entries.length - 1, s + 1));
    } else if (ev.key === "ArrowUp") {
      ev.preventDefault();
      setSel((s) => Math.max(0, s - 1));
    } else if (ev.key === "Enter") {
      ev.preventDefault();
      go(entries[sel]);
    } else if (ev.key === "Escape") {
      onClose();
    }
  };

  let lastGroup = "";
  return (
    <div className="palette-backdrop" onMouseDown={onClose}>
      <div
        className="palette"
        role="dialog"
        aria-label="Jump to"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <input
          ref={inputRef}
          className="palette-input"
          placeholder="Jump to a repo, page, or #pipeline…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={onKey}
          aria-label="Search"
        />
        <div className="palette-list" role="listbox">
          {entries.length === 0 && (
            <div className="palette-empty">{searching ? "Searching…" : "Nothing matches."}</div>
          )}
          {entries.map((e, i) => {
            const head = e.group !== lastGroup ? e.group : null;
            lastGroup = e.group;
            return (
              <div key={`${e.group}:${e.to}`}>
                {head && <div className="palette-group">{head}</div>}
                <div
                  role="option"
                  aria-selected={i === sel}
                  className={i === sel ? "palette-item selected" : "palette-item"}
                  onMouseEnter={() => setSel(i)}
                  onClick={() => go(e)}
                >
                  <Icon name={e.icon} />
                  <span>{e.label}</span>
                  {e.hint && <span className="muted">{e.hint}</span>}
                </div>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
