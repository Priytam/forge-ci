import { describe, expect, it } from "vitest";
import { toYaml } from "./yaml";

describe("toYaml", () => {
  it("renders a manifest the way kubectl would", () => {
    const m = {
      apiVersion: "apps/v1",
      kind: "Deployment",
      metadata: { name: "cart-api", labels: { "app.kubernetes.io/name": "cart-api" }, annotations: {} },
      spec: {
        replicas: 2,
        paused: false,
        template: { spec: { containers: [{ name: "app", image: "localhost:5001/lab/echo:v1", ports: [{ containerPort: 8080 }] }] } },
      },
    };
    expect(toYaml(m)).toBe(
      [
        "apiVersion: apps/v1",
        "kind: Deployment",
        "metadata:",
        "  name: cart-api",
        "  labels:",
        "    app.kubernetes.io/name: cart-api",
        "  annotations: {}",
        "spec:",
        "  replicas: 2",
        "  paused: false",
        "  template:",
        "    spec:",
        "      containers:",
        "      - name: app",
        "        image: localhost:5001/lab/echo:v1",
        "        ports:",
        "        - containerPort: 8080",
      ].join("\n")
    );
  });

  it("quotes strings YAML would read as something else", () => {
    expect(toYaml({ a: "true", b: "8080", c: "", d: "a: b", e: "null", f: "plain text" })).toBe(
      'a: "true"\nb: "8080"\nc: ""\nd: "a: b"\ne: "null"\nf: plain text'
    );
  });

  it("keeps multi-line strings as blocks", () => {
    expect(toYaml({ data: { "app.conf": "a=1\nb=2\n" } })).toBe("data:\n  app.conf: |\n    a=1\n    b=2");
  });
});
