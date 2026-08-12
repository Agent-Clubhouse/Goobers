import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { RefObject } from "react";
import type { WorkflowGraph, WorkflowGraphEdge, WorkflowGraphNode } from "../api/types";
import {
  branchStateLabel,
  edgeTraversed,
  traversedEdgeSeq,
  type BranchStateEntry,
  type RunNodeState,
  type TraversedEdges,
} from "../runDetailData";
import { WorkflowGraphLegend } from "./WorkflowGraphLegend";
import {
  MAX_GRAPH_ZOOM,
  MIN_GRAPH_ZOOM,
  clampGraphZoom,
  fitGraphZoom,
  layoutWorkflowGraph,
  terminalLabel,
  type WorkflowLayoutEdge,
} from "../workflowGraph";

const FALLBACK_VIEWPORT_WIDTH = 720;
const FALLBACK_VIEWPORT_HEIGHT = 360;
const PAN_DISTANCE = 120;
const ZOOM_STEP = 0.1;
const WHEEL_ZOOM_SENSITIVITY = 0.002;
const FALLBACK_FOCUSABLE_SELECTOR = [
  "a[href]",
  "area[href]",
  "button",
  'input:not([type="hidden"])',
  "select",
  "textarea",
  "summary",
  '[contenteditable="true"]',
  "[tabindex]",
].join(", ");

export type WorkflowGraphFullscreenMode = "none" | "native" | "fallback";

interface PointerPosition {
  x: number;
  y: number;
}

interface PendingScroll {
  left: number;
  top: number;
  zoom: number;
}

function runStateLabel(state: RunNodeState): string {
  return state.charAt(0).toUpperCase() + state.slice(1);
}

