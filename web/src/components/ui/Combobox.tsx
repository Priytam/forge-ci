import { useEffect, useId, useMemo, useRef, useState } from "react";
import Icon from "../Icon";

/**
 * A filter picker: type to search the known values, arrows + Enter to pick,
 * × to clear. Empty means "all". Free text is accepted too (Enter on a value
 * not in the list), so a repo that has no history yet can still be chosen.
 */
export default function Combobox({
  value,
  onChange,
  options,
  placeholder,
  label,
  allLabel,
}: {
  value: string;
  onChange: (v: string) => void;
  options: string[];
  placeholder: string;
  label: string;
  /** shown when empty, e.g. "All runners" */
  allLabel: string;
}) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const root = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const matches = useMemo(() => {
    const q = query.trim().toLowerCase();
    const uniq = [...new Set(options)].sort((a, b) => a.localeCompare(b));
    return (q ? uniq.filter((o) => o.toLowerCase().includes(q)) : uniq).slice(0, 50);
  }, [options, query]);
  useEffect(() => {
    if (!open) return;
    const close = (e: MouseEvent) => root.current && !root.current.contains(e.target as Node) && setOpen(false);
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);
  const pick = (v: string) => {
    onChange(v);
    setQuery("");
    setOpen(false);
  };
  return (
    <div className={value ? "combo combo-set" : "combo"} ref={root}>
      <Icon name="search" size={13} />
      <input
        ref={input}
        role="combobox"
        aria-label={label}
        aria-expanded={open}
        aria-controls={`${id}-list`}
        aria-activedescendant={open && matches[active] ? `${id}-${active}` : undefined}
        aria-autocomplete="list"
        className={value ? "mono" : undefined}
        placeholder={open ? placeholder : value || allLabel}
        value={open ? query : value || ""}
        onFocus={() => {
          setOpen(true);
          setActive(0);
        }}
        onChange={(e) => {
          setQuery(e.target.value);
          setActive(0);
          setOpen(true);
        }}
        onKeyDown={(e) => {
          e.stopPropagation();
          if (e.key === "ArrowDown") (e.preventDefault(), setActive((a) => Math.min(a + 1, matches.length - 1)));
          else if (e.key === "ArrowUp") (e.preventDefault(), setActive((a) => Math.max(a - 1, 0)));
          else if (e.key === "Enter") {
            e.preventDefault();
            if (matches[active]) pick(matches[active]);
            else if (query.trim()) pick(query.trim());
            input.current?.blur();
          } else if (e.key === "Escape") {
            setOpen(false);
            input.current?.blur();
          }
        }}
      />
      {value && (
        <button type="button" className="icon-btn combo-clear" aria-label={`Clear ${label}`} onClick={() => pick("")}>
          <Icon name="close" size={12} />
        </button>
      )}
      {open && (
        <ul id={`${id}-list`} className="popover combo-list" role="listbox" aria-label={label}>
          {matches.length === 0 && <li className="combo-empty">{query.trim() ? `Press Enter to filter by “${query.trim()}”` : "No known values yet"}</li>}
          {matches.map((m, i) => (
            <li
              key={m}
              id={`${id}-${i}`}
              role="option"
              aria-selected={m === value}
              className={i === active ? "combo-opt combo-active" : "combo-opt"}
              onMouseEnter={() => setActive(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(m);
              }}
            >
              <span className="mono">{m}</span>
              {m === value && <Icon name="check" size={12} />}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
