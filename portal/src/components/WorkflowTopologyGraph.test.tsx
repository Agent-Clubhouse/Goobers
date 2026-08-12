import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import type { WorkflowGraph } from "../api/types";
import {
  MAX_GRAPH_ZOOM,
  MIN_GRAPH_ZOOM,
  clampGraphZoom,
  fitGraphZoom,
} from "../workflowGraph";
import { WorkflowTopologyGraph } from "./WorkflowTopologyGraph";

// traversedEdgesFrom builds a TraversedEdges map from edge keys alone, for
// tests that only care about membership, not the specific causal sequence.
function traversedEdgesFrom(keys: string[]): Map<string, number> {
  return new Map(keys.map((key, index) => [key, index + 1]));
}

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

  it("shapes gate nodes as decision points and keeps every kind separable by class (#2693)", () => {
    render(<Harness graph={cyclicGraph} />);

    const gate = screen.getByRole("button", { name: /^review, Gate/ });
    const shape = gate.querySelector(".workflow-node-shape");
    expect(shape).not.toBeNull();
    // The shape is decoration: the kind stays readable as text and as a class.
    expect(shape).toHaveAttribute("aria-hidden", "true");
    expect(gate).toHaveClass("workflow-node-gate");

    const deterministic = screen.getByRole("button", { name: /^query, Deterministic task/ });
    const agentic = screen.getByRole("button", { name: /^implement, Agentic task/ });
    expect(deterministic.querySelector(".workflow-node-shape")).toBeNull();
    expect(agentic.querySelector(".workflow-node-shape")).toBeNull();
    expect(deterministic).toHaveClass("workflow-node-deterministic");
    expect(agentic).toHaveClass("workflow-node-agentic");
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
    const viewport = screen.getByRole("group", { name: "implementation execution graph" });
    const initialZoom = Number(viewport.getAttribute("data-zoom"));
    fireEvent.keyDown(second, { key: "-" });
    expect(Number(viewport.getAttribute("data-zoom"))).toBeLessThan(initialZoom);
    focus.mockRestore();
  });

  it("offers concise bounded zoom, fit, and keyboard pan controls", () => {
    const graph = longGraph();
    render(<Harness graph={graph} />);
    const viewport = screen.getByRole("group", { name: "long execution graph" });
    const scrollBy = vi.fn();
    Object.defineProperty(viewport, "scrollBy", { configurable: true, value: scrollBy });

    expect(screen.getByRole("group", { name: "Graph view controls" })).toBeInTheDocument();
    expect(viewport).toHaveAttribute("data-responsive-layout", "scroll-under-820");
    expect(viewport).toHaveAttribute("data-zoom", "1.000");
    expect(screen.getByText("100%")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Pan (left|right|up|down)/ })).not.toBeInTheDocument();

    viewport.focus();
    fireEvent.keyDown(viewport, { key: "ArrowRight" });
    expect(scrollBy).toHaveBeenCalledWith({
      behavior: "auto",
      left: 120,
      top: 0,
    });

    const zoomOut = screen.getByRole("button", { name: "Zoom out" });
    fireEvent.keyDown(viewport, { key: "0" });
    for (let index = 0; index < 20; index += 1) {
      fireEvent.click(zoomOut);
    }
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(MIN_GRAPH_ZOOM);

    const zoomIn = screen.getByRole("button", { name: "Zoom in" });
    for (let index = 0; index < 20; index += 1) {
      fireEvent.click(zoomIn);
    }
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(MAX_GRAPH_ZOOM);

    fireEvent.keyDown(viewport, { key: "0" });
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(1);
    viewport.scrollLeft = 80;
    fireEvent.keyDown(viewport, { key: "f" });
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(
      fitGraphZoom(4492, 182, 720, 360),
    );
    expect(viewport.scrollLeft).toBe(0);
  });

  it("zooms around the pointer only when wheel input has explicit graph intent", () => {
    render(<Harness graph={longGraph()} />);
    const viewport = screen.getByRole("group", { name: "long execution graph" });
    Object.defineProperties(viewport, {
      clientHeight: { configurable: true, value: 360 },
      clientWidth: { configurable: true, value: 720 },
    });
    vi.spyOn(viewport, "getBoundingClientRect").mockReturnValue({
      bottom: 410,
      height: 360,
      left: 100,
      right: 820,
      top: 50,
      width: 720,
      x: 100,
      y: 50,
      toJSON: () => ({}),
    });
    fireEvent.click(screen.getByRole("button", { name: "Fit" }));
    viewport.scrollLeft = 100;
    viewport.scrollTop = 40;
    const initialZoom = Number(viewport.getAttribute("data-zoom"));

    const passiveWheel = new WheelEvent("wheel", {
      bubbles: true,
      cancelable: true,
      clientX: 300,
      clientY: 150,
      deltaY: -100,
    });
    expect(fireEvent(viewport, passiveWheel)).toBe(true);
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(initialZoom);

    viewport.focus();
    const boundedWheel = new WheelEvent("wheel", {
      bubbles: true,
      cancelable: true,
      clientX: 300,
      clientY: 150,
      deltaY: 100,
    });
    expect(fireEvent(viewport, boundedWheel)).toBe(true);
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(initialZoom);

    const zoomWheel = new WheelEvent("wheel", {
      bubbles: true,
      cancelable: true,
      clientX: 300,
      clientY: 150,
      deltaY: -100,
    });
    expect(fireEvent(viewport, zoomWheel)).toBe(false);
    const nextZoom = clampGraphZoom(initialZoom * Math.exp(0.2));
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(nextZoom);
    expect(viewport.scrollLeft).toBeCloseTo(((100 + 200) / initialZoom) * nextZoom - 200);
    expect(viewport.scrollTop).toBeCloseTo(((40 + 100) / initialZoom) * nextZoom - 100);
  });

  it("supports direct pointer pan and touch pinch zoom", () => {
    render(<Harness graph={longGraph()} />);
    const viewport = screen.getByRole("group", { name: "long execution graph" });
    viewport.scrollLeft = 200;
    viewport.scrollTop = 100;

    fireEvent.pointerDown(viewport, {
      button: 0,
      clientX: 300,
      clientY: 200,
      pointerId: 1,
      pointerType: "mouse",
    });
    fireEvent.pointerMove(viewport, {
      clientX: 250,
      clientY: 150,
      pointerId: 1,
      pointerType: "mouse",
    });
    expect(viewport.scrollLeft).toBe(250);
    expect(viewport.scrollTop).toBe(150);
    fireEvent.pointerUp(viewport, { pointerId: 1, pointerType: "mouse" });

    const initialZoom = Number(viewport.getAttribute("data-zoom"));
    fireEvent.pointerDown(viewport, {
      clientX: 200,
      clientY: 100,
      pointerId: 2,
      pointerType: "touch",
    });
    fireEvent.pointerDown(viewport, {
      clientX: 300,
      clientY: 100,
      pointerId: 3,
      pointerType: "touch",
    });
    fireEvent.pointerMove(viewport, {
      clientX: 400,
      clientY: 100,
      pointerId: 3,
      pointerType: "touch",
    });
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(
      clampGraphZoom(initialZoom * 2),
    );
  });

  it("uses an in-page fullscreen fallback and refits after viewport changes", () => {
    render(<Harness graph={longGraph()} />);
    const viewport = screen.getByRole("group", { name: "long execution graph" });
    const shell = viewport.closest(".workflow-graph-shell");
    if (!(shell instanceof HTMLElement)) {
      throw new Error("workflow graph shell was not rendered");
    }
    let width = 720;
    let height = 360;
    Object.defineProperties(viewport, {
      clientHeight: { configurable: true, get: () => height },
      clientWidth: { configurable: true, get: () => width },
    });
    const selected = screen.getByRole("button", { name: /^stage-1,/ });
    fireEvent.click(screen.getByRole("button", { name: "Fit" }));
    const initialZoom = Number(viewport.getAttribute("data-zoom"));

    fireEvent.click(screen.getByRole("button", { name: "Fullscreen" }));
    expect(shell).toHaveClass("workflow-graph-shell-expanded");
    expect(shell).toHaveAttribute("data-fullscreen", "fallback");
    expect(shell).toHaveAttribute("role", "dialog");
    expect(shell).toHaveAttribute("aria-modal", "true");
    expect(selected).toHaveAttribute("aria-pressed", "true");

    const firstFocusable = screen.getByRole("button", { name: "Zoom in" });
    // #1431's graph legend adds one more focusable stop (a native <summary>)
    // after the last graph node — it, not stage-16, is now the trap's actual
    // last element. The trap only intercepts the wrap-around edges (it
    // otherwise relies on the browser's native tab order between stops, which
    // jsdom's synthetic keydown does not simulate), so focus the legend
    // directly here rather than tabbing from stage-16 to it.
    const legendSummary = screen.getByText("Graph legend");
    legendSummary.focus();
    fireEvent.keyDown(window, { key: "Tab" });
    expect(firstFocusable).toHaveFocus();

    width = 1400;
    height = 700;
    fireEvent(window, new Event("resize"));
    expect(Number(viewport.getAttribute("data-zoom"))).toBeGreaterThan(initialZoom);

    fireEvent.keyDown(window, { key: "Escape" });
    expect(shell).not.toHaveClass("workflow-graph-shell-expanded");
    expect(screen.getByRole("button", { name: "Fullscreen" })).toHaveFocus();
    width = 720;
    height = 360;
    fireEvent(window, new Event("resize"));
    expect(Number(viewport.getAttribute("data-zoom"))).toBe(initialZoom);
    expect(selected).toHaveAttribute("aria-pressed", "true");
  });

  it("enters and exits the native Fullscreen API when available", async () => {
    render(<Harness graph={cyclicGraph} />);
    const viewport = screen.getByRole("group", { name: "implementation execution graph" });
    const shell = viewport.closest(".workflow-graph-shell");
    if (!(shell instanceof HTMLElement)) {
      throw new Error("workflow graph shell was not rendered");
    }
    const fullscreenDescriptor = Object.getOwnPropertyDescriptor(
      document,
      "fullscreenElement",
    );
    const exitFullscreenDescriptor = Object.getOwnPropertyDescriptor(
      document,
      "exitFullscreen",
    );
    let fullscreenElement: Element | null = null;
    Object.defineProperty(document, "fullscreenElement", {
      configurable: true,
      get: () => fullscreenElement,
    });
    const requestFullscreen = vi.fn(async () => {
      fullscreenElement = shell;
      document.dispatchEvent(new Event("fullscreenchange"));
    });
    const exitFullscreen = vi.fn(async () => {
      fullscreenElement = null;
      document.dispatchEvent(new Event("fullscreenchange"));
    });
    Object.defineProperty(shell, "requestFullscreen", {
      configurable: true,
      value: requestFullscreen,
    });
    Object.defineProperty(document, "exitFullscreen", {
      configurable: true,
      value: exitFullscreen,
    });

    try {
      fireEvent.click(screen.getByRole("button", { name: "Fullscreen" }));
      await waitFor(() => expect(shell).toHaveAttribute("data-fullscreen", "native"));
      expect(requestFullscreen).toHaveBeenCalledOnce();

      fireEvent.click(screen.getByRole("button", { name: "Exit fullscreen" }));
      await waitFor(() => expect(shell).toHaveAttribute("data-fullscreen", "none"));
      expect(exitFullscreen).toHaveBeenCalledOnce();
    } finally {
      if (fullscreenDescriptor) {
        Object.defineProperty(document, "fullscreenElement", fullscreenDescriptor);
      } else {
        Reflect.deleteProperty(document, "fullscreenElement");
      }
      if (exitFullscreenDescriptor) {
        Object.defineProperty(document, "exitFullscreen", exitFullscreenDescriptor);
      } else {
        Reflect.deleteProperty(document, "exitFullscreen");
      }
    }
  });

  it("keeps graph controls available for a compact topology", () => {
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

    expect(screen.getByRole("group", { name: "Graph view controls" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Fullscreen" })).toBeInTheDocument();
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
  const nodes = Array.from({ length: 16 }, (_, index) => ({
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

describe("workflow topology graph traversed edges (#1430)", () => {
  it("emphasizes only the edges actually crossed, not every edge whose endpoints were visited", () => {
    const { container } = render(
      <WorkflowTopologyGraph
        graph={cyclicGraph}
        nodeStates={{ query: "completed", implement: "completed", review: "completed" }}
        onSelectStage={() => {}}
        stateSeq={4}
        traversedEdges={traversedEdgesFrom(["query->implement", "implement->review", "review=>complete"])}
      />,
    );
    const edges = container.querySelectorAll<SVGPathElement>(".workflow-graph-edges path.workflow-graph-edge");
    // query -> implement -> review -> complete: the taken path.
    expect(edges[0]).toHaveClass("workflow-graph-edge-traversed");
    expect(edges[1]).toHaveClass("workflow-graph-edge-traversed");
    expect(edges[2]).toHaveClass("workflow-graph-edge-traversed");
    // review -> implement (needs-changes, a repass): never crossed this run,
    // even though both review and implement show non-pending states above —
    // the exact false positive #1430 exists to eliminate. Still dashed
    // (repass is a declared-graph property, independent of execution).
    expect(edges[3]).not.toHaveClass("workflow-graph-edge-traversed");
    expect(edges[3]).toHaveClass("workflow-graph-edge-repass");
    // review -> @escalate: declared but not taken.
    expect(edges[4]).not.toHaveClass("workflow-graph-edge-traversed");
  });

  it("emphasizes an actually-taken repass as both dashed and traversed", () => {
    const { container } = render(
      <WorkflowTopologyGraph
        graph={cyclicGraph}
        nodeStates={{ query: "completed", implement: "completed", review: "completed" }}
        onSelectStage={() => {}}
        stateSeq={4}
        traversedEdges={traversedEdgesFrom(["query->implement", "implement->review", "review->implement"])}
      />,
    );
    const edges = container.querySelectorAll<SVGPathElement>(".workflow-graph-edges path.workflow-graph-edge");
    const repass = edges[3]; // review -> implement (needs-changes)
    expect(repass).toHaveClass("workflow-graph-edge-repass");
    expect(repass).toHaveClass("workflow-graph-edge-traversed");
  });

  it("reflects traversal in the accessible topology list, not color alone", () => {
    render(
      <WorkflowTopologyGraph
        graph={cyclicGraph}
        nodeStates={{ query: "completed", implement: "completed", review: "completed" }}
        onSelectStage={() => {}}
        stateSeq={4}
        traversedEdges={traversedEdgesFrom(["query->implement", "implement->review", "review=>complete"])}
      />,
    );
    const topology = screen.getByRole("list", { name: "implementation accessible topology" });
    const reviewItem = within(topology)
      .getAllByRole("listitem")
      .find((item) => item.textContent?.startsWith("review,"));
    expect(reviewItem?.textContent).toContain(
      "approve to Complete terminal, configured forward route, traversed at sequence 3",
    );
    // The untaken repass gets no traversed qualifier, and is explicitly named
    // as a configured repass route rather than a forward one.
    expect(reviewItem?.textContent).toContain(
      "needs-changes to implement, configured repass route, not traversed",
    );
  });

  it("never emphasizes an edge when transitions are unavailable, even with matching node states", () => {
    const { container } = render(
      <WorkflowTopologyGraph
        graph={cyclicGraph}
        nodeStates={{ query: "completed", implement: "completed", review: "completed" }}
        onSelectStage={() => {}}
        stateSeq={4}
      />,
    );
    const edges = container.querySelectorAll<SVGPathElement>(".workflow-graph-edges path.workflow-graph-edge");
    for (const edge of edges) {
      expect(edge).not.toHaveClass("workflow-graph-edge-traversed");
    }
  });
});

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

describe("workflow topology graph parallel/branch lanes (#1567)", () => {
  const parallelGraph: WorkflowGraph = {
    name: "fanout-review",
    version: 1,
    digest: "sha256:fanout",
    start: "fanout",
    nodes: [
      { id: "fanout", kind: "parallel" },
      { id: "security-review", kind: "agentic", owner: "security" },
      { id: "perf-review", kind: "agentic", owner: "perf" },
      { id: "join", kind: "gate", evaluator: "automated" },
    ],
    edges: [
      { source: "fanout", target: "security-review", branch: "security-lens" },
      { source: "fanout", target: "perf-review", branch: "perf-lens" },
      { source: "security-review", target: "join", outcome: "join" },
      { source: "perf-review", target: "join", outcome: "join" },
    ],
  };

  it("renders a parallel node distinctly and its branches as independent lanes", () => {
    render(
      <WorkflowTopologyGraph
        graph={parallelGraph}
        onSelectStage={() => {}}
      />,
    );
    const node = screen.getByRole("button", {
      name: "fanout, Parallel, Fans out into declared branches, configured",
    });
    expect(node).toHaveAttribute("data-node-kind", "parallel");
    expect(node).toHaveClass("workflow-node-parallel");
    // Each declared branch is named on its own fan-out edge.
    expect(screen.getByText("security-lens", { selector: "text" })).toBeInTheDocument();
    expect(screen.getByText("perf-lens", { selector: "text" })).toBeInTheDocument();
  });

  it("names a branch's terminal state on its fan-out edge, distinct by label and dash pattern (not color alone)", () => {
    const { container } = render(
      <WorkflowTopologyGraph
        branchStates={
          new Map([
            ["security-lens", { state: "failed", seq: 9 }],
            ["perf-lens", { state: "cancelled", seq: 9 }],
          ])
        }
        graph={parallelGraph}
        nodeStates={{
          fanout: "completed",
          "security-review": "failed",
          "perf-review": "aborted",
          join: "pending",
        }}
        onSelectStage={() => {}}
        stateSeq={9}
      />,
    );

    expect(screen.getByText("security-lens — Failed", { selector: "text" })).toBeInTheDocument();
    expect(screen.getByText("perf-lens — Cancelled", { selector: "text" })).toBeInTheDocument();

    const edges = container.querySelectorAll<SVGPathElement>(
      ".workflow-graph-edges path.workflow-graph-edge",
    );
    const failedEdge = [...edges].find(
      (edge) => edge.getAttribute("data-branch-status") === "failed",
    );
    const cancelledEdge = [...edges].find(
      (edge) => edge.getAttribute("data-branch-status") === "cancelled",
    );
    expect(failedEdge).toHaveClass("workflow-graph-edge-branch-failed");
    expect(cancelledEdge).toHaveClass("workflow-graph-edge-branch-cancelled");
    // The two are visually distinguishable from each other by class alone,
    // independent of whatever color each resolves to.
    expect(failedEdge?.className.baseVal).not.toBe(cancelledEdge?.className.baseVal);
  });

  it("names the branch and its state in the accessible topology list", () => {
    render(
      <WorkflowTopologyGraph
        branchStates={new Map([["security-lens", { state: "succeeded", seq: 9 }]])}
        graph={parallelGraph}
        onSelectStage={() => {}}
        stateSeq={9}
        traversedEdges={new Map([["fanout->security-review", 9]])}
      />,
    );
    const topology = screen.getByRole("list", { name: "fanout-review accessible topology" });
    const fanoutItem = within(topology)
      .getAllByRole("listitem")
      .find((item) => item.textContent?.includes("fanout,"));
    expect(fanoutItem?.textContent).toContain(
      "branch security-lens — Succeeded to security-review, configured forward route, traversed at sequence 9",
    );
  });
});
