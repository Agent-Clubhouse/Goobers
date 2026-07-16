import { useId, useMemo, useRef } from "react";
import type { NodeState, Workflow } from "../fixtures";

export function GraphFrame({
  workflow,
  states,
  activeEdges,
  traversingEdge,
  selectedStageId,
  onSelectStage,
}: {
  workflow: Workflow;
  states?: Record<string, NodeState>;
  activeEdges?: ReadonlySet<string>;
  traversingEdge?: string;
  selectedStageId?: string;
  onSelectStage: (stageId: string) => void;
}) {
  const topologyId = useId();
  const nodeRefs = useRef(new Map<string, HTMLButtonElement>());
  const stageById = useMemo(
    () => new Map(workflow.stages.map((stage) => [stage.id, stage])),
    [workflow.stages],
  );

  const focusStage = (currentIndex: number, direction: "first" | "last" | "next" | "previous") => {
    let nextIndex = currentIndex;
    if (direction === "first") {
      nextIndex = 0;
    } else if (direction === "last") {
      nextIndex = workflow.stages.length - 1;
    } else if (direction === "next") {
      nextIndex = (currentIndex + 1) % workflow.stages.length;
    } else {
      nextIndex = (currentIndex - 1 + workflow.stages.length) % workflow.stages.length;
    }
    nodeRefs.current.get(workflow.stages[nextIndex]?.id)?.focus();
  };

  return (
    <div
      aria-label={`Scrollable ${workflow.name} execution graph`}
      className="graph-viewport"
      role="region"
      tabIndex={0}
    >
      <div
        aria-describedby={topologyId}
        aria-label={`${workflow.name} execution graph`}
        className="graph-canvas"
        role="group"
      >
        <svg aria-hidden="true" className="graph-edges" preserveAspectRatio="none" viewBox="0 0 100 100">
          {workflow.edges.map((edge) => {
            const from = stageById.get(edge.from);
            const to = stageById.get(edge.to);
            if (!from || !to) {
              return null;
            }
            const edgeKey = `${edge.from}->${edge.to}`;
            const active = activeEdges?.has(edgeKey) ?? false;
            const path = edge.repass
              ? `M ${from.x} ${from.y + 9} C ${from.x} 88, ${to.x} 88, ${to.x} ${to.y + 9}`
              : `M ${from.x + 5} ${from.y} L ${to.x - 5} ${to.y}`;
            return (
              <g key={`${edge.from}-${edge.to}-${edge.label ?? "next"}`}>
                <path
                  className={[
                    "graph-edge",
                    active ? "graph-edge-active" : "",
                    traversingEdge === edgeKey ? "graph-edge-traversing" : "",
                  ].filter(Boolean).join(" ")}
                  d={path}
                  data-edge={edgeKey}
                />
                {edge.label && (
                  <text className="graph-edge-label" x={(from.x + to.x) / 2} y={edge.repass ? 87 : from.y - 5}>
                    {edge.label}
                  </text>
                )}
              </g>
            );
          })}
        </svg>
        {workflow.stages.map((stage, index) => {
          const state = states?.[stage.id] ?? "pending";
          return (
            <button
              aria-label={`${stage.name}, ${state}`}
              aria-pressed={selectedStageId === stage.id}
              className={`graph-node node-${stage.kind} node-${state} ${
                selectedStageId === stage.id ? "graph-node-selected" : ""
              }`}
              key={stage.id}
              onClick={() => onSelectStage(stage.id)}
              onKeyDown={(event) => {
                if (event.key === "ArrowRight" || event.key === "ArrowDown") {
                  event.preventDefault();
                  focusStage(index, "next");
                } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
                  event.preventDefault();
                  focusStage(index, "previous");
                } else if (event.key === "Home") {
                  event.preventDefault();
                  focusStage(index, "first");
                } else if (event.key === "End") {
                  event.preventDefault();
                  focusStage(index, "last");
                }
              }}
              ref={(node) => {
                if (node) {
                  nodeRefs.current.set(stage.id, node);
                } else {
                  nodeRefs.current.delete(stage.id);
                }
              }}
              style={{ left: `${stage.x}%`, top: `${stage.y}%` }}
              type="button"
            >
              <span className="graph-node-kind">{stage.kind === "gate" ? "gate" : stage.kind}</span>
              <strong>{stage.name}</strong>
              <span className="graph-node-state">{state}</span>
            </button>
          );
        })}
        <ul className="sr-only" id={topologyId}>
          {workflow.stages.map((stage) => {
            const outgoing = workflow.edges
              .filter((edge) => edge.from === stage.id)
              .map((edge) => `${edge.label ?? "next"} to ${stageById.get(edge.to)?.name ?? edge.to}`);
            return (
              <li key={`topology-${stage.id}`}>
                {stage.name}, {stage.kind}. {outgoing.length > 0 ? `Outgoing: ${outgoing.join("; ")}.` : "Terminal stage."}
              </li>
            );
          })}
        </ul>
      </div>
    </div>
  );
}
