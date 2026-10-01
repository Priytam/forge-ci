import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { getIsAdmin } from "../api";

/** Admin dropdown in the header — only rendered for admins (all, in open mode). */
export default function AdminNav() {
  const [isAdmin, setIsAdmin] = useState(false);
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
  }, []);

  // Close on a click outside the menu or on Escape. Using outside-click rather
  // than onMouseLeave avoids the menu closing while the cursor crosses the gap
  // between the trigger and the items (which ate the click before).
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  if (!isAdmin) return null;

  return (
    <div className="admin-nav" ref={ref}>
      <button
        type="button"
        className="header-link admin-nav-trigger"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        Admin ▾
      </button>
      {open && (
        <div className="admin-dropdown" role="menu">
          <Link to="/admin/sso" role="menuitem" onClick={() => setOpen(false)}>
            SSO configuration
          </Link>
          <Link to="/admin/audit" role="menuitem" onClick={() => setOpen(false)}>
            Audit log
          </Link>
          <Link to="/admin/runner-tokens" role="menuitem" onClick={() => setOpen(false)}>
            Runner tokens
          </Link>
        </div>
      )}
    </div>
  );
}
