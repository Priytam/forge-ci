import { useEffect, useState } from "react";

export type ThemePref = "system" | "light" | "dark";

const KEY = "forge-theme";

function read(): ThemePref {
  try {
    const v = localStorage.getItem(KEY);
    if (v === "light" || v === "dark") return v;
  } catch {
    // storage unavailable: follow the OS
  }
  return "system";
}

function apply(pref: ThemePref) {
  const root = document.documentElement;
  if (pref === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", pref);
}

// Apply before first paint so the page never flashes the wrong theme.
apply(read());

/** The viewer's theme choice: follow the OS, or force light/dark. */
export function useTheme(): [ThemePref, (p: ThemePref) => void] {
  const [pref, setPref] = useState<ThemePref>(read);
  useEffect(() => {
    apply(pref);
    try {
      if (pref === "system") localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, pref);
    } catch {
      // not persisted; still applied for this page
    }
  }, [pref]);
  return [pref, setPref];
}