export function WorkflowTopologyGraph({
  graph,
  onSelectStage,
  selectedStageId,
  nodeStates,
  stateSeq,
  traversedEdges,
  branchStates,
  causalNodeId,
  fullscreenTargetRef,
  onFullscreenModeChange,
}: {
  graph: WorkflowGraph;
  onSelectStage: (stageId: string, revealInspector?: boolean) => void;
  selectedStageId?: string;
  // When present, the graph is a live run overlay: each node paints its
  // as-of-sequence run state and the executed path is emphasized (DASH-19).
  nodeStates?: Record<string, RunNodeState>;
  stateSeq?: number;
  // The declared edges actually crossed as of stateSeq, from the run's exact
  // transition history (#1427) via deriveTraversedEdges — never inferred from
  // both endpoints being visited, which false-positives on a cyclic
  // workflow's untaken repass edges (#1430).
  traversedEdges?: TraversedEdges;
  // Per-declared-branch execution state (#1567), keyed by WorkflowGraphEdge's
  // branch name — the same key a parallel's fan-out edges carry. Read off
  // branch.started/branch.finished events directly, since a branch has no
  // single graph node of its own to hang a nodeStates entry off of.
  branchStates?: Map<string, BranchStateEntry>;
  // The single node the authoritative escalation cause points at (DASH-21).
  causalNodeId?: string;
  fullscreenTargetRef?: RefObject<HTMLElement | null>;
  onFullscreenModeChange?: (mode: WorkflowGraphFullscreenMode) => void;
}) {
  const layout = useMemo(() => layoutWorkflowGraph(graph), [graph]);
  const markerId = `workflow-arrow-${useId().replaceAll(":", "")}`;
  const instructionsId = `workflow-instructions-${useId().replaceAll(":", "")}`;
  const shellRef = useRef<HTMLDivElement | null>(null);
  const viewportRef = useRef<HTMLDivElement | null>(null);
  const fullscreenButtonRef = useRef<HTMLButtonElement | null>(null);
  const nodeRefs = useRef(new Map<string, HTMLButtonElement>());
  const pointersRef = useRef(new Map<number, PointerPosition>());
  const pendingScrollRef = useRef<PendingScroll | null>(null);
  const [viewportSize, setViewportSize] = useState({
    width: FALLBACK_VIEWPORT_WIDTH,
    height: FALLBACK_VIEWPORT_HEIGHT,
  });
  const [fitActive, setFitActive] = useState(false);
  const [zoom, setZoom] = useState(1);
  const zoomRef = useRef(zoom);
  const [dragging, setDragging] = useState(false);
  const [fullscreenMode, setFullscreenMode] =
    useState<WorkflowGraphFullscreenMode>("none");
  const [fullscreenError, setFullscreenError] = useState("");
  zoomRef.current = zoom;
  const getFullscreenTarget = useCallback(
    () => fullscreenTargetRef?.current ?? shellRef.current,
    [fullscreenTargetRef],
  );

  const setZoomAround = useCallback(
    (
      requestedZoom: number,
      sourceClientX?: number,
      sourceClientY?: number,
      targetClientX = sourceClientX,
      targetClientY = sourceClientY,
    ) => {
      const viewport = viewportRef.current;
      const currentZoom = zoomRef.current;
      const nextZoom =
        currentZoom < MIN_GRAPH_ZOOM && requestedZoom < currentZoom
          ? currentZoom
          : clampGraphZoom(requestedZoom);
      if (!viewport || nextZoom === currentZoom) {
        return false;
      }

      setFitActive(false);
      const rect = viewport.getBoundingClientRect();
      const visibleWidth = viewport.clientWidth || viewportSize.width;
      const visibleHeight = viewport.clientHeight || viewportSize.height;
      const sourceX = (sourceClientX ?? rect.left + visibleWidth / 2) - rect.left;
      const sourceY = (sourceClientY ?? rect.top + visibleHeight / 2) - rect.top;
      const targetX = (targetClientX ?? rect.left + visibleWidth / 2) - rect.left;
      const targetY = (targetClientY ?? rect.top + visibleHeight / 2) - rect.top;
      const pending = pendingScrollRef.current;
      const currentLeft = pending?.zoom === currentZoom ? pending.left : viewport.scrollLeft;
      const currentTop = pending?.zoom === currentZoom ? pending.top : viewport.scrollTop;

      pendingScrollRef.current = {
        left: ((currentLeft + sourceX) / currentZoom) * nextZoom - targetX,
        top: ((currentTop + sourceY) / currentZoom) * nextZoom - targetY,
        zoom: nextZoom,
      };
      zoomRef.current = nextZoom;
      setZoom(nextZoom);
      return true;
    },
    [viewportSize.height, viewportSize.width],
  );

  useLayoutEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    const measure = () => {
      if (viewport.clientWidth > 0 && viewport.clientHeight > 0) {
        setViewportSize({
          width: viewport.clientWidth,
          height: viewport.clientHeight,
        });
      }
    };
    measure();
    window.addEventListener("resize", measure);
    const observer =
      typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    observer?.observe(viewport);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, []);

  useLayoutEffect(() => {
    if (!fitActive) {
      return;
    }
    const nextZoom = fitGraphZoom(
      layout.width,
      layout.height,
      viewportSize.width,
      viewportSize.height,
    );
    zoomRef.current = nextZoom;
    pendingScrollRef.current = null;
    setZoom(nextZoom);
    const viewport = viewportRef.current;
    if (viewport) {
      viewport.scrollLeft = 0;
      viewport.scrollTop = 0;
    }
  }, [fitActive, layout.height, layout.width, viewportSize.height, viewportSize.width]);

  useLayoutEffect(() => {
    const pending = pendingScrollRef.current;
    const viewport = viewportRef.current;
    if (!pending || !viewport || pending.zoom !== zoom) {
      return;
    }
    viewport.scrollLeft = Math.max(0, pending.left);
    viewport.scrollTop = Math.max(0, pending.top);
    pendingScrollRef.current = null;
  }, [zoom]);

  useEffect(() => {
    const syncFullscreenState = () => {
      setFullscreenMode((current) => {
        const target = getFullscreenTarget();
        if (target && document.fullscreenElement === target) {
          return "native";
        }
        return current === "native" ? "none" : current;
      });
    };
    document.addEventListener("fullscreenchange", syncFullscreenState);
    return () => document.removeEventListener("fullscreenchange", syncFullscreenState);
  }, [getFullscreenTarget]);

  useEffect(() => {
    onFullscreenModeChange?.(fullscreenMode);
  }, [fullscreenMode, onFullscreenModeChange]);

  useEffect(() => {
    if (fullscreenMode !== "fallback") {
      return;
    }
    const containFallbackFocus = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setFullscreenMode("none");
        fullscreenButtonRef.current?.focus();
        return;
      }
      const target = getFullscreenTarget();
      if (event.key !== "Tab" || !target) {
        return;
      }
      const focusable = [
        ...target.querySelectorAll<HTMLElement>(FALLBACK_FOCUSABLE_SELECTOR),
      ]
        .filter((element) => element.tabIndex >= 0 && !element.matches(":disabled"))
        .sort((left, right) => {
          if (left === right) {
            return 0;
          }
          return left.compareDocumentPosition(right) & Node.DOCUMENT_POSITION_FOLLOWING
            ? -1
            : 1;
        });
      const first = focusable[0];
      const last = focusable.at(-1);
      if (!first || !last) {
        return;
      }
      const activeIndex = focusable.findIndex(
        (element) => element === document.activeElement,
      );
      if (activeIndex < 0) {
        event.preventDefault();
        (event.shiftKey ? last : first).focus();
      } else if (event.shiftKey && activeIndex === 0) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && activeIndex === focusable.length - 1) {
        event.preventDefault();
        first.focus();
      }
    };
    window.addEventListener("keydown", containFallbackFocus);
    return () => window.removeEventListener("keydown", containFallbackFocus);
  }, [fullscreenMode, getFullscreenTarget]);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    const zoomOnWheel = (event: WheelEvent) => {
      const focusedElement = document.activeElement;
      const hasZoomIntent =
        event.ctrlKey ||
        event.metaKey ||
        (focusedElement instanceof Node && viewport.contains(focusedElement));
      const horizontalPan =
        !event.ctrlKey && !event.metaKey && (event.shiftKey || event.deltaY === 0);
      if (!hasZoomIntent || horizontalPan) {
        return;
      }
      if (
        setZoomAround(
          zoomRef.current * Math.exp(-event.deltaY * WHEEL_ZOOM_SENSITIVITY),
          event.clientX,
          event.clientY,
        )
      ) {
        event.preventDefault();
      }
    };
    viewport.addEventListener("wheel", zoomOnWheel, { passive: false });
    return () => viewport.removeEventListener("wheel", zoomOnWheel);
  }, [layout.nodes.length, setZoomAround]);

  const moveSelection = (targetIndex: number) => {
    const target = layout.stageOrder[targetIndex];
    if (!target) {
      return;
    }
    onSelectStage(target.id);
    const element = nodeRefs.current.get(target.id);
    element?.scrollIntoView?.({ block: "nearest", inline: "nearest" });
    element?.focus();
  };
  const changeZoom = (change: number) => {
    setZoomAround(zoomRef.current + change);
  };
  const fit = () => {
    const nextZoom = fitGraphZoom(
      layout.width,
      layout.height,
      viewportSize.width,
      viewportSize.height,
    );
    setFitActive(true);
    zoomRef.current = nextZoom;
    pendingScrollRef.current = null;
    setZoom(nextZoom);
    const viewport = viewportRef.current;
    if (viewport) {
      viewport.scrollLeft = 0;
      viewport.scrollTop = 0;
    }
  };
  const pan = (left: number, top: number) => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    setFitActive(false);
    if (typeof viewport.scrollBy === "function") {
      viewport.scrollBy({ behavior: "auto", left, top });
    } else {
      viewport.scrollLeft += left;
      viewport.scrollTop += top;
    }
  };
  const handlePointerDown = (event: React.PointerEvent<HTMLDivElement>) => {
    if (event.pointerType === "mouse" && event.button !== 0) {
      return;
    }
    if (event.target instanceof Element && event.target.closest("button")) {
      return;
    }
    if (event.pointerType !== "touch") {
      event.preventDefault();
    }
    event.currentTarget.focus({ preventScroll: true });
    event.currentTarget.setPointerCapture?.(event.pointerId);
    pointersRef.current.set(event.pointerId, { x: event.clientX, y: event.clientY });
    setDragging(true);
  };
  const handlePointerMove = (event: React.PointerEvent<HTMLDivElement>) => {
    const pointers = pointersRef.current;
    const previous = pointers.get(event.pointerId);
    if (!previous) {
      return;
    }
    event.preventDefault();
    const previousPair = [...pointers.values()].slice(0, 2);
    pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    const currentPair = [...pointers.values()].slice(0, 2);

    if (previousPair.length === 2 && currentPair.length === 2) {
      const previousCenter = midpoint(previousPair[0], previousPair[1]);
      const currentCenter = midpoint(currentPair[0], currentPair[1]);
      const previousDistance = distance(previousPair[0], previousPair[1]);
      const currentDistance = distance(currentPair[0], currentPair[1]);
      if (previousDistance > 0) {
        setZoomAround(
          zoomRef.current * (currentDistance / previousDistance),
          previousCenter.x,
          previousCenter.y,
          currentCenter.x,
          currentCenter.y,
        );
      }
      return;
    }

    const viewport = viewportRef.current;
    if (viewport) {
      setFitActive(false);
      viewport.scrollLeft -= event.clientX - previous.x;
      viewport.scrollTop -= event.clientY - previous.y;
    }
  };
  const handlePointerEnd = (event: React.PointerEvent<HTMLDivElement>) => {
    pointersRef.current.delete(event.pointerId);
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    if (pointersRef.current.size === 0) {
      setDragging(false);
    }
  };
  const handleViewportKeyDown = (event: React.KeyboardEvent<HTMLDivElement>) => {
    switch (event.key) {
      case "+":
      case "=":
        event.preventDefault();
        changeZoom(ZOOM_STEP);
        break;
      case "-":
        event.preventDefault();
        changeZoom(-ZOOM_STEP);
        break;
      case "0":
        event.preventDefault();
        setZoomAround(1);
        break;
      case "f":
      case "F":
        event.preventDefault();
        fit();
        break;
      case "ArrowLeft":
        if (event.target !== event.currentTarget) {
          break;
        }
        event.preventDefault();
        pan(-PAN_DISTANCE, 0);
        break;
      case "ArrowRight":
        if (event.target !== event.currentTarget) {
          break;
        }
        event.preventDefault();
        pan(PAN_DISTANCE, 0);
        break;
      case "ArrowUp":
        if (event.target !== event.currentTarget) {
          break;
        }
        event.preventDefault();
        pan(0, -PAN_DISTANCE);
        break;
      case "ArrowDown":
        if (event.target !== event.currentTarget) {
          break;
        }
        event.preventDefault();
        pan(0, PAN_DISTANCE);
        break;
    }
  };
  const toggleFullscreen = async () => {
    const target = getFullscreenTarget();
    if (!target) {
      return;
    }
    setFullscreenError("");
    if (fullscreenMode === "fallback") {
      setFullscreenMode("none");
      return;
    }
    if (fullscreenMode === "native") {
      try {
        await document.exitFullscreen();
        setFullscreenMode("none");
      } catch {
        setFullscreenError("Fullscreen could not be closed. Use Escape to exit.");
      }
      return;
    }
    if (typeof target.requestFullscreen !== "function") {
      setFullscreenMode("fallback");
      return;
    }
    try {
      await target.requestFullscreen();
      setFullscreenMode("native");
    } catch {
      setFullscreenMode("fallback");
    }
  };

  if (layout.nodes.length === 0) {
    return (
      <div className="empty-detail" role="status">
        <strong>No stages in this workflow definition</strong>
      </div>
    );
  }

  return (
    <div
      className={[
        "workflow-graph-shell",
        fullscreenTargetRef ? "" : "workflow-graph-fullscreen-target",
        fullscreenMode === "fallback" && !fullscreenTargetRef
          ? "workflow-graph-shell-expanded"
          : "",
      ]
        .filter(Boolean)
        .join(" ")}
      aria-label={
        fullscreenMode === "fallback" && !fullscreenTargetRef
          ? "Workflow graph fullscreen view"
          : undefined
      }
      aria-modal={
        fullscreenMode === "fallback" && !fullscreenTargetRef ? "true" : undefined
      }
      data-fullscreen={fullscreenMode}
      ref={shellRef}
      role={
        fullscreenMode === "fallback" && !fullscreenTargetRef ? "dialog" : undefined
      }
    >
      {nodeStates && (
        <p className="run-graph-pin">
          Pinned v{graph.version} · <span className="mono">{graph.digest}</span>
        </p>
      )}
      <div aria-label="Graph view controls" className="workflow-graph-controls" role="group">
        <button
          aria-label="Zoom out"
          disabled={zoom <= MIN_GRAPH_ZOOM}
          onClick={() => changeZoom(-ZOOM_STEP)}
          type="button"
        >
          -
        </button>
        <span aria-live="polite" className="graph-zoom-value">
          {Math.round(zoom * 100)}%
        </span>
        <button
          aria-label="Zoom in"
          disabled={zoom >= MAX_GRAPH_ZOOM}
          onClick={() => changeZoom(ZOOM_STEP)}
          type="button"
        >
          +
        </button>
        <button onClick={fit} type="button">
          Fit
        </button>
        <button
          aria-label={fullscreenMode === "none" ? "Fullscreen" : "Exit fullscreen"}
          onClick={() => void toggleFullscreen()}
          ref={fullscreenButtonRef}
          type="button"
        >
          {fullscreenMode === "none" ? "Fullscreen" : "Exit fullscreen"}
        </button>
        {fullscreenError && (
          <span className="sr-only" role="status">
            {fullscreenError}
          </span>
        )}
      </div>
      <p className="sr-only" id={instructionsId}>
        Focus the graph to zoom with the wheel. Drag to pan. Use plus and minus to zoom,
        arrow keys to pan, zero to reset, and F to fit.
      </p>
      <div
        aria-describedby={instructionsId}
        aria-label={`${graph.name} ${nodeStates ? "pinned " : ""}execution graph`}
        className={`workflow-graph-viewport${dragging ? " is-dragging" : ""}`}
        data-responsive-layout="scroll-under-820"
        data-zoom={zoom.toFixed(3)}
        onKeyDown={handleViewportKeyDown}
        onPointerCancel={handlePointerEnd}
        onPointerDown={handlePointerDown}
        onPointerMove={handlePointerMove}
        onPointerUp={handlePointerEnd}
        ref={viewportRef}
        role="group"
        tabIndex={0}
      >
        <div
          className="workflow-graph-sizer"
          style={{ height: layout.height * zoom, width: layout.width * zoom }}
        >
          <div
            className="workflow-graph-surface"
            style={{
              height: layout.height,
              transform: `scale(${zoom})`,
              width: layout.width,
            }}
          >
            <svg
              aria-hidden="true"
              className="workflow-graph-edges"
              height={layout.height}
              viewBox={`0 0 ${layout.width} ${layout.height}`}
              width={layout.width}
            >
              <defs>
                <marker
                  id={markerId}
                  markerHeight="8"
                  markerWidth="8"
                  orient="auto"
                  refX="7"
                  refY="4"
                >
                  <path className="workflow-graph-arrow" d="M 0 0 L 8 4 L 0 8 z" />
                </marker>
              </defs>
              {layout.edges.map((edge) => {
                const traversed = edgeTraversed(traversedEdges, edge.edge);
                const branchState = edge.edge.branch
                  ? branchStates?.get(edge.edge.branch)
                  : undefined;
                const label = edge.edge.branch
                  ? branchEdgeLabel(edge.edge.branch, branchState)
                  : edge.edge.outcome;
                return (
                <g key={edge.id}>
                  <path
                    className={[
                      "workflow-graph-edge",
                      edge.repass ? "workflow-graph-edge-repass" : "",
                      nodeStates ? "workflow-graph-edge-declared" : "",
                      traversed ? "workflow-graph-edge-traversed" : "",
                      branchState ? `workflow-graph-edge-branch-${branchState.state}` : "",
                    ]
                      .filter(Boolean)
                      .join(" ")}
                    d={edge.path}
                    data-branch-status={branchState?.state}
                    markerEnd={`url(#${markerId})`}
                  />
                  {label && (
                    <text
                      className="workflow-graph-edge-label"
                      x={edge.labelX}
                      y={edge.labelY}
                    >
                      {label}
                    </text>
                  )}
                </g>
                );
              })}
            </svg>
            {layout.nodes.map((layoutNode) => {
              if (layoutNode.type === "terminal") {
                return (
                  <div
                    aria-label={`${terminalLabel(layoutNode.terminal)} terminal target`}
                    className={`workflow-terminal workflow-terminal-${layoutNode.terminal}`}
                    key={layoutNode.id}
                    role="note"
                    style={{ left: layoutNode.x, top: layoutNode.y }}
                  >
                    <span>Terminal</span>
                    <strong>{terminalLabel(layoutNode.terminal)}</strong>
                  </div>
                );
              }
              const { node } = layoutNode;
              const index = layout.stageOrder.findIndex((stage) => stage.id === node.id);
              const actor = nodeActor(node);
              const runState = nodeStates?.[node.id];
              const stateText = runState ? runStateLabel(runState) : "Configured";
              const seqSuffix = stateSeq !== undefined ? ` at sequence ${stateSeq}` : "";
              // In a live run overlay the label is concise and state-first
              // ("review, gate, Running at sequence 6"); on the definition page
              // it stays the richer configured-topology label.
              const causalSuffix = causalNodeId === node.id ? ", escalation cause" : "";
              const ariaLabel = runState
                ? `${node.id}, ${node.kind}, ${runStateLabel(runState)}${seqSuffix}${causalSuffix}`
                : `${node.id}, ${nodeKindLabel(node)}, ${actor}, configured${causalSuffix}`;
              return (
                <button
                  aria-label={ariaLabel}
                  aria-pressed={selectedStageId === node.id}
                  className={[
                    "workflow-graph-node",
                    `workflow-node-${node.kind}`,
                    runState ? `run-node-state-${runState}` : "",
                    causalNodeId === node.id ? "run-node-causal" : "",
                  ]
                    .filter(Boolean)
                    .join(" ")}
                  data-node-kind={node.kind}
                  data-run-state={runState}
                  data-causal={causalNodeId === node.id ? "true" : undefined}
                  key={node.id}
                  onClick={() => onSelectStage(node.id, true)}
                  onKeyDown={(event) => {
                    let targetIndex: number | undefined;
                    if (event.key === "ArrowRight" || event.key === "ArrowDown") {
                      targetIndex = (index + 1) % layout.stageOrder.length;
                    } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
                      targetIndex =
                        (index - 1 + layout.stageOrder.length) % layout.stageOrder.length;
                    } else if (event.key === "Home") {
                      targetIndex = 0;
                    } else if (event.key === "End") {
                      targetIndex = layout.stageOrder.length - 1;
                    }
                    if (targetIndex !== undefined) {
                      event.preventDefault();
                      moveSelection(targetIndex);
                    }
                  }}
                  ref={(element) => {
                    if (element) {
                      nodeRefs.current.set(node.id, element);
                    } else {
                      nodeRefs.current.delete(node.id);
                    }
                  }}
                  style={{ left: layoutNode.x, top: layoutNode.y }}
                  type="button"
                >
                  {node.kind === "gate" && <GateShape />}
                  <span className="graph-node-kind">{nodeKindLabel(node)}</span>
                  <strong>{node.id}</strong>
                  <span className="workflow-node-actor">{actor}</span>
                  <span className="graph-node-state">{stateText}</span>
                </button>
              );
            })}
          </div>
        </div>
        <TopologyList
          branchStates={branchStates}
          graph={graph}
          layoutEdges={layout.edges}
          stageOrder={layout.stageOrder}
          traversedEdges={traversedEdges}
        />
        <WorkflowGraphLegend />
      </div>
    </div>
  );
}

