import { renderToStaticMarkup } from "react-dom/server";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it } from "vitest";
import RunnersTable from "./RunnersTable";
import { buildViews } from "./model";
import { FLEET, loadFor, NOW } from "./fixtures";

function render(cards = false) {
  const views = buildViews(FLEET, loadFor(FLEET));
  return renderToStaticMarkup(
    <MemoryRouter>
      <RunnersTable
        groups={[{ key: "all", views }]}
        sort={{ key: "state", dir: 1 }}
        onSort={() => undefined}
        selected={null}
        onSelect={() => undefined}
        onOpen={() => undefined}
        actions={() => []}
        cards={cards}
        now={NOW}
      />
    </MemoryRouter>,
  );
}

const headers = (html: string) => [...html.matchAll(/<th[^>]*>(.*?)<\/th>/g)].map((m) => m[1].replace(/<[^>]+>/g, "").replace(/[↑↓]/g, "").trim());

describe("runners table column order (bug 1)", () => {
  const html = render();
  it("headers are in the agreed order", () => {
    expect(headers(html)).toEqual(["Runner", "State", "Tags", "Now", "Utilisation", "Version", "Last contact", "Actions"]);
  });
  it("every row's cells follow the header order", () => {
    const rows = [...html.matchAll(/<tr[^>]*data-row="[^"]*"[^>]*>(.*?)<\/tr>/g)].map((m) => [...m[1].matchAll(/data-col="([^"]+)"/g)].map((c) => c[1]));
    expect(rows).toHaveLength(FLEET.length);
    for (const cols of rows) expect(cols).toEqual(["runner", "state", "tags", "now", "utilisation", "version", "contact", "actions"]);
  });
  it("the State cell holds the state and the Version cell the version", () => {
    const row = html.match(/<tr[^>]*data-row="all:gce-builder-1"[^>]*>(.*?)<\/tr>/)![1];
    const cell = (k: string) => row.match(new RegExp(`data-col="${k}"[^>]*>(.*?)</td>`))![1].replace(/<[^>]+>/g, "");
    expect(cell("state")).toBe("Online");
    expect(cell("version")).toBe("99e8c2c · outdated");
    expect(cell("state")).not.toContain("99e8c2c");
  });
  it("paused and offline rows are tinted and say so", () => {
    expect(html).toMatch(/class="runner-row row-paused"[^>]*data-row="all:builder-2"|data-row="all:builder-2"[^>]*class="runner-row row-paused"/);
    expect(html).toContain("Paused");
    expect(html).toMatch(/data-row="all:gce-runner-dev"[^>]*class="runner-row row-offline"/);
  });
});

describe("runner cards", () => {
  it("narrow screens get one card per runner with the same fields", () => {
    const html = render(true);
    expect((html.match(/<article/g) ?? []).length).toBe(FLEET.length);
    expect(html).toContain('data-col="version"');
    expect(html).not.toContain("<table");
  });
});
