import { afterEach, expect, it, vi } from "vitest";
import { subscribeHumanStream } from "./humanStream";

afterEach(() => localStorage.clear());

it("streams split SSE frames with the origin credential in a header, never the URL", async () => {
  localStorage.setItem("goobers.interactive.human-binding", "fixture-binding");
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(encoder.encode('event: interactive\ndata: {"sequence":1}\n\nevent: inter'));
      controller.enqueue(encoder.encode('active\ndata: {"sequence":2}\n\n'));
      controller.close();
    },
  });
  const fetcher = vi.fn().mockResolvedValue(new Response(body, { headers: { "Content-Type": "text/event-stream" } }));
  const events = vi.fn();
  const states = vi.fn();
  const stream = subscribeHumanStream("/api/events", 0, fetcher, events, states);
  try {
    await vi.waitFor(() => expect(events).toHaveBeenCalledTimes(2));
    expect(fetcher.mock.calls[0]).toMatchObject([
      "/api/events?after=0", { headers: { "X-Goobers-CSRF": "fixture-binding" }, redirect: "error" },
    ]);
    expect(states).toHaveBeenCalledWith("live");
    expect(events.mock.calls.map(call => call[0].sequence)).toEqual([1, 2]);
  } finally { stream.close(); }
});