// GateShape draws the decision-point silhouette a gate node wears instead of
// the shared rounded rectangle (#2693). It is a layer inside the button rather
// than a clip-path on the button so the focus ring, the selection ring, and
// the escalation-cause outline — all painted outside the border box — survive.
// The left and right points sit at the node's vertical midline, exactly where
// layoutWorkflowGraph anchors forward edges, and the flat top and bottom keep
// the four stacked labels legible at the node's 172x86 footprint, which a
// full-width rhombus would pinch away.
function GateShape() {
  return (
    <svg
      aria-hidden="true"
      className="workflow-node-shape"
      preserveAspectRatio="none"
      viewBox="0 0 100 100"
    >
      <polygon points="0,50 14,0 86,0 100,50 86,100 14,100" />
    </svg>
  );
}

function midpoint(left: PointerPosition, right: PointerPosition): PointerPosition {
  return {
    x: (left.x + right.x) / 2,
    y: (left.y + right.y) / 2,
  };
}

function distance(left: PointerPosition, right: PointerPosition): number {
  return Math.hypot(right.x - left.x, right.y - left.y);
}

// branchEdgeLabel is the non-color-dependent text for a parallel's fan-out
// edge — naming the branch, and once it has a recorded state, appending it
// ("security-lens — Failed") so a cancelled branch reads differently from a
// failed one by the words alone, per #1567's acceptance criteria.
function branchEdgeLabel(branchName: string, state: BranchStateEntry | undefined): string {
  return state ? `${branchName} — ${branchStateLabel(state.state)}` : branchName;
}

