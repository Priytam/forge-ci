import { useEffect, useState } from "react";

/**
 * useState remembered in localStorage for this viewer (a collapsed panel, a
 * density choice). Falls back to the default when storage is unavailable.
 */
export function useStoredState<T extends string>(key: string, initial: T, allowed: readonly T[]): [T, (v: T) => void] {
  const [value, setValue] = useState<T>(() => {
    try {
      const v = window.localStorage.getItem(key) as T | null;
      return v && allowed.includes(v) ? v : initial;
    } catch {
      return initial;
    }
  });
  useEffect(() => {
    try {
      window.localStorage.setItem(key, value);
    } catch {
      // per-viewer convenience only
    }
  }, [key, value]);
  return [value, setValue];
}
