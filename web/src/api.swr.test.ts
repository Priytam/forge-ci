import { afterEach, describe, expect, it, vi } from "vitest";

// Each test gets a fresh module so the cache starts empty.
async function freshApi() {
  vi.resetModules();
  return import("./api");
}

function mockFetch(bodies: unknown[]) {
  let i = 0;
  const fn = vi.fn(async () => new Response(JSON.stringify(bodies[Math.min(i++, bodies.length - 1)]), { status: 200 }));
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("GET stale-while-revalidate cache", () => {
  it("serves a repeat read from memory without a request while fresh", async () => {
    const api = await freshApi();
    const fetch = mockFetch([[{ repo: "a/b" }]]);
    await api.listRegistry();
    const again = await api.listRegistry();
    expect(again).toEqual([{ repo: "a/b" }]);
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(api.revalidation(again)).toBeUndefined();
  });

  it("returns stale data at once and refreshes it in the background", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const api = await freshApi();
    const fetch = mockFetch([[{ repo: "old/one" }], [{ repo: "new/one" }]]);
    await api.listRegistry();
    vi.setSystemTime(Date.now() + 5_000);
    const stale = await api.listRegistry();
    expect(stale).toEqual([{ repo: "old/one" }]);
    const pending = api.revalidation(stale);
    expect(pending).toBeDefined();
    await pending;
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(await api.listRegistry()).toEqual([{ repo: "new/one" }]);
  });

  it("drops every cached read after a write", async () => {
    const api = await freshApi();
    const fetch = mockFetch([[{ repo: "a/b" }], {}, [{ repo: "a/b" }, { repo: "c/d" }]]);
    await api.listRegistry();
    await api.registerRepo({ repo: "c/d", provider: "git" } as never);
    expect(await api.listRegistry()).toEqual([{ repo: "a/b" }, { repo: "c/d" }]);
    expect(fetch).toHaveBeenCalledTimes(3);
  });

  it("refetches once an entry is older than a minute", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const api = await freshApi();
    const fetch = mockFetch([[{ repo: "a/b" }], [{ repo: "x/y" }]]);
    await api.listRegistry();
    vi.setSystemTime(Date.now() + 61_000);
    expect(await api.listRegistry()).toEqual([{ repo: "x/y" }]);
    expect(fetch).toHaveBeenCalledTimes(2);
  });
});
