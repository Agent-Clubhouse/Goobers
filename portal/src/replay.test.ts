import { describe, expect, it } from "vitest";
import type { RunEvent } from "./api/types";
import {
  compressedIdleDelayMs,
  formatReplayDuration,
  idleCompressionThresholdMs,
  orderedReplayEvents,
  replayTransition,
} from "./replay";

function ev(seq: number, time: string): RunEvent {
  return { schema: "v1", seq, type: "stage.started", branch: 0, time, knownSchema: true } as RunEvent;
}

describe("replay engine (live event stream)", () => {
  it("orders by durable sequence and rejects duplicates", () => {
    const events = [ev(3, "2026-01-01T00:00:03Z"), ev(1, "2026-01-01T00:00:01Z"), ev(2, "2026-01-01T00:00:02Z")];
    expect(orderedReplayEvents(events).map((e) => e.seq)).toEqual([1, 2, 3]);
    expect(() => orderedReplayEvents([ev(1, "2026-01-01T00:00:01Z"), ev(1, "2026-01-01T00:00:02Z")])).toThrow(
      /Duplicate durable event sequence/,
    );
  });

  it("derives real delay from the events' durable timestamps and scales by speed", () => {
    const events = [ev(1, "2026-01-01T00:00:00Z"), ev(2, "2026-01-01T00:00:02Z")];
    const at1x = replayTransition(events, 0, 1);
    expect(at1x?.realDelayMs).toBe(2_000);
    expect(at1x?.playbackDelayMs).toBe(2_000);
    expect(replayTransition(events, 0, 10)?.playbackDelayMs).toBe(200);
  });

  it("compresses long idle gaps to a fixed playback delay", () => {
    const start = Date.parse("2026-01-01T00:00:00Z");
    const gap = idleCompressionThresholdMs + 60_000;
    const events = [ev(1, "2026-01-01T00:00:00Z"), ev(2, new Date(start + gap).toISOString())];
    const transition = replayTransition(events, 0, 1);
    expect(transition?.idleCompressed).toBe(true);
    expect(transition?.playbackDelayMs).toBe(compressedIdleDelayMs);
  });

  it("returns undefined past the last event", () => {
    const events = [ev(1, "2026-01-01T00:00:00Z")];
    expect(replayTransition(events, 0, 1)).toBeUndefined();
  });

  it("formats playback durations", () => {
    expect(formatReplayDuration(450)).toBe("450ms");
    expect(formatReplayDuration(2_000)).toBe("2s");
    expect(formatReplayDuration(90_000)).toBe("1m 30s");
  });
});
