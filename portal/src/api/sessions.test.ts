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


it("retains the exact human-selected PR as typed message content", async () => {
  const fetcher = vi.fn<typeof fetch>().mockResolvedValue(Response.json(goWireFixtures.sessionAccepted));
  const client = new HttpDaemonClient({ fetch: fetcher });
  const repairTarget = { sourceBindingId: "code", repository: { provider: "github" as const, owner: "org", name: "repo" }, repositorySourceId: "100", id: "12", sourceId: "900", expectedHeadSha: "a".repeat(40) };
  await client.sendSessionMessage("team", "session-one", "same-key", { text: "Repair this", repairTarget });
  expect(JSON.parse(String(fetcher.mock.calls[0][1]?.body))).toEqual({ text: "Repair this", repairTarget });
  expect(new Headers(fetcher.mock.calls[0][1]?.headers).get("Idempotency-Key")).toBe("same-key");
});

it("inspects a selected repository PR through an encoded read-only path", async () => {
  const fetcher = vi.fn<typeof fetch>().mockResolvedValue(Response.json({}));
  await new HttpDaemonClient({ fetch: fetcher }).inspectPullRequest("team space", "code/source", "12");
  expect(fetcher).toHaveBeenCalledWith("/api/v1/gaggles/team%20space/workbench/sources/code%2Fsource/pull-requests/12", expect.objectContaining({ method: "GET" }));
});
