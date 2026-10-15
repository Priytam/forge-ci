import { useEffect, useState } from "react";
import { getEnvRegistry, type EnvPolicy, type RegisteredEnv } from "../api";

/** This server's environment registry (empty until loaded or when unset). */
export function useEnvRegistry(): EnvPolicy {
  const [p, setP] = useState<EnvPolicy>({ environments: [], unknown: "allow" });
  useEffect(() => {
    void getEnvRegistry().then(setP);
  }, []);
  return p;
}

/** The registered environment for name (by name or alias), if any. */
export function findEnv(policy: EnvPolicy, name: string): RegisteredEnv | undefined {
  const n = name.toLowerCase();
  return policy.environments.find(
    (e) => e.name.toLowerCase() === n || e.aliases.some((a) => a.toLowerCase() === n)
  );
}

/**
 * Environment switcher shown above a list: "All" plus one tab per environment
 * in the server's order (the admin's registry first, then dev → staging →
 * prod by name). Colour comes from the registered tier, or a guess from the
 * name when the server has no registry. Hovering a tab shows which branches
 * may deploy there.
 */
export default function EnvTabs({
  envs,
  value,
  onChange,
  allLabel = "All environments",
}: {
  envs: string[];
  value: string;
  onChange: (env: string) => void;
  allLabel?: string;
}) {
  const policy = useEnvRegistry();
  const shown = value && !envs.includes(value) ? [...envs, value] : envs;
  if (shown.length === 0) return null;
  return (
    <div className="env-tabs" role="tablist" aria-label="Environment">
      <button
        type="button"
        role="tab"
        aria-selected={value === ""}
        className={value === "" ? "env-tab env-tab-active" : "env-tab"}
        onClick={() => onChange("")}
      >
        {allLabel}
      </button>
      {shown.map((e) => {
        const reg = findEnv(policy, e);
        return (
          <button
            key={e}
            type="button"
            role="tab"
            aria-selected={value === e}
            title={reg ? branchesLabel(reg) : undefined}
            className={`env-tab env-tab-${envKind(e, policy)}${value === e ? " env-tab-active" : ""}`}
            onClick={() => onChange(e)}
          >
            {e}
          </button>
        );
      })}
    </div>
  );
}

/** "deploys from: main, release/*" or "deploys from: any branch". */
export function branchesLabel(e: RegisteredEnv): string {
  return `deploys from: ${e.allowed_refs.length ? e.allowed_refs.join(", ") : "any branch"}`;
}

/** Kind of an environment, for colour: the registered tier when the server
 *  has one, else a guess from common names. */
export function envKind(env: string, policy?: EnvPolicy): "prod" | "staging" | "dev" | "other" {
  const reg = policy ? findEnv(policy, env) : undefined;
  if (reg) return reg.tier;
  const e = env.toLowerCase();
  if (e === "prod" || e === "production" || e.startsWith("prod-") || e.endsWith("-prod")) return "prod";
  if (["staging", "stage", "uat", "preprod", "pre-prod", "canary"].includes(e)) return "staging";
  if (["dev", "development", "local", "test", "testing", "qa", "integration"].includes(e)) return "dev";
  return "other";
}
