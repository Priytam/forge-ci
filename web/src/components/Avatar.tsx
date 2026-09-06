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

/** Tints, built exactly like the existing status chips: colour at 0.18 over the
 *  ground, hairline at 0.35, bright foreground. Nothing new enters the palette. */
const TINTS = [
  { bg: "rgba(56, 139, 253, 0.18)", fg: "#60a5fa", ring: "rgba(56, 139, 253, 0.35)" },
  { bg: "rgba(188, 140, 255, 0.18)", fg: "#c4a7ff", ring: "rgba(188, 140, 255, 0.35)" },
  { bg: "rgba(46, 164, 79, 0.18)", fg: "#4ade80", ring: "rgba(46, 164, 79, 0.35)" },
  { bg: "rgba(232, 131, 58, 0.18)", fg: "#fb923c", ring: "rgba(232, 131, 58, 0.35)" },
  { bg: "rgba(212, 167, 44, 0.18)", fg: "#fbbf24", ring: "rgba(212, 167, 44, 0.35)" },
  { bg: "rgba(229, 83, 75, 0.18)", fg: "#f87171", ring: "rgba(229, 83, 75, 0.35)" },
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
