import { describe, expect, it, vi } from "vitest";
import { HttpDaemonClient } from "./httpClient";
import { goWireFixtures } from "./wire.generated";

describe("workbench source transport", () => {
  it("loads the server-selected graph without supplying source data or cursors", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(Response.json(goWireFixtures.workbenchGraph));
    const client = new HttpDaemonClient({ fetch: fetcher });
    expect(await client.getWorkbenchGraph("team space")).toEqual(goWireFixtures.workbenchGraph);
    expect(fetcher).toHaveBeenCalledExactlyOnceWith("/api/v1/gaggles/team%20space/workbench/graph", expect.objectContaining({ method: "GET" }));
    expect(fetcher.mock.calls[0][1]?.body).toBeUndefined();
  });
  it("reads only the selected document source with an opaque bounded continuation", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(Response.json(goWireFixtures.workbenchDocuments));
    const client = new HttpDaemonClient({ fetch: fetcher });
    const controller = new AbortController();
    expect(await client.getWorkbenchDocuments("team space", "strategy/source", { cursor: "opaque/+?=cursor", limit: 8 }, { signal: controller.signal })).toEqual(goWireFixtures.workbenchDocuments);
    expect(fetcher).toHaveBeenCalledWith("/api/v1/gaggles/team%20space/workbench/sources/strategy%2Fsource/documents?cursor=opaque%2F%2B%3F%3Dcursor&limit=8", expect.objectContaining({ method: "GET", signal: expect.any(AbortSignal) }));
    expect(fetcher.mock.calls[0][1]?.body).toBeUndefined();
  });
  it("uses configured source paths with opaque cursors and pins immutable identity on detail reads", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(goWireFixtures.workbenchSources)).mockResolvedValueOnce(Response.json(goWireFixtures.workbenchItems)).mockResolvedValueOnce(Response.json(goWireFixtures.workbenchItem));
    const client = new HttpDaemonClient({ fetch: fetcher });
    await client.listWorkbenchSources("team space");
    await client.getWorkbenchItems("team space", "backlog/source", { cursor: "opaque/+?=cursor", limit: 50 });
    await client.getWorkbenchItem("team space", "backlog/source", { id: "item/12", expectedSourceId: "stable+identity" });
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual([
      "/api/v1/gaggles/team%20space/workbench/sources",
      "/api/v1/gaggles/team%20space/workbench/sources/backlog%2Fsource/items?cursor=opaque%2F%2B%3F%3Dcursor&limit=50",
      "/api/v1/gaggles/team%20space/workbench/sources/backlog%2Fsource/items/item%2F12?expectedSourceId=stable%2Bidentity",
    ]);
    for (const [, init] of fetcher.mock.calls) { expect(init?.method).toBe("GET"); expect(init?.body).toBeUndefined(); }
  });
});
