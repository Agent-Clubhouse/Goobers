import { describe, expect, it, vi } from "vitest";
import { HttpDaemonClient } from "./httpClient";
import { goWireFixtures } from "./wire.generated";
import { readMetadataResponse } from "./workbenchProposalTransport";

describe("governed source proposal transport", () => {
  it("keeps authority in transport and separates preview, submission, observation and continuation", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(goWireFixtures.metadataPreview));
    for (let i = 0; i < 4; i++) fetcher.mockResolvedValueOnce(Response.json(goWireFixtures.metadataProposal));
    const client = new HttpDaemonClient({ fetch: fetcher });
    const id = goWireFixtures.metadataProposal.id;
    await client.previewMetadataChange("team space", "strategy/source", goWireFixtures.metadataChange);
    await client.submitMetadataProposal("team space", "strategy/source", "same-key", goWireFixtures.metadataChange);
    await client.getMetadataProposal("team space", "strategy/source", id);
    await client.checkMetadataProposal("team space", "strategy/source", id);
    await client.continueMetadataProposal("team space", "strategy/source", id);
    const base = "/api/v1/gaggles/team%20space/workbench/sources/strategy%2Fsource";
    expect(fetcher.mock.calls.map(([url]) => url)).toEqual([`${base}/proposal-preview`, `${base}/proposals`, `${base}/proposals/${id}`, `${base}/proposals/${id}/check`, `${base}/proposals/${id}/continue`]);
    expect(fetcher.mock.calls[1][1]?.headers).toMatchObject({ "Idempotency-Key": "same-key" });
    expect(JSON.parse(fetcher.mock.calls[1][1]?.body as string)).toEqual(goWireFixtures.metadataChange);
    expect(fetcher.mock.calls[2][1]?.body).toBeUndefined();
    expect(fetcher.mock.calls[3][1]?.body).toBe("{}");
    expect(fetcher.mock.calls[4][1]?.body).toBe("{}");
  });
  it("never automatically replays a rejected or uncertain provider command", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(Response.json({ error: { code: "busy", message: "Inspect the receipt." } }, { status: 503, headers: { "Retry-After": "0" } }));
    const client = new HttpDaemonClient({ fetch: fetcher });
    await expect(client.submitMetadataProposal("team", "strategy", "one", goWireFixtures.metadataChange)).rejects.toThrow();
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
  it("accepts complete two-file previews and bounds escaped/streamed input and output", async () => {
    const large = { ...goWireFixtures.metadataPreview, before: "a".repeat(1 << 20), after: "b".repeat(1 << 20) };
    const fetcher = vi.fn<typeof fetch>().mockResolvedValueOnce(Response.json(large));
    const client = new HttpDaemonClient({ fetch: fetcher });
    expect((await client.previewMetadataChange("team", "strategy", goWireFixtures.metadataChange)).after.length).toBe(1 << 20);
    const cancelled = vi.fn();
    let calls = 0;
    const stream = new ReadableStream<Uint8Array>({ pull(controller) { calls++; controller.enqueue(new Uint8Array(600_000)); }, cancel: cancelled });
    await expect(readMetadataResponse(new Response(stream), 1 << 20)).rejects.toThrow("byte bound");
    expect(cancelled).toHaveBeenCalledTimes(1); expect(calls).toBeLessThanOrEqual(4);
    const input = { ...goWireFixtures.metadataChange, value: "\u0001".repeat(1 << 20) };
    await expect(client.submitMetadataProposal("team", "strategy", "one", input)).rejects.toThrow("request bound");
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
});
