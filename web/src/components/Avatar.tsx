/**
 * Circular identity avatar.
 *
 * Forge stores no profile images — an identity is a bare string (an SSO email,
 * or a free-text name in open bootstrap mode). So the avatar is derived from
 * that string alone: initials over a tint picked by hashing it. The same person
 * gets the same colour on every screen, and nothing is fetched from a third
 * party (a Gravatar-style lookup would leak user emails to an outside service).
 *
 * An empty identity renders the outline glyph rather than a guessed letter — a
 * scheduled run genuinely has no actor, and inventing one would be a lie.
 */

/** One neutral tint for everyone: colour in this UI means status, and a red
 *  or green avatar read as a failed or passed run. Initials carry identity. */
const TINTS = [
  { bg: "var(--surface-2)", fg: "var(--text-muted)", ring: "var(--border-strong)" },
];

/** displayName strips an email down to its local part so the UI reads "alice",
 *  not "alice@tablespace.com" — the domain is noise when everyone shares one. */
export function displayName(identity: string): string {
  const id = identity.trim();
  const at = id.indexOf("@");
  return at > 0 ? id.slice(0, at) : id;
}

/**
 * initials takes up to two letters: the first of each of the first two word-ish
 * parts ("alice pandey" / "bob.mehta" / "carol_d" -> AP / BM / CD), else the
 * first two letters of a single word ("octocat" -> OC).
 */
export function initials(identity: string): string {
  const parts = displayName(identity)
    .split(/[\s._\-+]+/)
    .filter(Boolean);
  if (parts.length === 0) return "";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

/** tintFor hashes the FULL identity (not the display name) so two people whose
 *  local parts collide across domains still differ. */
export function tintFor(identity: string): (typeof TINTS)[number] {
  let h = 0;
  for (let i = 0; i < identity.length; i++) {
    h = (h * 31 + identity.charCodeAt(i)) | 0;
  }
  return TINTS[Math.abs(h) % TINTS.length];
}

interface AvatarProps {
  /** the raw identity string; "" renders the unknown-actor glyph */
  identity: string;
  /** diameter in px — 18 repo card, 22 run header, 26 approval row */
  size?: number;
  /** ring in the page ground, for overlapping stacks */
  stacked?: boolean;
  title?: string;
}

export default function Avatar({ identity, size = 22, stacked, title }: AvatarProps) {
  const text = initials(identity);
  const ringShadow = stacked ? ", 0 0 0 2px var(--bg)" : "";
  const common = {
    width: size,
    height: size,
    fontSize: Math.round(size * 0.4),
  };

  if (!text) {
    return (
      <span
        className="avatar avatar-unknown"
        style={{ ...common, boxShadow: `inset 0 0 0 1px var(--border)${ringShadow}` }}
        title={title ?? "no recorded actor"}
        aria-label={title ?? "no recorded actor"}
      >
        <svg
          width={Math.round(size * 0.5)}
          height={Math.round(size * 0.5)}
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          aria-hidden="true"
        >
          <circle cx="12" cy="8" r="3.5" />
          <path d="M5.5 20a6.5 6.5 0 0 1 13 0" />
        </svg>
      </span>
    );
  }

  const tint = tintFor(identity);
  return (
    <span
      className="avatar"
      style={{
        ...common,
        background: tint.bg,
        color: tint.fg,
        boxShadow: `inset 0 0 0 1px ${tint.ring}${ringShadow}`,
      }}
      title={title ?? identity}
      aria-label={title ?? identity}
    >
      {text}
    </span>
  );
}
