import { useCallback, useEffect, useRef, useState } from "react";
import { revalidation } from "../api";

export interface PollState<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
  refresh: () => void;
}

/**
 * Generic polling hook. Calls `fn` immediately and then every `intervalMs`.
 * Pass `enabled: false` to stop polling (e.g. once a terminal state is reached);
 * the last fetched data is kept.
 */
export function usePoll<T>(
  fn: () => Promise<T>,
  intervalMs: number,
  enabled: boolean = true
): PollState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState<boolean>(true);
  const [tick, setTick] = useState(0);

  const fnRef = useRef(fn);
  fnRef.current = fn;

  const refresh = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    let cancelled = false;

    const run = async () => {
      try {
        const result = await fnRef.current();
        if (!cancelled) {
          setData(result);
          setError(null);
          setLoading(false);
        }
        // Served from the stale-while-revalidate cache: show it now, then
        // swap in the fresh copy the moment it arrives.
        const fresh = revalidation(result);
        if (fresh && !cancelled) {
          await fresh;
          if (!cancelled) {
            const next = await fnRef.current();
            if (!cancelled) setData(next);
          }
        }
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err));
          setLoading(false);
        }
      }
    };

    // Always fetch once (even when polling is disabled) so the view has data.
    void run();

    if (!enabled) {
      return () => {
        cancelled = true;
      };
    }

    const timer = window.setInterval(run, intervalMs);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [intervalMs, enabled, tick]);

  return { data, error, loading, refresh };
}
