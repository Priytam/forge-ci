/**
 * Environment switcher shown above a list: "All" plus one tab per environment
 * in promotion order (dev → staging → prod, as the server sorts them). Keeps
 * dev, staging and prod traffic apart at a glance instead of one mixed list.
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
      {shown.map((e) => (
        <button
          key={e}
          type="button"
          role="tab"
          aria-selected={value === e}
          className={`env-tab env-tab-${envKind(e)}${value === e ? " env-tab-active" : ""}`}
          onClick={() => onChange(e)}
        >
          {e}
        </button>
      ))}
    </div>
  );
}

/** Coarse kind of an environment name, for colour only. */
export function envKind(env: string): "prod" | "staging" | "dev" | "other" {
  const e = env.toLowerCase();
  if (e === "prod" || e === "production" || e.startsWith("prod-") || e.endsWith("-prod")) return "prod";
  if (["staging", "stage", "uat", "preprod", "pre-prod", "canary"].includes(e)) return "staging";
  if (["dev", "development", "local", "test", "testing", "qa", "integration"].includes(e)) return "dev";
  return "other";
}