// edgeDescription is the accessible, non-color-dependent explanation of one
// edge's declared shape and (when a run overlay is active) execution state —
// #1431's "Configured repass route; not traversed" / "Traversed at sequence
// 23" wording, so the topology list communicates the same two independent
// dimensions the visual encoding does (repass vs. forward; traversed vs.
// declared-only) rather than a bare boolean.
function edgeDescription(
  layoutEdge: WorkflowLayoutEdge,
  traversedEdges: TraversedEdges | undefined,
  branchStates: Map<string, BranchStateEntry> | undefined,
): string {
  const { edge } = layoutEdge;
  const target = edge.terminal ? `${terminalLabel(edge.terminal)} terminal` : edge.target;
  const route = edge.branch
    ? `branch ${branchEdgeLabel(edge.branch, branchStates?.get(edge.branch))} to ${target}`
    : `${edge.outcome || "next"} to ${target}`;
  const shape = layoutEdge.repass ? "repass route" : "forward route";
  if (traversedEdges === undefined) {
    // No run overlay active (the configured-topology page): only the
    // declared shape is meaningful, execution state does not apply.
    return `${route}, configured ${shape}`;
  }
  const seq = traversedEdgeSeq(traversedEdges, edge);
  const state = seq === undefined ? "not traversed" : `traversed at sequence ${seq}`;
  return `${route}, configured ${shape}, ${state}`;
}

