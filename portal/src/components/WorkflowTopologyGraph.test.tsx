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
    screen.getByRole("button", { name: /^stage-16,/ }).focus();
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
