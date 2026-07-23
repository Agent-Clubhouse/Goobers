import { describe, expect, it } from "vitest";
import type { RunEvent, WorkflowGraph } from "./api/types";
import {
  deriveNodeStates,
  eventNodeAtSequence,
  eventSummary,
  orderRunEvents,
} from "./runDetailData";

const graph: WorkflowGraph = {
  name: "implementation",
  version: 7,
  digest: "sha256:pinned",
  start: "implement",
  nodes: [
    { id: "implement", kind: "agentic" },
    { id: "review", kind: "gate", evaluator: "agentic" },
  ],
  edges: [
    { source: "implement", target: "review" },
    { source: "review", target: "implement", outcome: "needs-changes" },
  ],
};

function event(seq: number, type: RunEvent["type"], fields: Partial<RunEvent>): RunEvent {
  return {
    schema: "v1",
    seq,
    type,
    branch: 0,
    time: `2026-07-18T00:00:${String(seq).padStart(2, "0")}Z`,
    knownSchema: true,
    ...fields,
  };
}

describe("run detail projection", () => {
  it("orders and derives state at a sequence without mutating source data", () => {
    const events = [
      event(4, "gate.evaluated", {
        gate: "review",
        verdict: "needs-changes",
        target: "implement",
      }),
      event(1, "stage.started", { stage: "implement", attempt: 1 }),
      event(3, "gate.started", { gate: "review", attempt: 1 }),
      event(2, "stage.finished", {
        stage: "implement",
        attempt: 1,
        status: "success",
      }),
      event(5, "stage.started", {
        stage: "implement",
        attempt: 2,
        attemptClass: "policy",
      }),
    ];
    const originalGraph = structuredClone(graph);
    const originalEvents = structuredClone(events);

    expect(orderRunEvents(events).map(({ seq }) => seq)).toEqual([1, 2, 3, 4, 5]);
    expect(deriveNodeStates(graph, events, 3)).toEqual({
      implement: "completed",
      review: "running",
    });
    expect(deriveNodeStates(graph, events, 5)).toEqual({
      implement: "running",
      review: "completed",
    });
    expect(eventNodeAtSequence(events, 4)).toBe("review");
    expect(eventNodeAtSequence([...events, event(6, "run.finished", { status: "failed" })], 6))
      .toBe("implement");
    expect(graph).toEqual(originalGraph);
    expect(events).toEqual(originalEvents);
  });

  it("retains unsupported schemas through safe generic presentation", () => {
    const unsupported = event(9, "future.recorded", {
      schema: "v2-preview",
      knownSchema: false,
      raw: { privateImplementationDetail: "not for summary" },
    });

    expect(eventSummary(unsupported)).toBe(
      "Schema v2-preview is not supported; future.recorded is retained with generic fields.",
    );
    expect(eventSummary(unsupported)).not.toContain("privateImplementationDetail");
  });

  it("applies an API-shaped terminal event to the previously active node and skips no-work nodes", () => {
    const events = [
      event(1, "stage.started", { stage: "implement", attempt: 1 }),
      event(2, "run.finished", { status: "aborted" }),
    ];

    // review was never entered before the run ended: it is a no-work node and
    // must read "skipped", not stay "pending" (DASH-19 regression guard).
    expect(deriveNodeStates(graph, events, 2)).toEqual({
      implement: "aborted",
      review: "skipped",
    });
  });

  it("keeps no-work nodes pending before terminal and skipped at/after it", () => {
    const events = [
      event(1, "stage.started", { stage: "implement", attempt: 1 }),
      event(2, "stage.finished", { stage: "implement", attempt: 1, status: "success" }),
      event(3, "run.finished", { status: "completed" }),
    ];

    // Before the run is terminal, an unvisited node may still run → pending.
    expect(deriveNodeStates(graph, events, 2)).toEqual({
      implement: "completed",
      review: "pending",
    });
    // As of the terminal sequence, the unvisited node is skipped and stays so.
    expect(deriveNodeStates(graph, events, 3)).toEqual({
      implement: "completed",
      review: "skipped",
    });
  });
});
