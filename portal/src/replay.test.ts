import { describe, expect, it } from "vitest";
import type { RunEvent, WorkflowGraph } from "./api/types";
import {
  compressedIdleDelayMs,
  formatReplayClock,
  formatReplayDuration,
  idleCompressionThresholdMs,
  orderedReplayEvents,
  replayChapterKind,
  replayTimeline,
  replayTransition,
} from "./replay";

function ev(
  seq: number,
  time: string,
  fields: Partial<RunEvent> = {},
): RunEvent {
  return {
    schema: "v1",
    seq,
    type: "stage.started",
    branch: 0,
    time,
    knownSchema: true,
    ...fields,
  };
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

  it("positions events and chapters by deterministic compressed elapsed time", () => {
    const events = [
      ev(1, "2026-01-01T00:00:00Z", {
        type: "run.started",
        category: "transition",
        replayChapter: true,
      }),
      ev(2, "2026-01-01T00:00:02Z", {
        type: "artifact.recorded",
        category: "evidence",
        replayChapter: false,
      }),
      ev(3, "2026-01-01T00:01:02Z", {
        type: "gate.evaluated",
        category: "decision",
        replayChapter: true,
      }),
    ];
    const timeline = replayTimeline(events);

    expect(timeline.compressedDurationMs).toBe(2_000 + compressedIdleDelayMs);
    expect(timeline.points.map((point) => point.percent)).toEqual([
      0,
      (2_000 / 3_500) * 100,
      100,
    ]);
    expect(timeline.chapters.map((chapter) => chapter.event.seq)).toEqual([1, 3]);
    expect(timeline.idleGaps).toEqual([
      expect.objectContaining({
        fromSeq: 2,
        toSeq: 3,
        startPercent: (2_000 / 3_500) * 100,
        endPercent: 100,
      }),
    ]);
  });

  it("groups the timeline into stage segments attributed to their owning goober (#2538)", () => {
    const graph: WorkflowGraph = {
      name: "implementation",
      version: 1,
      digest: "sha256:graph",
      start: "query",
      nodes: [
        { id: "query", kind: "deterministic" },
        { id: "implement", kind: "agentic", owner: "builder-goober" },
      ],
      edges: [],
    };
    const events = [
      ev(1, "2026-01-01T00:00:00Z", { type: "stage.started", stage: "query" }),
      ev(2, "2026-01-01T00:00:01Z", { type: "stage.finished", stage: "query", status: "success" }),
      ev(3, "2026-01-01T00:00:02Z", { type: "stage.started", stage: "implement" }),
      ev(4, "2026-01-01T00:00:03Z", {
        type: "artifact.recorded",
        category: "evidence",
        replayChapter: false,
      }),
    ];

    const timeline = replayTimeline(events, graph, "run-1");

    expect(timeline.stageSegments).toEqual([
      expect.objectContaining({
        stageId: "query",
        label: "query",
        owner: undefined,
        colorIndex: 0,
      }),
      expect.objectContaining({
        stageId: "implement",
        label: "implement",
        owner: "builder-goober",
        colorIndex: 1,
      }),
    ]);
    // Event 4 carries no stage of its own; it stays attributed to the last
    // known stage rather than opening an unscoped, ownerless segment.
    expect(timeline.stageSegments[1].endPercent).toBe(100);
  });

  it("keeps a repassed stage's color stable across its separate segments (#2538)", () => {
    const events = [
      ev(1, "2026-01-01T00:00:00Z", { type: "stage.started", stage: "implement" }),
      ev(2, "2026-01-01T00:00:01Z", { type: "stage.started", stage: "review" }),
      ev(3, "2026-01-01T00:00:02Z", { type: "stage.started", stage: "implement" }),
    ];

    const timeline = replayTimeline(events);

    expect(timeline.stageSegments.map((segment) => segment.stageId)).toEqual([
      "implement",
      "review",
      "implement",
    ]);
    expect(timeline.stageSegments[0].colorIndex).toBe(0);
    expect(timeline.stageSegments[1].colorIndex).toBe(1);
    // The second "implement" segment (a repass) reuses stage 0's color rather
    // than taking whatever color its list position would land on.
    expect(timeline.stageSegments[2].colorIndex).toBe(0);
  });

  it("assigns distinct landmark kinds to meaningful chapters", () => {
    expect(
      replayChapterKind(
        ev(1, "2026-01-01T00:00:00Z", {
          type: "ref.touched",
          externalRef: { provider: "github", kind: "pr", id: "42" },
        }),
      ),
    ).toBe("external");
    expect(
      replayChapterKind(
        ev(2, "2026-01-01T00:00:01Z", {
          type: "gate.evaluated",
          category: "decision",
          target: "@escalate",
          escalated: true,
        }),
      ),
    ).toBe("escalation");
    expect(
      replayChapterKind(
        ev(3, "2026-01-01T00:00:02Z", {
          type: "stage.finished",
          status: "failure",
        }),
      ),
    ).toBe("failure");
    expect(
      replayChapterKind(
        ev(4, "2026-01-01T00:00:03Z", {
          type: "run.finished",
          status: "failed",
        }),
      ),
    ).toBe("terminal");
  });

  it("uses the authoritative escalation flag instead of target naming", () => {
    expect(
      replayChapterKind(
        ev(1, "2026-01-01T00:00:00Z", {
          type: "gate.evaluated",
          category: "decision",
          target: "@human-review",
          escalated: true,
        }),
      ),
    ).toBe("escalation");
    expect(
      replayChapterKind(
        ev(2, "2026-01-01T00:00:01Z", {
          type: "gate.evaluated",
          category: "decision",
          target: "@escalate-if-needed",
          escalated: false,
        }),
      ),
    ).toBe("decision");
  });

  it("returns undefined past the last event", () => {
    const events = [ev(1, "2026-01-01T00:00:00Z")];
    expect(replayTransition(events, 0, 1)).toBeUndefined();
  });

  it("formats playback durations", () => {
    expect(formatReplayDuration(450)).toBe("450ms");
    expect(formatReplayDuration(2_000)).toBe("2s");
    expect(formatReplayDuration(90_000)).toBe("1m 30s");
    expect(formatReplayClock(0)).toBe("0:00");
    expect(formatReplayClock(65_000)).toBe("1:05");
    expect(formatReplayClock(3_665_000)).toBe("1:01:05");
  });
});
