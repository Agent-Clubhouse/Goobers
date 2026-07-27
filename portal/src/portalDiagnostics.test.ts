import { describe, expect, it, vi } from "vitest";
import { createPortalDiagnostics } from "./portalDiagnostics";

describe("portal diagnostics", () => {
  it("stays entirely disabled unless explicitly opted in", () => {
    const debug = vi.fn();
    const now = vi.fn(() => {
      throw new Error("disabled diagnostics must not read the clock");
    });

    expect(createPortalDiagnostics({ search: "", debug, now })).toBeUndefined();
    expect(debug).not.toHaveBeenCalled();
    expect(now).not.toHaveBeenCalled();
  });

  it("logs request timings and overlapping request bursts with the initiating page", () => {
    const debug = vi.fn();
    const times = [0, 5, 20, 30];
    const diagnostics = createPortalDiagnostics({
      search: "?portal-diagnostics=1",
      debug,
      now: () => times.shift() ?? 0,
      timestamp: () => "2026-07-26T19:00:00.000Z",
      page: () => "#/runs",
    });

    const first = diagnostics?.startRequest({ endpoint: "/api/v1/runs", method: "GET" });
    const second = diagnostics?.startRequest({ endpoint: "/api/v1/health", method: "GET" });
    first?.finish(200);
    second?.finish("timeout");

    expect(debug.mock.calls.map((call) => call[1])).toEqual([
      {
        type: "request",
        timestamp: "2026-07-26T19:00:00.000Z",
        endpoint: "/api/v1/runs",
        method: "GET",
        status: 200,
        durationMs: 20,
        initiatedBy: "#/runs",
      },
      {
        type: "request",
        timestamp: "2026-07-26T19:00:00.000Z",
        endpoint: "/api/v1/health",
        method: "GET",
        status: "timeout",
        durationMs: 25,
        initiatedBy: "#/runs",
      },
      {
        type: "request-burst",
        timestamp: "2026-07-26T19:00:00.000Z",
        count: 2,
        totalElapsedMs: 30,
        initiatedBy: "#/runs",
      },
    ]);
  });

  it("does not report a lone request as a concurrent burst", () => {
    const debug = vi.fn();
    const times = [10, 25];
    const diagnostics = createPortalDiagnostics({
      search: "?portal-diagnostics=1",
      debug,
      now: () => times.shift() ?? 0,
      timestamp: () => "2026-07-26T19:00:00.000Z",
      page: () => "#/runs",
    });

    diagnostics?.startRequest({ endpoint: "/api/v1/runs", method: "GET" }).finish(200);

    expect(debug).toHaveBeenCalledOnce();
    expect(debug.mock.calls[0]?.[1]).toMatchObject({
      type: "request",
      endpoint: "/api/v1/runs",
    });
  });

  it("logs SSE lifecycle events with their causes", () => {
    const debug = vi.fn();
    const diagnostics = createPortalDiagnostics({
      search: "?portal-diagnostics=1",
      debug,
      timestamp: () => "2026-07-26T19:00:00.000Z",
    });

    diagnostics?.recordSSE({ event: "reconnect", cause: "stream-error", delayMs: 250 });

    expect(debug).toHaveBeenCalledWith("[goobers portal diagnostics]", {
      type: "sse",
      timestamp: "2026-07-26T19:00:00.000Z",
      event: "reconnect",
      cause: "stream-error",
      delayMs: 250,
    });
  });
});
