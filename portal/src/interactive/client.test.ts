import { describe, expect, it, vi } from "vitest";
import { HttpInteractiveClient, InteractiveError, mergeEvents, readPending, writePending, type InteractiveEvent } from "./client";

describe("interactive client", () => {
  it("preserves command IDs and fences across an explicit retry", async () => {
    const fetcher = vi.fn().mockRejectedValueOnce(new TypeError("lost response")).mockResolvedValue(new Response(JSON.stringify({ commandId: "same-intent", status: "accepted" })));
    const client = new HttpInteractiveClient("/api/v1/interactive", fetcher);
    const command = { commandId: "same-intent", type: "message" as const, text: "Inspect", expectedSequence: 18, expectedFence: 3 };
    await expect(client.command("session/a", command)).rejects.toMatchObject({ status: 0, code: "network" });
    await client.command("session/a", command);
    expect(fetcher.mock.calls[0][0]).toEqual(fetcher.mock.calls[1][0]);
    expect(fetcher.mock.calls[0][1].body).toBe(fetcher.mock.calls[1][1].body);
    expect(fetcher.mock.calls[1][0]).toBe("/api/v1/interactive/sessions/session%2Fa/commands");
    expect(fetcher.mock.calls[1][1]).toMatchObject({ credentials: "same-origin", body: JSON.stringify(command) });
  });
  it("keeps stale fence and authorization failures distinct from unknown outcomes", async () => {
    const client = new HttpInteractiveClient("", vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: {code: "stale_fence", message: "Execution changed"} }), {status: 409})));
    await expect(client.session("x")).rejects.toEqual(new InteractiveError("Execution changed", "stale_fence", 409));
  });
  it("deduplicates stream and replay by durable sequence without changing order", () => {
    const event = (sequence: number): InteractiveEvent => ({ sequence, sessionId: "a", type: "assistant.delta", time: "2026-10-17T00:00:00Z", data: {text: `${sequence}`} });
    expect(mergeEvents([event(3), event(1)], [event(3), event(2)]).map(item => item.sequence)).toEqual([1, 2, 3]);
  });
  it("restores an uncertain command across a new component lifecycle and clears only explicitly", () => {
    const pending = {commandId: "same-request", type: "message", text: "Investigate"};
    writePending("pending.session", pending);
    expect(readPending("pending.session")).toEqual(pending);
    writePending("pending.session", undefined);
    expect(readPending("pending.session")).toBeUndefined();
  });
});
