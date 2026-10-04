import { describe, expect, it, vi } from "vitest";
import { HttpDaemonClient } from "./httpClient";
import { goWireFixtures } from "./wire.generated";

describe("post-turn repair observation transport", () => {
  it("sends only retained identity and explicit empty check, preserving producer and checker", async () => {
    const fetcher = vi.fn<typeof fetch>().mockImplementation(async () => Response.json(goWireFixtures.prRepairCommand));
    const client = new HttpDaemonClient({ fetch: fetcher }); const id = goWireFixtures.prRepairCommand.id;
    expect(await client.getPRRepairCommand("team space", id)).toEqual(goWireFixtures.prRepairCommand);
    expect(await client.checkPRRepairCommand("team space", id)).toEqual(goWireFixtures.prRepairCommand);
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual([`/api/v1/gaggles/team%20space/pr-repairs/${id}`, `/api/v1/gaggles/team%20space/pr-repairs/${id}/check`]);
    expect(fetcher.mock.calls[0][1]?.body).toBeUndefined();
    expect(fetcher.mock.calls[1][1]?.body).toBe("{}");
    expect(fetcher.mock.calls[1][1]?.method).toBe("POST");
  });
  it("bounds evidence and does not automatically repeat an uncertain check", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json({ padding: "x".repeat(128 << 10) })).mockResolvedValue(Response.json({ error: { code: "busy", message: "Inspect later." } }, { status: 503, headers: { "Retry-After": "0" } }));
    const client = new HttpDaemonClient({ fetch: fetcher }); const id = goWireFixtures.prRepairCommand.id;
    await expect(client.getPRRepairCommand("team", id)).rejects.toThrow("byte bound");
    await expect(client.checkPRRepairCommand("team", id)).rejects.toThrow();
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
});
