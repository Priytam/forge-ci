import { useEffect, useRef, useState } from "react";
import Icon, { type IconName } from "../Icon";

export interface MenuItem {
  label: string;
  icon?: IconName;
  danger?: boolean;
  disabled?: boolean;
  hint?: string;
  onSelect: () => void;
}

/** A kebab (⋯) button with a keyboard-navigable action menu. */
export default function Menu({ items, label }: { items: MenuItem[]; label: string }) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const root = useRef<HTMLDivElement>(null);
  const list = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => root.current && !root.current.contains(e.target as Node) && setOpen(false);
    document.addEventListener("mousedown", close);
    list.current?.querySelectorAll<HTMLButtonElement>("button")[active]?.focus();
    return () => document.removeEventListener("mousedown", close);
  }, [open, active]);
  const enabled = items.map((it, i) => (it.disabled ? -1 : i)).filter((i) => i >= 0);
  const move = (d: number) => {
    const at = enabled.indexOf(active);
    setActive(enabled[(at + d + enabled.length) % enabled.length] ?? 0);
  };
  return (
    <div className="menu" ref={root} onClick={(e) => e.stopPropagation()}>
      <button
        type="button"
        className="icon-btn"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => {
          setActive(enabled[0] ?? 0);
          setOpen((o) => !o);
        }}
        onKeyDown={(e) => e.stopPropagation()}
      >
        <Icon name="more" size={16} />
      </button>
      {open && (
        <div
          ref={list}
          className="popover menu-list"
          role="menu"
          onKeyDown={(e) => {
            e.stopPropagation();
            if (e.key === "ArrowDown") (e.preventDefault(), move(1));
            else if (e.key === "ArrowUp") (e.preventDefault(), move(-1));
            else if (e.key === "Escape" || e.key === "Tab") setOpen(false);
          }}
        >
          {items.map((it, i) => (
            <button
              key={it.label}
              type="button"
              role="menuitem"
              className={it.danger ? "menu-item menu-danger" : "menu-item"}
              disabled={it.disabled}
              title={it.hint}
              tabIndex={i === active ? 0 : -1}
              onClick={() => {
                setOpen(false);
                it.onSelect();
              }}
            >
              {it.icon && <Icon name={it.icon} size={14} />}
              {it.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
