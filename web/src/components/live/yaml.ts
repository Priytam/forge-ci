/**
 * JSON → YAML, for showing live manifests the way people read them. Covers
 * what JSON can hold (objects, arrays, strings, numbers, booleans, null);
 * strings are quoted only when YAML would read them as something else.
 */
export function toYaml(v: unknown, indent = ""): string {
  if (Array.isArray(v)) {
    if (v.length === 0) return "[]";
    return v
      .map((item) => {
        const body = toYaml(item, indent + "  ");
        return isBlock(item) ? `${indent}- ${body.trimStart()}` : `${indent}- ${body}`;
      })
      .join("\n");
  }
  if (v !== null && typeof v === "object") {
    const entries = Object.entries(v as Record<string, unknown>);
    if (entries.length === 0) return "{}";
    return entries
      .map(([k, val]) => {
        const key = indent + scalar(k);
        if (isBlock(val)) return `${key}:\n${toYaml(val, Array.isArray(val) ? indent : indent + "  ")}`;
        return `${key}: ${toYaml(val, indent + "  ")}`;
      })
      .join("\n");
  }
  if (typeof v === "string") {
    if (v.includes("\n")) {
      return "|\n" + v.replace(/\n$/, "").split("\n").map((l) => indent + l).join("\n");
    }
    return scalar(v);
  }
  return String(v);
}

function isBlock(v: unknown): boolean {
  return v !== null && typeof v === "object" && (Array.isArray(v) ? v.length > 0 : Object.keys(v).length > 0);
}

const PLAIN = /^[A-Za-z0-9_./][A-Za-z0-9_ ./@:+-]*$/;
const SPECIAL = /^(true|false|yes|no|on|off|null|~|-?\d+(\.\d+)?([eE][-+]?\d+)?|0x[0-9a-fA-F]+)$/i;

function scalar(s: string): string {
  if (s !== "" && PLAIN.test(s) && !SPECIAL.test(s) && !s.endsWith(" ") && !s.includes(": ") && !s.includes(" #")) {
    return s;
  }
  return JSON.stringify(s);
}
