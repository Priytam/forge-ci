import { createContext, useCallback, useContext, useRef, useState, type ReactNode } from "react";
import Icon from "../Icon";

export type ToastTone = "success" | "danger" | "neutral";

interface ToastItem {
  id: number;
  text: string;
  tone: ToastTone;
}

const ToastContext = createContext<(text: string, tone?: ToastTone) => void>(() => undefined);

/** Hosts toasts for its subtree: results of actions, bottom-right, auto-dismissed. */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const seq = useRef(0);
  const dismiss = useCallback((id: number) => setItems((all) => all.filter((t) => t.id !== id)), []);
  const show = useCallback(
    (text: string, tone: ToastTone = "neutral") => {
      const id = ++seq.current;
      setItems((all) => [...all.slice(-3), { id, text, tone }]);
      window.setTimeout(() => dismiss(id), tone === "danger" ? 8000 : 4000);
    },
    [dismiss],
  );
  return (
    <ToastContext.Provider value={show}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {items.map((t) => (
          <div key={t.id} className={`toast toast-${t.tone}`}>
            <Icon name={t.tone === "danger" ? "alert" : t.tone === "success" ? "check" : "dot"} size={14} />
            <span>{t.text}</span>
            <button type="button" className="icon-btn" aria-label="Dismiss" onClick={() => dismiss(t.id)}>
              <Icon name="close" size={12} />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

/** show(text, tone) — a no-op outside a ToastProvider. */
export function useToast() {
  return useContext(ToastContext);
}
