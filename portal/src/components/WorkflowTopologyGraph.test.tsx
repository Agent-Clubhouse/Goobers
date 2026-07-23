import { fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import type { WorkflowGraph } from "../api/types";
import { MIN_GRAPH_ZOOM } from "../workflowGraph";
import { WorkflowTopologyGraph } from "./WorkflowTopologyGraph";

const cyclicGraph: WorkflowGraph = {
  name: "implementation",
  version: 7,
  digest: "sha256:fixture",
  start: "query",
  nodes: [
    { id: "query", kind: "deterministic" },
    { id: "implement", kind: "agentic", owner: "core/implementer" },
    { id: "review", kind: "gate", evaluator: "agentic", owner: "core/reviewer" },
  ],
  edges: [
    { source: "query", target: "implement" },
    { source: "implement", target: "review" },
    { source: "review", target: "", outcome: "approve", terminal: "complete" },
    { source: "review", target: "implement", outcome: "needs-changes" },
    { source: "review", target: "@escalate", outcome: "fail", terminal: "escalate" },
  ],
};

describe("workflow topology graph", () => {
  it.each([
    ["linear", linearGraph()],
    ["branching", branchingGraph()],
    ["cyclic/repass", cyclicGraph],
    ["terminal-target", terminalGraph()],
  ])("renders the %s topology fixture", (name, graph) => {
    render(<Harness graph={graph} />);

    expect(
      screen.getByRole("group", { name: `${graph.name} execution graph` }),
      name,
    ).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /configured$/i })).toHaveLength(
      graph.nodes.length,
    );
    expect(screen.getAllByRole("note")).toHaveLength(
      graph.edges.filter((edge) => edge.terminal).length,
    );
  });

  it("exposes node semantics, branch labels, terminals, and an equivalent topology list", () => {
    render(<Harness graph={cyclicGraph} />);

    expect(
      screen.getByRole("button", {
        name: "query, Deterministic task, Runs deterministically, configured",
      }),
    ).toHaveAttribute("data-node-kind", "deterministic");
    expect(
      screen.getByRole("button", {
        name: "implement, Agentic task, Owned by core/implementer, configured",
      }),
    ).toHaveTextContent("Configured");
    expect(
      screen.getByRole("button", {
        name: "review, Gate, agentic evaluator, owned by core/reviewer, configured",
      }),
    ).toHaveAttribute("data-node-kind", "gate");
    expect(screen.getByText("needs-changes", { selector: "text" })).toBeInTheDocument();
    expect(screen.getByRole("note", { name: "Complete terminal target" })).toHaveTextContent(
      "Complete",
    );
    expect(screen.getByRole("note", { name: "Escalate terminal target" })).toHaveTextContent(
      "Escalate",
    );

    const topology = screen.getByRole("list", {
      name: "implementation accessible topology",
    });
    expect(within(topology).getByText(/Start stage.*query.*Deterministic task/)).toBeInTheDocument();
    expect(within(topology).getByText(/needs-changes to implement/)).toBeInTheDocument();
    expect(within(topology).getByText(/approve to Complete terminal/)).toBeInTheDocument();
  });

  it("scrolls the next stage into view before moving keyboard focus", () => {
    const scrollIntoView = vi.fn();
    Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
      configurable: true,
      value: scrollIntoView,
    });
    const focus = vi.spyOn(HTMLElement.prototype, "focus");
    render(<Harness graph={cyclicGraph} />);
    const first = screen.getByRole("button", { name: /^query,/ });
    const second = screen.getByRole("button", { name: /^implement,/ });

    first.focus();
    focus.mockClear();
    fireEvent.keyDown(first, { key: "ArrowRight" });

    expect(scrollIntoView).toHaveBeenCalledWith({ block: "nearest", inline: "nearest" });
    expect(second).toHaveFocus();
    expect(second).toHaveAttribute("aria-pressed", "true");
    expect(scrollIntoView.mock.invocationCallOrder.at(-1)).toBeLessThan(
      focus.mock.invocationCallOrder.at(-1) ?? Number.POSITIVE_INFINITY,
    );
    focus.mockRestore();
  });

  it("shows bounded fit, pan, and zoom controls only for long topology", () => {
    const graph = longGraph();
    render(<Harness graph={graph} />);
    const viewport = screen.getByRole("group", { name: "long execution graph" });
    const scrollBy = vi.fn();
    Object.defineProperty(viewport, "scrollBy", { configurable: true, value: scrollBy });

    expect(screen.getByRole("group", { name: "Graph view controls" })).toBeInTheDocument();
    expect(viewport).toHaveAttribute("data-responsive-layout", "scroll-under-820");
    expect(Number(viewport.getAttribute("data-zoom"))).toBeGreaterThanOrEqual(MIN_GRAPH_ZOOM);

    fireEvent.click(screen.getByRole("button", { name: "Pan right" }));
    expect(scrollBy).toHaveBeenCalledWith({
      behavior: "auto",
      left: 120,
      top: 0,
    });
    viewport.scrollLeft = 80;

    fireEvent.click(screen.getByRole("button", { name: "Zoom in" }));
    expect(Number(viewport.getAttribute("data-zoom"))).toBeGreaterThan(MIN_GRAPH_ZOOM);
    fireEvent.click(screen.getByRole("button", { name: "Fit" }));
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(MIN_GRAPH_ZOOM);
    expect(viewport.scrollLeft).toBe(0);
  });

  it("does not add navigation controls to a compact topology", () => {
    render(
      <Harness
        graph={{
          name: "compact",
          version: 1,
          digest: "sha256:compact",
          start: "only",
          nodes: [{ id: "only", kind: "deterministic" }],
          edges: [{ source: "only", target: "", terminal: "complete" }],
        }}
      />,
    );

    expect(screen.queryByRole("group", { name: "Graph view controls" })).not.toBeInTheDocument();
  });
});

