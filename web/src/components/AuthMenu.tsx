import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { getMe, logout, type AuthUser } from "../api";

/** Header widget: signed-in identity with a Sign out menu. */
export default function AuthMenu() {
  const [user, setUser] = useState<AuthUser | null>(null);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    void getMe().then(setUser);
  }, []);

  if (!user) return null;

  const signOut = async () => {
    await logout();
    window.location.reload();
  };

  return (
    <div className="auth-menu">
      <button
        type="button"
        className="auth-trigger"
        onClick={() => setOpen((o) => !o)}
      >
        {user.email} <span className="muted">▾</span>
      </button>
      {open && (
        <div className="auth-dropdown">
          <div className="auth-dropdown-meta">
            <div>{user.name || user.email}</div>
            <div className="muted">
              {user.email} · via {user.provider}
            </div>
          </div>
          <div className="menu-sep" />
          <Link to="/tokens" className="menu-item" onClick={() => setOpen(false)}>
            API tokens
          </Link>
          <div className="menu-sep" />
          <button type="button" className="menu-item" onClick={() => void signOut()}>
            Sign out
          </button>
        </div>
      )}
    </div>
  );
}
