import { describe, expect, it } from "vitest";
import type { WorkflowGraph } from "./api/types";
import {
  MAX_GRAPH_ZOOM,
  MIN_GRAPH_ZOOM,
  fitGraphZoom,
  layoutWorkflowGraph,
} from "./workflowGraph";

const fixtures: Record<string, WorkflowGraph> = {
  linear: {
    name: "linear",
    version: 1,
    digest: "sha256:linear",
    start: "first",
    nodes: [
      { id: "first", kind: "deterministic" },
      { id: "second", kind: "agentic", owner: "builder" },
    ],
    edges: [
      { source: "first", target: "second" },
      { source: "second", target: "", terminal: "complete" },
    ],
  },
  branching: {
    name: "branching",
    version: 1,
    digest: "sha256:branching",
    start: "prepare",
    nodes: [
      { id: "prepare", kind: "deterministic" },
      { id: "choose", kind: "gate", evaluator: "automated" },
      { id: "publish", kind: "deterministic" },
      { id: "repair", kind: "agentic", owner: "builder" },
    ],
    edges: [
      { source: "prepare", target: "choose" },
      { source: "choose", target: "publish", outcome: "pass" },
      { source: "choose", target: "repair", outcome: "repair" },
      { source: "publish", target: "", terminal: "complete" },
      { source: "repair", target: "@escalate", terminal: "escalate" },
    ],
  },
  cyclic: {
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
  },
  terminals: {
    name: "terminals",
    version: 1,
    digest: "sha256:terminals",
    start: "decide",
    nodes: [{ id: "decide", kind: "gate", evaluator: "human" }],
    edges: [
      { source: "decide", target: "", outcome: "approve", terminal: "complete" },
      { source: "decide", target: "@abort", outcome: "cancel", terminal: "abort" },
      { source: "decide", target: "@escalate", outcome: "defer", terminal: "escalate" },
    ],
  },
};

describe("canonical workflow graph layout", () => {
  it.each(Object.entries(fixtures))(
    "lays out the %s fixture deterministically without clipping",
    (_name, graph) => {
      const first = layoutWorkflowGraph(graph);
      const second = layoutWorkflowGraph(structuredClone(graph));

      expect(second).toEqual(first);
      expect(first.edges).toHaveLength(graph.edges.length);
      for (const node of first.nodes) {
        expect(node.x).toBeGreaterThanOrEqual(0);
        expect(node.y).toBeGreaterThanOrEqual(0);
        expect(node.x + node.width).toBeLessThanOrEqual(first.width);
        expect(node.y + node.height).toBeLessThanOrEqual(first.height);
      }
      expect(new Set(first.nodes.map((node) => `${node.x}:${node.y}`)).size).toBe(
        first.nodes.length,
      );
    },
  );

  it("reserves a visible lane and label for a repass edge", () => {
    const layout = layoutWorkflowGraph(fixtures.cyclic);
    const repass = layout.edges.find((edge) => edge.edge.outcome === "needs-changes");

    expect(repass).toMatchObject({ repass: true });
    expect(repass?.path).toContain(" C ");
    expect(repass?.labelY).toBeGreaterThan(
      Math.max(...layout.nodes.map((node) => node.y + node.height)),
    );
  });

  it("keeps fit and manual zoom within readable boundaries", () => {
    expect(fitGraphZoom(400, 200, 800, 400)).toBe(1);
    expect(fitGraphZoom(4_000, 2_000, 800, 400)).toBe(MIN_GRAPH_ZOOM);
    expect(MIN_GRAPH_ZOOM).toBeGreaterThanOrEqual(0.75);
    expect(MAX_GRAPH_ZOOM).toBeLessThanOrEqual(1.5);
  });
});