function Harness({ graph }: { graph: WorkflowGraph }) {
  const [selected, setSelected] = useState(graph.start);
  return (
    <WorkflowTopologyGraph
      graph={graph}
      onSelectStage={setSelected}
      selectedStageId={selected}
    />
  );
}

function longGraph(): WorkflowGraph {
  const nodes = Array.from({ length: 8 }, (_, index) => ({
    id: `stage-${index + 1}`,
    kind: "deterministic" as const,
  }));
  return {
    name: "long",
    version: 1,
    digest: "sha256:long",
    start: nodes[0].id,
    nodes,
    edges: nodes.map((node, index) =>
      index === nodes.length - 1
        ? { source: node.id, target: "", terminal: "complete" as const }
        : { source: node.id, target: nodes[index + 1].id },
    ),
  };
}

function linearGraph(): WorkflowGraph {
  return {
    name: "linear",
    version: 1,
    digest: "sha256:linear",
    start: "one",
    nodes: [
      { id: "one", kind: "deterministic" },
      { id: "two", kind: "agentic", owner: "builder" },
    ],
    edges: [
      { source: "one", target: "two" },
      { source: "two", target: "", terminal: "complete" },
    ],
  };
}

function branchingGraph(): WorkflowGraph {
  return {
    name: "branching",
    version: 1,
    digest: "sha256:branching",
    start: "choose",
    nodes: [
      { id: "choose", kind: "gate", evaluator: "automated" },
      { id: "left", kind: "deterministic" },
      { id: "right", kind: "agentic", owner: "builder" },
    ],
    edges: [
      { source: "choose", target: "left", outcome: "left" },
      { source: "choose", target: "right", outcome: "right" },
      { source: "left", target: "", terminal: "complete" },
      { source: "right", target: "@escalate", terminal: "escalate" },
    ],
  };
}

function terminalGraph(): WorkflowGraph {
  return {
    name: "terminal-target",
    version: 1,
    digest: "sha256:terminal",
    start: "choose",
    nodes: [{ id: "choose", kind: "gate", evaluator: "human" }],
    edges: [
      { source: "choose", target: "", outcome: "approve", terminal: "complete" },
      { source: "choose", target: "@abort", outcome: "cancel", terminal: "abort" },
      { source: "choose", target: "@escalate", outcome: "defer", terminal: "escalate" },
    ],
  };
}

describe("workflow topology graph escalation cause (DASH-21)", () => {
  const graph: WorkflowGraph = {
    name: "impl",
    version: 1,
    digest: "sha256:x",
    start: "implement",
    nodes: [
      { id: "implement", kind: "agentic", owner: "core/impl" },
      { id: "review", kind: "gate", evaluator: "agentic" },
    ],
    edges: [{ source: "implement", target: "review" }],
  };

  it("marks the causal node by class and accessible label, not color alone", () => {
    render(
      <WorkflowTopologyGraph
        causalNodeId="review"
        graph={graph}
        nodeStates={{ implement: "completed", review: "escalated" }}
        onSelectStage={() => {}}
        stateSeq={9}
      />,
    );
    const causal = screen.getByRole("button", { name: /review, gate, Escalated at sequence 9, escalation cause/ });
    expect(causal).toHaveClass("run-node-causal");
    expect(
      screen.getByRole("button", { name: /implement, agentic, Completed at sequence 9$/ }),
    ).not.toHaveClass("run-node-causal");
  });
});