function TopologyList({
  graph,
  layoutEdges,
  stageOrder,
  traversedEdges,
  branchStates,
}: {
  graph: WorkflowGraph;
  layoutEdges: WorkflowLayoutEdge[];
  stageOrder: WorkflowGraphNode[];
  traversedEdges?: TraversedEdges;
  branchStates?: Map<string, BranchStateEntry>;
}) {
  return (
    <ol aria-label={`${graph.name} accessible topology`} className="sr-only">
      {stageOrder.map((node) => {
        const outgoing = layoutEdges
          .filter((layoutEdge) => layoutEdge.edge.source === node.id)
          .map((layoutEdge) => edgeDescription(layoutEdge, traversedEdges, branchStates));
        return (
          <li key={`topology-${node.id}`}>
            {node.id === graph.start ? "Start stage. " : ""}
            {node.id}, {nodeKindLabel(node)}, {nodeActor(node)}, configured.{" "}
            {outgoing.length > 0 ? `Outgoing: ${outgoing.join("; ")}.` : "No outgoing target."}
          </li>
        );
      })}
    </ol>
  );
}

function nodeKindLabel(node: WorkflowGraphNode): string {
  switch (node.kind) {
    case "agentic":
      return "Agentic task";
    case "deterministic":
      return "Deterministic task";
    case "gate":
      return "Gate";
    case "parallel":
      return "Parallel";
  }
}

function nodeActor(node: WorkflowGraphNode): string {
  if (node.kind === "gate") {
    const evaluator = node.evaluator ? `${node.evaluator} evaluator` : "Evaluator not declared";
    return node.owner ? `${evaluator}, owned by ${node.owner}` : evaluator;
  }
  if (node.kind === "agentic") {
    return node.owner ? `Owned by ${node.owner}` : "Owner not declared";
  }
  if (node.kind === "parallel") {
    return "Fans out into declared branches";
  }
  return "Runs deterministically";
}
