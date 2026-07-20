import { useId, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { WorkflowGraph, WorkflowGraphNode } from "../api/types";
import {
  MAX_GRAPH_ZOOM,
  MIN_GRAPH_ZOOM,
  clampGraphZoom,
  fitGraphZoom,
  layoutWorkflowGraph,
  terminalLabel,
} from "../workflowGraph";

const FALLBACK_VIEWPORT_WIDTH = 720;
const FALLBACK_VIEWPORT_HEIGHT = 360;
const PAN_DISTANCE = 120;

export function WorkflowTopologyGraph({
  graph,
  onSelectStage,
  selectedStageId,
}: {
  graph: WorkflowGraph;
  onSelectStage: (stageId: string) => void;
  selectedStageId?: string;
}) {
  const layout = useMemo(() => layoutWorkflowGraph(graph), [graph]);
  const markerId = `workflow-arrow-${useId().replaceAll(":", "")}`;
  const viewportRef = useRef<HTMLDivElement | null>(null);
  const nodeRefs = useRef(new Map<string, HTMLButtonElement>());
  const [viewportSize, setViewportSize] = useState({
    width: FALLBACK_VIEWPORT_WIDTH,
    height: FALLBACK_VIEWPORT_HEIGHT,
  });
  const [fitActive, setFitActive] = useState(true);
  const [zoom, setZoom] = useState(() =>
    fitGraphZoom(
      layout.width,
      layout.height,
      FALLBACK_VIEWPORT_WIDTH,
      FALLBACK_VIEWPORT_HEIGHT,
    ),
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
    setZoom(
      fitGraphZoom(
        layout.width,
        layout.height,
        viewportSize.width,
        viewportSize.height,
      ),
    );
    const viewport = viewportRef.current;
    if (viewport) {
      viewport.scrollLeft = 0;
      viewport.scrollTop = 0;
    }
  }, [fitActive, layout.height, layout.width, viewportSize.height, viewportSize.width]);

  if (layout.nodes.length === 0) {
    return (
      <div className="empty-detail" role="status">
        <strong>No stages in this workflow definition</strong>
      </div>
    );
  }

  const navigationRequired =
    layout.width > viewportSize.width || layout.height > viewportSize.height;
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
    setFitActive(false);
    setZoom((current) => clampGraphZoom(current + change));
  };
  const fit = () => {
    setFitActive(true);
    setZoom(
      fitGraphZoom(
        layout.width,
        layout.height,
        viewportSize.width,
        viewportSize.height,
      ),
    );
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
    if (typeof viewport.scrollBy === "function") {
      viewport.scrollBy({ behavior: "auto", left, top });
    } else {
      viewport.scrollLeft += left;
      viewport.scrollTop += top;
    }
  };

  return (
    <div className="workflow-graph-shell">
      {navigationRequired && (
        <div aria-label="Graph view controls" className="workflow-graph-controls" role="group">
          <button onClick={() => pan(-PAN_DISTANCE, 0)} type="button">
            Pan left
          </button>
          <button onClick={() => pan(PAN_DISTANCE, 0)} type="button">
            Pan right
          </button>
          <button onClick={() => pan(0, -PAN_DISTANCE)} type="button">
            Pan up
          </button>
          <button onClick={() => pan(0, PAN_DISTANCE)} type="button">
            Pan down
          </button>
          <span aria-hidden="true" className="graph-control-divider" />
          <button
            aria-label="Zoom out"
            disabled={zoom <= MIN_GRAPH_ZOOM}
            onClick={() => changeZoom(-0.1)}
            type="button"
          >
            -
          </button>
          <output aria-live="polite" className="graph-zoom-value">
            {Math.round(zoom * 100)}%
          </output>
          <button
            aria-label="Zoom in"
            disabled={zoom >= MAX_GRAPH_ZOOM}
            onClick={() => changeZoom(0.1)}
            type="button"
          >
            +
          </button>
          <button onClick={fit} type="button">
            Fit
          </button>
        </div>
      )}
      <div
        aria-label={`${graph.name} execution graph`}
        className="workflow-graph-viewport"
        data-responsive-layout="scroll-under-820"
        data-zoom={zoom.toFixed(2)}
        ref={viewportRef}
        role="group"
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
              {layout.edges.map((edge) => (
                <g key={edge.id}>
                  <path
                    className={`workflow-graph-edge ${edge.repass ? "workflow-graph-edge-repass" : ""}`}
                    d={edge.path}
                    markerEnd={`url(#${markerId})`}
                  />
                  {edge.edge.outcome && (
                    <text
                      className="workflow-graph-edge-label"
                      x={edge.labelX}
                      y={edge.labelY}
                    >
                      {edge.edge.outcome}
                    </text>
                  )}
                </g>
              ))}
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
              return (
                <button
                  aria-label={`${node.id}, ${nodeKindLabel(node)}, ${actor}, configured`}
                  aria-pressed={selectedStageId === node.id}
                  className={`workflow-graph-node workflow-node-${node.kind}`}
                  data-node-kind={node.kind}
                  key={node.id}
                  onClick={() => onSelectStage(node.id)}
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
                  <span className="graph-node-kind">{nodeKindLabel(node)}</span>
                  <strong>{node.id}</strong>
                  <span className="workflow-node-actor">{actor}</span>
                  <span className="graph-node-state">Configured</span>
                </button>
              );
            })}
          </div>
        </div>
        <TopologyList graph={graph} stageOrder={layout.stageOrder} />
      </div>
    </div>
  );
}

function TopologyList({
  graph,
  stageOrder,
}: {
  graph: WorkflowGraph;
  stageOrder: WorkflowGraphNode[];
}) {
  return (
    <ol aria-label={`${graph.name} accessible topology`} className="sr-only">
      {stageOrder.map((node) => {
        const outgoing = graph.edges
          .filter((edge) => edge.source === node.id)
          .map((edge) => {
            const target = edge.terminal ? `${terminalLabel(edge.terminal)} terminal` : edge.target;
            return `${edge.outcome || "next"} to ${target}`;
          });
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
  return "Runs deterministically";
}
