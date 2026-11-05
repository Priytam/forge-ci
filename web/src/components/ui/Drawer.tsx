import { useEffect, useRef, type ReactNode } from "react";
import Icon from "../Icon";

/** A right-hand panel over the page (full width on phones); Esc or the backdrop closes it. */
export default function Drawer({
  title,
  subtitle,
  onClose,
  children,
  wide,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  onClose: () => void;
  children: ReactNode;
  wide?: boolean;
}) {
  const panel = useRef<HTMLDivElement>(null);
  const close = useRef(onClose);
  close.current = onClose;
  // Focus the panel on open and give focus back on close — once, however
  // often the page re-renders underneath.
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null;
    panel.current?.focus();
    const esc = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !document.querySelector(".dialog")) close.current();
    };
    document.addEventListener("keydown", esc);
    return () => {
      document.removeEventListener("keydown", esc);
      prev?.focus?.();
    };
  }, []);
  return (
    <div className="overlay overlay-drawer" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <aside ref={panel} className={wide ? "drawer drawer-wide" : "drawer"} role="dialog" aria-modal="true" tabIndex={-1}>
        <header className="drawer-head">
          <div className="drawer-title">
            <h2>{title}</h2>
            {subtitle && <div className="drawer-sub">{subtitle}</div>}
          </div>
          <button type="button" className="icon-btn" aria-label="Close" onClick={onClose}>
            <Icon name="close" />
          </button>
        </header>
        <div className="drawer-body">{children}</div>
      </aside>
    </div>
  );
}
