import { useState } from "react";
import { Link } from "react-router-dom";
import Icon from "../Icon";
import RunnerPools from "../RunnerPools";
import Drawer from "../ui/Drawer";
import { useToast } from "../ui/Toast";

type Executor = "shell" | "docker" | "kubernetes";

const EXEC_NOTE: Record<Executor, string> = {
  shell: "Runs jobs directly on the host, one at a time.",
  docker: "Runs each job in a container; one at a time per runner.",
  kubernetes: "A manager that runs up to N jobs in parallel as ephemeral pods (--concurrency).",
};

const GUIDE: Record<Executor, string> = {
  shell: "/docs/runner-vm",
  docker: "/docs/runner-docker",
  kubernetes: "/docs/runner-kubernetes",
};

/** "Add runner": the registration command for the chosen executor, autoscaling, and docs. */
export default function AddRunnerDrawer({ isAdmin, onClose }: { isAdmin: boolean; onClose: () => void }) {
  const toast = useToast();
  const [exec, setExec] = useState<Executor>("docker");
  const [tags, setTags] = useState("");
  const [conc, setConc] = useState(4);
  const [pool, setPool] = useState("");
  const cleanTags = tags
    .split(",")
    .map((t) => t.trim())
    .filter(Boolean)
    .join(",");
  const cmd = [
    "./forge-runner",
    `--server=${window.location.origin}`,
    `--executor=${exec}`,
    cleanTags && `--tags=${cleanTags}`,
    exec === "kubernetes" && `--concurrency=${conc}`,
    pool.trim() && `--pool=${pool.trim()}`,
    "--token=$RUNNER_TOKEN",
  ]
    .filter(Boolean)
    .join(" ");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(cmd);
      toast("Command copied", "success");
    } catch {
      toast("Couldn't copy — select the command instead", "danger");
    }
  };
  return (
    <Drawer title="Add a runner" subtitle="Runners register themselves; run this where the jobs should run." onClose={onClose} wide>
      <div className="form-stack">
        <div className="field">
          <span>Executor</span>
          <div className="segmented" role="radiogroup" aria-label="Executor">
            {(["shell", "docker", "kubernetes"] as Executor[]).map((e) => (
              <button key={e} type="button" role="radio" aria-checked={e === exec} className={e === exec ? "seg-btn seg-active" : "seg-btn"} onClick={() => setExec(e)}>
                {e}
              </button>
            ))}
          </div>
          <span className="muted small">{EXEC_NOTE[exec]}</span>
        </div>
        <label className="field">
          <span>Tags (comma-separated)</span>
          <input className="mono" placeholder="linux, docker" value={tags} onChange={(e) => setTags(e.target.value)} />
        </label>
        <label className="field">
          <span>Pool (optional)</span>
          <input className="mono" placeholder="the autoscaled pool it belongs to" value={pool} onChange={(e) => setPool(e.target.value)} />
        </label>
        {exec === "kubernetes" && (
          <label className="field">
            <span>Parallel jobs (concurrency)</span>
            <input type="number" min={1} max={64} value={conc} onChange={(e) => setConc(Math.max(1, Number(e.target.value) || 1))} style={{ width: 96 }} />
          </label>
        )}
        <div className="cmd-box">
          <code className="mono">{cmd}</code>
          <button type="button" className="btn btn-icon" onClick={() => void copy()}>
            <Icon name="copy" size={14} /> Copy
          </button>
        </div>
        <p className="muted small">
          <code>$RUNNER_TOKEN</code> is needed when the server runs with <code>RUNNER_AUTH=on</code>
          {isAdmin && (
            <>
              {" "}
              — create one under <Link to="/admin/runner-tokens">Runner tokens</Link>
            </>
          )}
          . Repos pick runners by tag (repo Settings → Runner tags); a job's own <code>tags:</code> override them.
        </p>
        <p className="small">
          Guides: <Link to={GUIDE[exec]}>{exec === "shell" ? "VM / shell runner" : exec === "docker" ? "Docker runner" : "Kubernetes runner"}</Link> ·{" "}
          <Link to="/docs/runner-docker">Docker</Link> · <Link to="/docs/runner-kubernetes">Kubernetes</Link> · <Link to="/docs/runner-vm">VM</Link>
        </p>
      </div>

      <h3 className="drawer-h">Autoscaling</h3>
      <p className="small">
        Pools grow and shrink runners with the queue. Forge exports <code>forge_queue_pending_jobs{"{tags=\"…\"}"}</code> and{" "}
        <code>forge_queue_oldest_wait_seconds</code> at <code>/api/v1/metrics</code> for KEDA / HPA, or manages pools itself below.
      </p>
      {isAdmin ? <RunnerPools /> : <p className="muted small">Pools are managed by platform admins.</p>}
    </Drawer>
  );
}
