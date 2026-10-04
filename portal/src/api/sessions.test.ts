import { describe, expect, it, vi } from "vitest";
import { HttpDaemonClient } from "./httpClient";
import { goWireFixtures } from "./wire.generated";

describe("shared session transport", () => {
  it("uses encoded gaggle/session paths, bounded cursors and no body-supplied authority", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(Response.json(goWireFixtures.sessionAccepted));
    const client = new HttpDaemonClient({ fetch: fetcher });
    await client.sendSessionMessage("team space", "session/one", "same-command", { text: "Scope this" });
    expect(fetcher).toHaveBeenCalledWith("/api/v1/gaggles/team%20space/sessions/session%2Fone/messages", expect.objectContaining({ method: "POST", body: JSON.stringify({ text: "Scope this" }) }));
    const init = fetcher.mock.calls[0][1]!;
    expect(new Headers(init.headers).get("Idempotency-Key")).toBe("same-command");
    fetcher.mockResolvedValueOnce(Response.json(goWireFixtures.sessionMessages));
    await client.getSessionMessages("team", "session-one", 12);
    expect(fetcher).toHaveBeenLastCalledWith("/api/v1/gaggles/team/sessions/session-one/messages?after=12", expect.objectContaining({ method: "GET" }));
  });
});
