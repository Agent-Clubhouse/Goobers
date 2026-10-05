import { describe, expect, it, vi } from "vitest";
import { HttpDaemonClient } from "./httpClient";
import { goWireFixtures } from "./wire.generated";

describe("native backlog edit transport", () => {
  it("keeps item scope in the path, command identity in the header and receipt reads separate", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(goWireFixtures.workbenchWriteCapabilities)).mockResolvedValueOnce(Response.json(goWireFixtures.workbenchCommand)).mockResolvedValueOnce(Response.json(goWireFixtures.workbenchCommand));
    const client = new HttpDaemonClient({ fetch: fetcher });
    await client.getWorkbenchWriteCapabilities("team space", "issues/source");
    await client.patchWorkbenchItem("team space", "issues/source", "42", "exact-request-key", goWireFixtures.workbenchPatch);
    await client.getWorkbenchCommand("team space", "issues/source", goWireFixtures.workbenchCommand.id);
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual([
      "/api/v1/gaggles/team%20space/workbench/sources/issues%2Fsource/write-capabilities",
      "/api/v1/gaggles/team%20space/workbench/sources/issues%2Fsource/items/42",
      `/api/v1/gaggles/team%20space/workbench/sources/issues%2Fsource/commands/${goWireFixtures.workbenchCommand.id}`,
    ]);
    const request = fetcher.mock.calls[1][1];
    expect(request?.method).toBe("PATCH"); expect(request?.headers).toMatchObject({ "Idempotency-Key": "exact-request-key", "Content-Type": "application/json" });
    expect(JSON.parse(request?.body as string)).toEqual(goWireFixtures.workbenchPatch);
    expect(JSON.parse(request?.body as string)).not.toHaveProperty("id");
    expect(fetcher.mock.calls[2][1]?.body).toBeUndefined();
  });
  it("never automatically retries an ambiguous or overloaded write", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(Response.json({ error: { code: "class_saturated", message: "Try later." } }, { status: 503, headers: { "Retry-After": "0" } }));
    const client = new HttpDaemonClient({ fetch: fetcher });
    await expect(client.patchWorkbenchItem("team", "issues", "42", "one", goWireFixtures.workbenchPatch)).rejects.toThrow();
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});
