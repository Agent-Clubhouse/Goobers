import { describe, expect, it } from "vitest";
import type { RunEvent, RunTransition, WorkflowGraph } from "./api/types";
import {
  branchStateLabel,
  deriveBranchStates,
  deriveNodeStates,
  deriveTraversedEdges,
  edgeTraversed,
  evidenceVisit,
  evidenceDecision,
  eventHeading,
  eventNodeAtSequence,
  eventNodeId,
  eventStage,
  eventSummary,
  isFailureJournalEvent,
  journalEntries,
  keyMomentEvidence,
  keyMomentLabel,
  keyMoments,
  orderRunEvents,
  runEventStages,
  UNSCOPED_EVENT_STAGE,
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

  it("names a placement's node when one is known and its host otherwise (#3515)", () => {
    const onANode = event(9, "runner.placement", {
      stage: "implement",
      runner: {
        runner: "linux-large",
        node: "aks-linux-0001",
        host: "goobers-stage-implement-4x2vq",
        os: "linux",
        pod: "goobers-stage-implement-4x2vq",
      },
    });
    const summary = eventSummary(onANode, undefined, "run-1");
    expect(summary).toContain("node aks-linux-0001");
    // The pod name must never also be presented as an unlabelled location.
    expect(summary).not.toContain("host goobers-stage-implement-4x2vq");

    // A local attempt knows no node. Its hostname is reported as a host, never
    // promoted to a node it is not.
    const local = event(10, "runner.placement", {
      stage: "implement",
      runner: { runner: "self", host: "build-box-07", os: "darwin" },
    });
    const localSummary = eventSummary(local, undefined, "run-1");
    expect(localSummary).toContain("host build-box-07");
    expect(localSummary).not.toContain("node ");
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

  it("decodes only verified run prefixes from transcript stages", () => {
    const nodeId = "review:security";

    expect(eventNodeId(event(1, "stage.started", { stage: nodeId }))).toBe(nodeId);
    expect(eventNodeId(event(2, "gate.evaluated", { gate: nodeId }))).toBe(nodeId);
    expect(eventNodeId(event(3, "artifact.recorded", {
      artifact: {
        digest: "sha256:colon",
        size: 1,
        mediaType: "application/json",
        stage: nodeId,
      },
    }))).toBe(nodeId);
    expect(eventNodeId(event(4, "span.recorded", {
      stage: nodeId,
      name: "reviewer.transcript",
    }), "run-1")).toBe(nodeId);
    expect(eventNodeId(event(5, "span.recorded", {
      stage: `run-1:${nodeId}`,
      name: "reviewer.transcript",
    }), "run-1")).toBe(nodeId);
    expect(eventNodeId(event(6, "span.recorded", {
      stage: `run-1:${nodeId}`,
      name: "reviewer.transcript",
    }), "run-2")).toBe(`run-1:${nodeId}`);
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

    const entries = journalEntries(events, "run-1");
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
    expect(evidenceVisit(events, events[1], "run-1")).toBe(1);
    expect(evidenceVisit(events, events[8], "run-1")).toBe(2);
  });

  it("associates human-rerun evidence with canonical stage visits", () => {
    const events = [
      event(1, "stage.started", {
        category: "transition",
        stage: "implement",
        attempt: 1,
      }),
      event(2, "artifact.recorded", {
        category: "evidence",
        artifact: {
          digest: "sha256:initial",
          size: 1,
          mediaType: "application/json",
          stage: "implement",
          attempt: 1,
        },
      }),
      event(3, "stage.finished", {
        category: "transition",
        stage: "implement",
        attempt: 1,
        status: "success",
      }),
      event(4, "stage.rerun.requested", {
        category: "decision",
        stage: "implement",
        attempt: 2,
        attemptClass: "human",
      }),
      event(5, "stage.started", {
        category: "transition",
        stage: "implement",
        attempt: 2,
        attemptClass: "human",
      }),
      event(6, "artifact.recorded", {
        category: "evidence",
        artifact: {
          digest: "sha256:human-1",
          size: 1,
          mediaType: "application/json",
          stage: "implement",
          attempt: 2,
          attemptClass: "human",
        },
      }),
      event(7, "stage.finished", {
        category: "transition",
        stage: "implement",
        attempt: 2,
        attemptClass: "human",
        status: "failure",
      }),
      event(8, "stage.started", {
        category: "transition",
        stage: "implement",
        attempt: 3,
        attemptClass: "human",
      }),
      event(9, "stage.heartbeat", {
        category: "liveness",
        stage: "implement",
        attempt: 3,
        attemptClass: "human",
      }),
      event(10, "stage.finished", {
        category: "transition",
        stage: "implement",
        attempt: 3,
        attemptClass: "human",
        status: "success",
      }),
      event(11, "stage.rerun.requested", {
        category: "decision",
        stage: "implement",
        attempt: 4,
        attemptClass: "human",
      }),
      event(12, "stage.started", {
        category: "transition",
        stage: "implement",
        attempt: 4,
        attemptClass: "human",
      }),
      event(13, "span.recorded", {
        category: "evidence",
        stage: "run-1:implement",
        name: "reviewer.transcript",
      }),
    ];

    expect(evidenceVisit(events, events[1], "run-1")).toBe(1);
    expect(evidenceVisit(events, events[5], "run-1")).toBe(2);
    expect(evidenceVisit(events, events[8], "run-1")).toBe(2);
    expect(evidenceVisit(events, events[12], "run-1")).toBe(3);
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
    expect(eventSummary(transcript, undefined, "run-1")).toBe(
      "Transcript for Review was recorded. Select this event to inspect the evidence.",
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

  it("classifies and describes parallel/branch lifecycle events readably (#1567)", () => {
    const parallelGraph: WorkflowGraph = {
      ...graph,
      nodes: [...graph.nodes, { id: "fanout", kind: "parallel" }],
    };
    const started = event(3, "parallel.started", { parallel: "fanout" });
    const branchStarted = event(4, "branch.started", {
      branch: 1,
      parallel: "fanout",
      branchName: "security-lens",
    });
    const branchFinished = event(9, "branch.finished", {
      branch: 1,
      parallel: "fanout",
      branchName: "security-lens",
      branchStatus: "failed",
    });
    const finished = event(14, "parallel.finished", {
      parallel: "fanout",
      completeness: [
        { branch: 1, name: "security-lens", status: "failed", artifacts: 0 },
        { branch: 2, name: "perf-lens", status: "succeeded", artifacts: 1 },
      ],
    });

    expect(eventHeading(started)).toBe("Parallel started");
    expect(eventSummary(started)).toBe("Parallel fanout started.");
    expect(eventHeading(branchStarted)).toBe("Branch started");
    expect(eventSummary(branchStarted)).toBe("Branch security-lens started.");
    expect(eventHeading(branchFinished)).toBe("Branch finished");
    expect(eventSummary(branchFinished)).toBe("Branch security-lens finished as failed.");
    expect(eventHeading(finished)).toBe("Parallel finished");
    expect(eventSummary(finished)).toBe(
      "Parallel fanout finished — 1/2 branches succeeded (security-lens: failed, perf-lens: succeeded).",
    );

    // eventNodeId: the parallel container is its own graph node; a branch
    // event has no single node of its own.
    expect(eventNodeId(started)).toBe("fanout");
    expect(eventNodeId(finished)).toBe("fanout");
    expect(eventNodeId(branchStarted)).toBeUndefined();

    // deriveNodeStates: the container tracks "did execution reach the join",
    // not what any individual branch did.
    expect(deriveNodeStates(parallelGraph, [started], 3).fanout).toBe("running");
    expect(deriveNodeStates(parallelGraph, [started, finished], 14).fanout).toBe("completed");
  });

  it("correlates unscoped verdict evidence within its parallel branch", () => {
    const reviewSecurity = event(1, "gate.started", {
      branch: 1,
      gate: "review:security",
      attempt: 1,
    });
    const reviewQuality = event(2, "gate.started", {
      branch: 2,
      gate: "review:quality",
      attempt: 1,
    });
    const securityVerdict = event(3, "artifact.recorded", {
      branch: 1,
      artifact: {
        name: "verdict/security.json",
        digest: "sha256:security",
        size: 80,
        mediaType: "application/json",
      },
    });
    const qualityVerdict = event(4, "artifact.recorded", {
      branch: 2,
      artifact: {
        name: "verdict/quality.json",
        digest: "sha256:quality",
        size: 80,
        mediaType: "application/json",
      },
    });
    const qualityDecision = event(5, "gate.evaluated", {
      branch: 2,
      gate: "review:quality",
      verdict: "pass",
      target: "@complete",
    });
    const securityDecision = event(6, "gate.evaluated", {
      branch: 1,
      gate: "review:security",
      verdict: "needs-changes",
      target: "implement",
    });
    const events = [
      reviewSecurity,
      reviewQuality,
      securityVerdict,
      qualityVerdict,
      qualityDecision,
      securityDecision,
    ];

    expect(eventNodeAtSequence(events, securityVerdict.seq)).toBe("review:security");
    expect(evidenceDecision(events, securityVerdict)).toBe(securityDecision);
    expect(evidenceDecision(events, qualityVerdict)).toBe(qualityDecision);
    expect(
      eventSummary(
        securityVerdict,
        evidenceDecision(events, securityVerdict),
      ),
    ).toContain("Review:security decision: needs-changes");
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

function transition(
  seq: number,
  source: string,
  fields: Partial<RunTransition> = {},
): RunTransition {
  return { branch: 0, occurrence: 0, seq, source, ...fields };
}

// The cyclic implement<->review graph from workflowGraph.test.ts's "cyclic"
// fixture, reused here so an edge computed by deriveTraversedEdges matches a
// real declared graph shape rather than an ad hoc one.
const cyclicGraph: WorkflowGraph = {
  name: "cyclic",
  version: 1,
  digest: "sha256:cyclic",
  start: "implement",
  nodes: [
    { id: "implement", kind: "agentic", owner: "builder" },
    { id: "review", kind: "gate", evaluator: "agentic", owner: "reviewer" },
  ],
  edges: [
    { source: "implement", target: "review" },
    { source: "review", target: "", outcome: "approve", terminal: "complete" },
    { source: "review", target: "implement", outcome: "needs-changes" },
  ],
};

describe("deriveTraversedEdges", () => {
  it("emphasizes the taken forward edge and never the untaken repass edge (#1430 first-pass criterion)", () => {
    const transitions = [
      transition(3, "implement", { target: "review" }),
      transition(4, "review", { target: "", terminal: true, status: "completed", verdict: "approve" }),
    ];
    const traversed = deriveTraversedEdges(transitions, 4);
    expect(edgeTraversed(traversed, cyclicGraph.edges[0])).toBe(true); // implement -> review
    expect(edgeTraversed(traversed, cyclicGraph.edges[2])).toBe(false); // review -> implement (never crossed)
    expect(edgeTraversed(traversed, cyclicGraph.edges[1])).toBe(true); // review -> complete
  });

  it("emphasizes the dotted repass edge only at and after its causal gate sequence (scrubbing)", () => {
    const transitions = [
      transition(3, "implement", { target: "review" }),
      transition(4, "review", { target: "implement", verdict: "needs-changes" }),
      transition(7, "implement", { target: "review" }),
      transition(8, "review", { target: "", terminal: true, status: "completed", verdict: "approve" }),
    ];
    const repassEdge = cyclicGraph.edges[2];

    // Before the gate decision, the repass has not happened yet.
    expect(edgeTraversed(deriveTraversedEdges(transitions, 3), repassEdge)).toBe(false);
    // At and after the causal gate.evaluated sequence, it has.
    expect(edgeTraversed(deriveTraversedEdges(transitions, 4), repassEdge)).toBe(true);
    expect(edgeTraversed(deriveTraversedEdges(transitions, 8), repassEdge)).toBe(true);
  });

  it("does not create phantom duplicate edges for a repeatedly traversed repass", () => {
    const transitions = [
      transition(4, "review", { target: "implement", verdict: "needs-changes" }),
      transition(8, "review", { target: "implement", verdict: "needs-changes" }),
      transition(12, "review", { target: "implement", verdict: "needs-changes" }),
    ];
    const traversed = deriveTraversedEdges(transitions, 12);
    expect(traversed.size).toBe(1);
    expect(edgeTraversed(traversed, cyclicGraph.edges[2])).toBe(true);
  });

  it("reflects a terminal transition only under its actual outcome, from recorded data alone", () => {
    const escalateEdge = { source: "review", target: "@escalate", terminal: "escalate" as const };
    const abortEdge = { source: "review", target: "@abort", terminal: "abort" as const };

    const escalated = deriveTraversedEdges(
      [transition(9, "review", { terminal: true, status: "escalated", verdict: "fail" })],
      9,
    );
    expect(edgeTraversed(escalated, escalateEdge)).toBe(true);
    expect(edgeTraversed(escalated, abortEdge)).toBe(false);

    // A bare task failure with no declared gate route has no matching
    // terminal edge at all (#1427's task-sourced terminal case) — this must
    // never be fabricated as a highlighted edge on some other terminal.
    const failed = deriveTraversedEdges(
      [transition(2, "implement", { terminal: true, status: "failed" })],
      2,
    );
    expect(edgeTraversed(failed, escalateEdge)).toBe(false);
    expect(edgeTraversed(failed, abortEdge)).toBe(false);
  });

  it("returns an empty, always-false set when transitions are unavailable (legacy runs)", () => {
    const traversed = deriveTraversedEdges(undefined, 999);
    expect(traversed.size).toBe(0);
    expect(edgeTraversed(traversed, cyclicGraph.edges[0])).toBe(false);
    expect(edgeTraversed(undefined, cyclicGraph.edges[0])).toBe(false);
  });
});

describe("deriveBranchStates", () => {
  it("tracks each declared branch independently, keyed by name (#1567)", () => {
    const events = [
      event(4, "branch.started", { branch: 1, parallel: "fanout", branchName: "security-lens" }),
      event(5, "branch.started", { branch: 2, parallel: "fanout", branchName: "perf-lens" }),
      event(9, "branch.finished", {
        branch: 1,
        parallel: "fanout",
        branchName: "security-lens",
        branchStatus: "failed",
      }),
      event(11, "branch.finished", {
        branch: 2,
        parallel: "fanout",
        branchName: "perf-lens",
        branchStatus: "succeeded",
      }),
    ];

    // Before either branch starts, nothing is tracked yet.
    expect(deriveBranchStates(events, 3).size).toBe(0);
    // Both running once started, before either finishes.
    expect(deriveBranchStates(events, 8).get("security-lens")?.state).toBe("running");
    expect(deriveBranchStates(events, 8).get("perf-lens")?.state).toBe("running");
    // Scrubbing to exactly the failure sequence reflects it; the sibling is
    // unaffected — one branch's outcome never bleeds into another's.
    expect(deriveBranchStates(events, 9).get("security-lens")?.state).toBe("failed");
    expect(deriveBranchStates(events, 9).get("perf-lens")?.state).toBe("running");
    // Both terminal once past the join.
    const final = deriveBranchStates(events, 14);
    expect(final.get("security-lens")).toEqual({ state: "failed", seq: 9 });
    expect(final.get("perf-lens")).toEqual({ state: "succeeded", seq: 11 });
  });

  it("labels every BranchState in words, never relying on color to distinguish cancelled from failed", () => {
    expect(branchStateLabel("running")).toBe("Running");
    expect(branchStateLabel("succeeded")).toBe("Succeeded");
    expect(branchStateLabel("failed")).toBe("Failed");
    expect(branchStateLabel("timed-out")).toBe("Timed out");
    expect(branchStateLabel("cancelled")).toBe("Cancelled");
    expect(branchStateLabel("no-output")).toBe("No output");
    expect(branchStateLabel("cancelled")).not.toBe(branchStateLabel("failed"));
  });
});

describe("eventStage", () => {
  it("reports the producing stage", () => {
    expect(eventStage(event(1, "stage.started", { stage: "implement", attempt: 1 }))).toBe("implement");
  });

  // A gate event carries `gate`, not `stage`. One column has to carry either,
  // or half the ledger reads as unscoped.
  it("falls back to the evaluating gate", () => {
    expect(eventStage(event(2, "gate.evaluated", { gate: "review", verdict: "pass" }))).toBe("review");
  });

  it("marks run-level events as unscoped", () => {
    expect(eventStage(event(3, "run.started", {}))).toBe(UNSCOPED_EVENT_STAGE);
  });
});

describe("runEventStages", () => {
  it("lists each scope once in durable order, unscoped last", () => {
    const events: RunEvent[] = [
      event(1, "run.started", {}),
      event(2, "stage.started", { stage: "implement", attempt: 1 }),
      event(3, "stage.finished", { stage: "implement", attempt: 1 }),
      event(4, "gate.evaluated", { gate: "review", verdict: "needs-changes" }),
      event(5, "stage.started", { stage: "implement", attempt: 2 }),
      event(6, "run.finished", {}),
    ];

    expect(runEventStages(events)).toEqual(["implement", "review", UNSCOPED_EVENT_STAGE]);
  });

  it("omits the unscoped bucket when every event is scoped", () => {
    expect(runEventStages([event(1, "stage.started", { stage: "implement", attempt: 1 })])).toEqual([
      "implement",
    ]);
  });

  it("is empty for no events", () => {
    expect(runEventStages([])).toEqual([]);
  });
});

describe("keyMoments", () => {
  it("keeps only decisions, handoffs, and escalations, dropping bookkeeping/evidence/liveness noise", () => {
    const events: RunEvent[] = [
      event(1, "run.started", { category: "transition" }),
      event(2, "gate.started", { category: "bookkeeping", gate: "review" }),
      event(3, "span.recorded", { category: "evidence", stage: "run:review", name: "reviewer.transcript" }),
      event(4, "stage.heartbeat", { category: "liveness", stage: "implement" }),
      event(5, "gate.evaluated", {
        category: "decision",
        gate: "review",
        verdict: "pass",
        target: "implement",
      }),
      event(6, "branch.started", { branchName: "docs" }),
      event(7, "branch.finished", { branchName: "docs", branchStatus: "succeeded" }),
    ];

    const kinds = keyMoments(events).map((moment) => [moment.event.seq, moment.kind]);
    expect(kinds).toEqual(
      expect.arrayContaining([
        [5, "decision"],
        [6, "handoff"],
        [7, "handoff"],
      ]),
    );
    expect(keyMoments(events)).toHaveLength(3);
  });

  it("orders by significance — escalation, then decision, then handoff — before falling back to recency", () => {
    const events: RunEvent[] = [
      event(1, "branch.started", { branchName: "docs" }),
      event(2, "gate.evaluated", { category: "decision", gate: "review", verdict: "pass" }),
      event(3, "stage.finished", { category: "transition", stage: "implement", status: "escalated" }),
      event(4, "gate.evaluated", { category: "decision", gate: "review", verdict: "needs-changes" }),
    ];

    expect(keyMoments(events).map((moment) => moment.event.seq)).toEqual([3, 4, 2, 1]);
  });

  it("classifies a gate decision that routes to escalate as an escalation, not a plain decision", () => {
    const events: RunEvent[] = [
      event(1, "gate.evaluated", {
        category: "decision",
        gate: "review",
        verdict: "blocked",
        target: "@escalate",
      }),
    ];

    expect(keyMoments(events)).toEqual([{ event: events[0], kind: "escalation" }]);
  });

  it("flags an explicitly escalated event even without a decision category", () => {
    const events: RunEvent[] = [
      event(1, "stage.finished", { category: "transition", stage: "implement", escalated: true }),
    ];

    expect(keyMoments(events)).toEqual([{ event: events[0], kind: "escalation" }]);
  });

  it("is empty for a run with no significant events", () => {
    const events: RunEvent[] = [
      event(1, "run.started", { category: "transition" }),
      event(2, "stage.heartbeat", { category: "liveness", stage: "implement" }),
    ];

    expect(keyMoments(events)).toEqual([]);
  });
});

describe("isFailureJournalEvent", () => {
  it("flags an explicit error event", () => {
    expect(isFailureJournalEvent(event(1, "error", { category: "bookkeeping" }))).toBe(true);
  });

  it("flags a failed or blocked stage attempt", () => {
    expect(
      isFailureJournalEvent(
        event(1, "stage.finished", { category: "transition", status: "failure" }),
      ),
    ).toBe(true);
    expect(
      isFailureJournalEvent(
        event(2, "stage.finished", { category: "transition", status: "blocked" }),
      ),
    ).toBe(true);
  });

  it("flags an escalated event", () => {
    expect(
      isFailureJournalEvent(event(1, "stage.finished", { category: "transition", escalated: true })),
    ).toBe(true);
    expect(
      isFailureJournalEvent(event(2, "run.finished", { category: "transition", status: "escalated" })),
    ).toBe(true);
  });

  it("does not flag ordinary success events", () => {
    expect(
      isFailureJournalEvent(
        event(1, "stage.finished", { category: "transition", status: "success" }),
      ),
    ).toBe(false);
    expect(isFailureJournalEvent(event(2, "stage.heartbeat", { category: "liveness" }))).toBe(
      false,
    );
  });
});

describe("keyMomentLabel", () => {
  it("gives each kind a human label", () => {
    expect(keyMomentLabel("escalation")).toBe("Escalation");
    expect(keyMomentLabel("decision")).toBe("Decision");
    expect(keyMomentLabel("handoff")).toBe("Handoff");
  });
});

describe("keyMomentEvidence", () => {
  it("finds the verdict artifact recorded on the same gate before the decision", () => {
    const events: RunEvent[] = [
      event(1, "gate.started", { category: "bookkeeping", gate: "review" }),
      event(2, "artifact.recorded", {
        category: "evidence",
        artifact: {
          name: "verdict/review-1.json",
          digest: "sha256:verdict-1",
          size: 40,
          mediaType: "application/json",
          stage: "review",
        },
      }),
      event(3, "gate.evaluated", { category: "decision", gate: "review", verdict: "pass" }),
    ];

    const decision = events[2];
    expect(keyMomentEvidence(events, decision, "run-1")).toBe(events[1]);
  });

  it("never looks past the moment's own sequence", () => {
    const events: RunEvent[] = [
      event(1, "gate.evaluated", { category: "decision", gate: "review", verdict: "pass" }),
      event(2, "artifact.recorded", {
        category: "evidence",
        artifact: {
          name: "verdict/review-1.json",
          digest: "sha256:verdict-1",
          size: 40,
          mediaType: "application/json",
          stage: "review",
        },
      }),
    ];

    expect(keyMomentEvidence(events, events[0], "run-1")).toBeUndefined();
  });

  it("is undefined when the branch never recorded inspectable evidence", () => {
    const events: RunEvent[] = [
      event(1, "gate.evaluated", { category: "decision", gate: "review", verdict: "pass" }),
    ];

    expect(keyMomentEvidence(events, events[0], "run-1")).toBeUndefined();
  });
});
