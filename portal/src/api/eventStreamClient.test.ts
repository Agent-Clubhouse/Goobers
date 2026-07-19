import { describe, expect, it, vi } from "vitest";
import { DaemonApiError, MalformedResponseError } from "./errors";
import { HttpDaemonClient } from "./httpClient";

describe("HttpDaemonClient event stream", () => {
  it("sends a resume cursor and parses split, typed SSE events", async () => {
    const encoder = new TextEncoder();
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(
      new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(encoder.encode("id: session:8\nevent: inval"));
            controller.enqueue(
              encoder.encode(
                'idate\ndata: {"cursor":"session:8","models":["run"],"runIds":["run-1"]}\n\n',
              ),
            );
            controller.enqueue(
              encoder.encode(
                'event: heartbeat\ndata: {"cursor":"session:8","models":null}\n\n',
              ),
            );
            controller.close();
          },
        }),
        { headers: { "Content-Type": "text/event-stream; charset=utf-8" } },
      ),
    );
    const client = new HttpDaemonClient({ fetch: fetcher });

    const stream = await client.connectEvents({ cursor: "session:7" });
    const events = [];
    for await (const event of stream) {
      events.push(event);
    }

    const request = fetcher.mock.calls[0];
    expect(request?.[0]).toBe("/api/v1/events");
    expect(new Headers(request?.[1]?.headers).get("Last-Event-ID")).toBe("session:7");
    expect(events).toEqual([
      {
        id: "session:8",
        type: "invalidate",
        data: { cursor: "session:8", models: ["run"], runIds: ["run-1"] },
      },
      { type: "heartbeat", data: { cursor: "session:8" } },
    ]);
  });

  it("surfaces stale cursors before opening a stream", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(
      Response.json(
        { error: { code: "stale_cursor", message: "event history expired" } },
        { status: 409 },
      ),
    );

    await expect(
      new HttpDaemonClient({ fetch: fetcher }).connectEvents({ cursor: "old:4" }),
    ).rejects.toMatchObject({
      code: "stale_cursor",
      status: 409,
    } satisfies Partial<DaemonApiError>);
  });

  it("fails closed on malformed event payloads", async () => {
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(
      new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(
              new TextEncoder().encode(
                'id: session:2\nevent: invalidate\ndata: {"cursor":"other:2","models":["run"]}\n\n',
              ),
            );
            controller.close();
          },
        }),
        { headers: { "Content-Type": "text/event-stream" } },
      ),
    );
    const stream = await new HttpDaemonClient({ fetch: fetcher }).connectEvents();

    await expect(stream[Symbol.asyncIterator]().next()).rejects.toBeInstanceOf(
      MalformedResponseError,
    );
  });

  it("cancels the response body when the consumer closes the stream", async () => {
    const cancel = vi.fn();
    const fetcher = vi.fn<typeof fetch>().mockResolvedValue(
      new Response(new ReadableStream({ cancel }), {
        headers: { "Content-Type": "text/event-stream" },
      }),
    );
    const stream = await new HttpDaemonClient({ fetch: fetcher }).connectEvents();

    stream.close();

    await vi.waitFor(() => expect(cancel).toHaveBeenCalledOnce());
  });
});
