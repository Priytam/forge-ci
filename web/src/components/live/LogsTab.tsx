import { useEffect, useMemo, useRef, useState } from "react";
import { createLiveOp, getLiveManifest, streamOpOutput, type LiveResource } from "../../api";

/**
 * A pod's logs, streamed from the cluster through the agent (Forge holds no
 * cluster credentials). Leaving the tab stops the stream.
 */
export default function LogsTab({ appId, pod }: { appId: number; pod: LiveResource }) {
  const [containers, setContainers] = useState<string[]>([]);
  const [container, setContainer] = useState("");
  const [tail, setTail] = useState(500);
  const [follow, setFollow] = useState(true);
  const [text, setText] = useState("");
  const [state, setState] = useState<"starting" | "streaming" | "ended" | "error">("starting");
  const [message, setMessage] = useState("");
  const [q, setQ] = useState("");
  const box = useRef<HTMLPreElement>(null);
  const stick = useRef(true);

  useEffect(() => {
    getLiveManifest(appId, pod.uid).then((m) => {
      const spec = (m as { spec?: { containers?: { name: string }[] } }).spec;
      const names = (spec?.containers ?? []).map((c) => c.name);
      setContainers(names);
      setContainer((c) => c || names[0] || "");
    }, () => undefined);
  }, [appId, pod.uid]);

  useEffect(() => {
    if (!container && containers.length > 0) return;
    let stop = () => undefined as void;
    let cancelled = false;
    setText("");
    setState("starting");
    createLiveOp(appId, { kind: "logs", pod: `${pod.namespace}/${pod.name}`, container, tail, follow }).then(
      (op) => {
        if (cancelled) return;
        setState("streaming");
        stop = streamOpOutput(
          op.id,
          (chunk) => setText((t) => (t + chunk).slice(-2_000_000)),
          (done) => {
            setState(done.state === "failed" ? "error" : "ended");
            setMessage(done.result?.message || done.message);
          }
        );
      },
      (e) => {
        setState("error");
        setMessage(String(e?.message ?? e));
      }
    );
    return () => {
      cancelled = true;
      stop();
    };
  }, [appId, pod.namespace, pod.name, container, containers.length, tail, follow]);

  useEffect(() => {
    if (stick.current && box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [text]);

  const lines = useMemo(() => {
    const all = text.split("\n");
    const needle = q.trim().toLowerCase();
    return needle ? all.filter((l) => l.toLowerCase().includes(needle)) : all;
  }, [text, q]);

  return (
    <div className="lv-logs">
      <div className="lv-logs-bar">
        {containers.length > 1 && (
          <select value={container} onChange={(e) => setContainer(e.target.value)} aria-label="Container">
            {containers.map((c) => <option key={c}>{c}</option>)}
          </select>
        )}
        <select value={tail} onChange={(e) => setTail(Number(e.target.value))} aria-label="Lines">
          {[100, 500, 2000].map((n) => <option key={n} value={n}>last {n}</option>)}
        </select>
        <label className="check-row"><input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow</label>
        <input className="filter-search" placeholder="Filter lines…" value={q} onChange={(e) => setQ(e.target.value)} aria-label="Filter log lines" />
        <span className={`lv-logs-state lv-logs-${state}`}>{state === "streaming" ? (follow ? "live" : "loading") : state}</span>
      </div>
      <pre ref={box} className="lv-logs-body" onScroll={(e) => {
        const el = e.currentTarget;
        stick.current = el.scrollTop + el.clientHeight >= el.scrollHeight - 24;
      }}>
        {lines.join("\n") || (state === "starting" ? "Asking the agent…" : "")}
      </pre>
      {message && state !== "streaming" && <div className="muted lv-logs-msg">{message}</div>}
    </div>
  );
}
