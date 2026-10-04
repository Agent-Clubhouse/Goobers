import { describe, expect, it, vi } from "vitest";
import { HttpDaemonClient } from "./httpClient";
import { goWireFixtures as f } from "./wire.generated";

describe("suggestion review client", () => {
  it("uses bounded selection paths and transmits only exact reviewed decision inputs", async () => {
    const fetcher = vi.fn<typeof fetch>()
      .mockResolvedValueOnce(Response.json(f.suggestionInventory))
      .mockResolvedValueOnce(Response.json(f.suggestionBatch))
      .mockResolvedValueOnce(Response.json(f.suggestionPreview))
      .mockResolvedValueOnce(Response.json(f.suggestionReview))
      .mockResolvedValueOnce(Response.json(f.suggestionReview));
    const client = new HttpDaemonClient({ fetch: fetcher });
    await client.listSuggestionArtifacts("team space", f.suggestionSelection.runId, 2);
    await client.loadSuggestions("team space", f.suggestionSelection);
    await client.previewSuggestion("team space", f.suggestionPreviewRequest);
    await client.decideSuggestion("team space", f.suggestionDecision);
    await client.getSuggestionReview("team space", f.suggestionReview.id);
    const root = "/api/v1/gaggles/team%20space/workbench/suggestions";
    expect(String(fetcher.mock.calls[0][0])).toContain(`${root}/artifacts/${f.suggestionSelection.runId}?after=2`);
    expect(String(fetcher.mock.calls[1][0])).toContain(`${root}/artifacts/${f.suggestionSelection.runId}/3`);
    expect(String(fetcher.mock.calls[4][0])).toContain(`${root}/reviews/${f.suggestionReview.id}`);
    expect(JSON.parse(String(fetcher.mock.calls[2][1]?.body))).toEqual(f.suggestionPreviewRequest);
    expect(JSON.parse(String(fetcher.mock.calls[3][1]?.body))).toEqual(f.suggestionDecision);
    expect(fetcher.mock.calls[3][1]?.method).toBe("POST");
  });
  it("does not retry a lost decision or accept an oversized response", async () => {
    const fetcher = vi.fn<typeof fetch>().mockRejectedValueOnce(new TypeError("lost"));
    const client = new HttpDaemonClient({ fetch: fetcher });
    await expect(client.decideSuggestion("web", f.suggestionDecision)).rejects.toThrow();
    expect(fetcher).toHaveBeenCalledTimes(1);
    fetcher.mockResolvedValueOnce(new Response("x".repeat((1 << 20) + 1)));
    await expect(client.getSuggestionReview("web", f.suggestionReview.id)).rejects.toThrow();
    expect(fetcher).toHaveBeenCalledTimes(2);
  });
});
