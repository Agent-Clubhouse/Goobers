import { afterEach, describe, expect, it, vi } from "vitest";
import { GuidedClient } from "./client";

function neverSettles(): (input: string, init?: RequestInit) => Promise<Response> {
  return (_input, init) =>
    new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener(
        "abort",
        () => reject(new DOMException("The operation was aborted.", "AbortError")),
        { once: true },
      );
    });
}

describe("GuidedClient", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("fails a timed-out read with the path and the deadline", async () => {
    vi.useFakeTimers();
    const client = new GuidedClient(neverSettles());

    const state = client.getState({ timeoutMs: 25 });
    const assertion = expect(state).rejects.toMatchObject({
      name: "GuidedRequestTimeoutError",
      path: "/guided/state",
      timeoutMs: 25,
      message: "/guided/state timed out after 25ms",
    });
    await vi.advanceTimersByTimeAsync(25);
    await assertion;
  });

  it("cancels a read when the caller's scope aborts", async () => {
    const controller = new AbortController();
    const client = new GuidedClient(neverSettles());

    const state = client.getState({ signal: controller.signal });
    controller.abort();

    await expect(state).rejects.toMatchObject({
      name: "GuidedRequestCancelledError",
      path: "/guided/state",
    });
  });

  it("never issues a request for an already-abandoned scope", async () => {
    const fetchFn = vi.fn(neverSettles());
    const controller = new AbortController();
    controller.abort();

    await expect(
      new GuidedClient(fetchFn).getJob("job-1", { signal: controller.signal }),
    ).rejects.toMatchObject({
      name: "GuidedRequestCancelledError",
      path: "/guided/jobs/job-1",
    });
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it("leaves action requests without a deadline", async () => {
    const fetchFn = vi.fn(
      async (_input: string, _init?: RequestInit) =>
        new Response(JSON.stringify({ exitCode: 0, stdout: "", stderr: "" }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
    );

    await new GuidedClient(fetchFn).initInstance({ template: "guided" });

    expect(fetchFn.mock.calls[0][1]?.signal).toBeUndefined();
  });

  it("rejects a non-positive deadline", async () => {
    await expect(new GuidedClient(neverSettles()).getState({ timeoutMs: 0 })).rejects.toBeInstanceOf(
      RangeError,
    );
  });
});
