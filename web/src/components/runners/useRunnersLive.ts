import { useCallback, useEffect, useRef, useState } from "react";
import { getRunnerLoad, listRunners, runnerStreamURL, type RangeQuery, type Runner, type RunnerLoad } from "../../api";

export interface LiveRunners {
  runners: Runner[] | null;
  load: RunnerLoad | null;
  error: string | null;
  /** true while the SSE stream is delivering; false when polling instead */
  live: boolean;
  /** fetch now (after an action), without waiting for the stream */
  refresh: () => void;
}

const FALLBACK_POLL_MS = 10_000;

/**
 * Runners and their load for a range, pushed by /runners/stream. If the
 * stream can't be opened (or keeps failing) it falls back to polling.
 */
export function useRunnersLive(range: RangeQuery): LiveRunners {
  const [runners, setRunners] = useState<Runner[] | null>(null);
  const [load, setLoad] = useState<RunnerLoad | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [live, setLive] = useState(false);
  const key = `${range.range}|${range.from ?? ""}|${range.to ?? ""}`;
  const rangeRef = useRef(range);
  rangeRef.current = range;

  const fetchOnce = useCallback(async () => {
    try {
      const [r, l] = await Promise.all([listRunners(), getRunnerLoad(rangeRef.current)]);
      setRunners(r);
      setLoad(l);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);

  useEffect(() => {
    let poll: number | undefined;
    let failures = 0;
    let es: EventSource | null = null;
    const fallback = () => {
      es?.close();
      es = null;
      setLive(false);
      void fetchOnce();
      poll = window.setInterval(() => void fetchOnce(), FALLBACK_POLL_MS);
    };
    if (typeof EventSource === "undefined") {
      fallback();
    } else {
      es = new EventSource(runnerStreamURL(rangeRef.current));
      es.addEventListener("snapshot", (ev) => {
        failures = 0;
        try {
          const snap = JSON.parse((ev as MessageEvent).data) as { runners: Runner[]; load: RunnerLoad };
          setRunners(snap.runners);
          setLoad(snap.load);
          setError(null);
          setLive(true);
        } catch {
          // a malformed frame: keep the last snapshot
        }
      });
      es.onerror = () => {
        // EventSource retries on its own; give up after a few failures in a row.
        if (++failures >= 3) fallback();
        else setLive(false);
      };
    }
    return () => {
      es?.close();
      if (poll) window.clearInterval(poll);
    };
  }, [key, fetchOnce]);

  return { runners, load, error, live, refresh: () => void fetchOnce() };
}
