import { describe, expect, it } from "vitest";
import type { RunEvent, WorkflowGraph } from "./api/types";
import {
  deriveNodeStates,
  evidenceDecision,
  eventHeading,
  eventNodeAtSequence,
  eventSummary,
  journalEntries,
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

  it("groups adjacent supporting records by stage visit without changing event sequence", () => {
    const events = [
      event(1, "gate.started", {
        category: "bookkeeping",
        gate: "review",
        attempt: 1,
      }),
      event(2, "span.recorded", {
        category: "evidence",
        stage: "run-1:review",
        name: "reviewer.transcript",
      }),
      event(3, "artifact.recorded", {
        category: "evidence",
        artifact: {
          name: "verdict/review-1.json",
          digest: "sha256:verdict-1",
          size: 80,
          mediaType: "application/json",
          stage: "review",
          attempt: 1,
        },
      }),
      event(4, "gate.evaluated", {
        category: "decision",
        gate: "review",
        verdict: "needs-changes",
        target: "implement",
      }),
      event(5, "stage.started", {
        category: "transition",
        stage: "implement",
        attempt: 1,
        attemptClass: "initial",
      }),
      event(6, "stage.heartbeat", {
        category: "liveness",
        stage: "implement",
        attempt: 1,
      }),
      event(7, "stage.finished", {
        category: "transition",
        stage: "implement",
        attempt: 1,
        status: "success",
      }),
      event(8, "gate.started", {
        category: "bookkeeping",
        gate: "review",
        attempt: 1,
      }),
      event(9, "span.recorded", {
        category: "evidence",
        stage: "run-1:review",
        name: "reviewer.transcript",
      }),
      event(10, "artifact.recorded", {
        category: "evidence",
        artifact: {
          name: "verdict/review-2.json",
          digest: "sha256:verdict-2",
          size: 80,
          mediaType: "application/json",
          stage: "review",
          attempt: 1,
        },
      }),
      event(11, "gate.evaluated", {
        category: "decision",
        gate: "review",
        verdict: "pass",
        target: "@complete",
      }),
      event(12, "ref.touched", {
        category: "result",
        externalRef: {
          provider: "github",
          kind: "pr",
          id: "1432",
          url: "https://github.example/pull/1432",
        },
        runner: { operation: "open" },
      }),
      event(13, "future.recorded", {
        category: "unknown",
        schema: "v2-preview",
        knownSchema: false,
      }),
    ];

    const entries = journalEntries(events);
    const groups = entries.filter((entry) => entry.kind === "group");
    expect(
      groups.map((group) => ({
        seqs: group.events.map(({ seq }) => seq),
        nodeId: group.nodeId,
        visit: group.visit,
      })),
    ).toEqual([
      { seqs: [1, 2, 3], nodeId: "review", visit: 1 },
      { seqs: [6], nodeId: "implement", visit: 1 },
      { seqs: [8, 9, 10], nodeId: "review", visit: 2 },
    ]);
    expect(
      entries.flatMap((entry) =>
        entry.kind === "group" ? entry.events.map(({ seq }) => seq) : entry.event.seq,
      ),
    ).toEqual(events.map(({ seq }) => seq));
    expect(entries.at(-2)).toMatchObject({ kind: "event", event: { seq: 12 } });
    expect(entries.at(-1)).toMatchObject({ kind: "event", event: { seq: 13 } });
  });

  it("summarizes reviewer evidence, heartbeats, and external operations", () => {
    const transcript = event(2, "span.recorded", {
      stage: "run-1:review",
      name: "reviewer.transcript",
    });
    const verdict = event(3, "artifact.recorded", {
      artifact: {
        name: "verdict/review-1.json",
        digest: "sha256:verdict",
        size: 80,
        mediaType: "application/json",
        stage: "review",
        attempt: 1,
      },
    });
    const heartbeat = event(4, "stage.heartbeat", {
      stage: "implement",
      attempt: 1,
    });
    const ref = event(5, "ref.touched", {
      externalRef: { provider: "github", kind: "pr", id: "42" },
      runner: { operation: "open" },
    });
    const decision = event(6, "gate.evaluated", {
      gate: "review",
      verdict: "needs-changes",
      target: "implement",
    });

    expect(eventHeading(transcript)).toBe("Transcript recorded");
    expect(eventSummary(transcript)).toBe(
      "Transcript for Review was recorded. Select this event to inspect the associated attempt.",
    );
    expect(eventHeading(verdict)).toBe("Structured verdict recorded");
    expect(eventSummary(verdict)).toContain(
      "verdict/review-1.json was recorded for Review.",
    );
    expect(
      eventSummary(verdict, evidenceDecision([verdict, decision], verdict)),
    ).toBe(
      "verdict/review-1.json captured the Review decision: needs-changes selecting implement. Select this event to inspect the artifact.",
    );
    expect(eventNodeAtSequence([verdict], verdict.seq)).toBe("review");
    expect(eventSummary(heartbeat)).toBe(
      "Implement attempt 1 reported liveness; workflow state did not change.",
    );
    expect(eventSummary(ref)).toBe("GitHub opened pull request #42.");
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
