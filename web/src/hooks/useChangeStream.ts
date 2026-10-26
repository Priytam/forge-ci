import { useEffect, useRef } from "react";
import { subscribeChanges } from "../api";

/**
 * Calls onChange when a change is updated (webhook or refresh), at most once
 * per `settleMs` so a burst of events costs one refetch. Pass a change id to
 * hear only about that change.
 */
export function useChangeStream(onChange: () => void, onlyId?: number, settleMs = 400) {
  const fn = useRef(onChange);
  fn.current = onChange;
  useEffect(() => {
    let timer: number | undefined;
    const stop = subscribeChanges((id) => {
      if (onlyId !== undefined && id !== onlyId) return;
      window.clearTimeout(timer);
      timer = window.setTimeout(() => fn.current(), settleMs);
    });
    return () => {
      window.clearTimeout(timer);
      stop();
    };
  }, [onlyId, settleMs]);
}
