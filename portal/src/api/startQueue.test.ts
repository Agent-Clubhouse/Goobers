import { describe, expect, it, vi } from "vitest";
import { HttpDaemonClient } from "./httpClient";
import { goWireFixtures } from "./wire.generated";
describe("start queue transport", () => {
  it("uses scoped generated routes and sends no caller authority", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(goWireFixtures.startQueue)).mockResolvedValueOnce(Response.json(goWireFixtures.startQueueItem)).mockResolvedValueOnce(Response.json(goWireFixtures.startQueueItem));
    const client = new HttpDaemonClient({ fetch: fetcher });
    await client.getStartQueue("team space", { cursor: "next/+", limit: 25 }); await client.getStartQueueItem("team space", "trigger-abc"); await client.cancelQueuedStart("team space", "trigger-abc", { requestId: "same-key", reason: "Changed plan" });
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual(["/api/v1/gaggles/team%20space/start-queue?cursor=next%2F%2B&limit=25", "/api/v1/gaggles/team%20space/start-queue/trigger-abc", "/api/v1/gaggles/team%20space/start-queue/trigger-abc/cancel"]);
    expect(fetcher.mock.calls[2][1]).toMatchObject({ method: "POST", body: JSON.stringify({ requestId: "same-key", reason: "Changed plan" }) });
  });
});
