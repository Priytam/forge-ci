/**
 * Audit chip: which registered config version a pipeline ran with.
 * null = a one-off custom config was supplied at run time.
 */
export default function ConfigChip({ version }: { version: number | null }) {
  if (version !== null) {
    return <span className="cfg-chip">config v{version}</span>;
  }
  return <span className="cfg-chip cfg-chip-custom">custom config</span>;
}
