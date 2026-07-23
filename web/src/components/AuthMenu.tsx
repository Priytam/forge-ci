import { useEffect, useState } from "react";
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
        {user.email} ▾
      </button>
      {open && (
        <div className="auth-dropdown">
          <div className="auth-dropdown-meta muted">
            {user.name} · via {user.provider}
          </div>
          <button
            type="button"
            className="btn auth-signout"
            onClick={() => void signOut()}
          >
            Sign out
          </button>
        </div>
      )}
    </div>
  );
}
