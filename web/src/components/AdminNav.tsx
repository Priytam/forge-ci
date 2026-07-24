import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { getIsAdmin } from "../api";

/** Admin dropdown in the header — only rendered for admins (all, in open mode). */
export default function AdminNav() {
  const [isAdmin, setIsAdmin] = useState(false);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    void getIsAdmin().then(setIsAdmin);
  }, []);

  if (!isAdmin) return null;

  return (
    <div
      className="admin-nav"
      onMouseLeave={() => setOpen(false)}
    >
      <button
        type="button"
        className="header-link admin-nav-trigger"
        onClick={() => setOpen((o) => !o)}
      >
        Admin ▾
      </button>
      {open && (
        <div className="admin-dropdown">
          <Link to="/admin/sso" onClick={() => setOpen(false)}>
            SSO configuration
          </Link>
          <Link to="/admin/audit" onClick={() => setOpen(false)}>
            Audit log
          </Link>
        </div>
      )}
    </div>
  );
}
