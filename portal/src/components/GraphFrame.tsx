import { useId, useMemo, type ReactNode } from "react";
import type { NodeState, Workflow } from "../prototypeData";

export interface TopologyGraphProps {
  workflow: Workflow;
  states?: Record<string, NodeState>;
  activeEdges?: ReadonlySet<string>;
  traversingEdge?: string;
  selectedStageId?: string;
  onSelectStage: (stageId: string) => void;
}

export function TopologyGraph({
  workflow,
  states,
  activeEdges,
  traversingEdge,
  selectedStageId,
  onSelectStage,
}: TopologyGraphProps) {
  const descriptionId = useId();
  const stageById = useMemo(
    () => new Map(workflow.stages.map((stage) => [stage.id, stage])),
    [workflow.stages],
  );

  return (
    <div
      aria-describedby={descriptionId}
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
      {workflow.stages.map((stage) => {
        const state = states?.[stage.id] ?? "pending";
        return (
          <button
            aria-label={`${stage.name}, ${stage.kind}, ${state}`}
            aria-pressed={selectedStageId === stage.id}
            className={`graph-node node-${stage.kind} node-${state} ${
              selectedStageId === stage.id ? "graph-node-selected" : ""
            }`}
            key={stage.id}
            onClick={() => onSelectStage(stage.id)}
            style={{ left: `${stage.x}%`, top: `${stage.y}%` }}
            type="button"
          >
            <span className="graph-node-kind">{stage.kind}</span>
            <strong>{stage.name}</strong>
            {states && <span className="graph-node-state">{state}</span>}
          </button>
        );
      })}
      <ul className="sr-only" id={descriptionId}>
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
  );
}

export function GraphFrame({
  action,
  className,
  legend = "Select a stage to inspect it",
  ...graphProps
}: TopologyGraphProps & { action?: ReactNode; className?: string; legend?: string }) {
  return (
    <div className={`graph-panel ${className ?? ""}`.trim()}>
      <div className="panel-heading-row">
        <div>
          <p className="section-kicker">Structure</p>
          <h2>Execution graph</h2>
        </div>
        {action ?? <span className="graph-legend">{legend}</span>}
      </div>
      <TopologyGraph {...graphProps} />
    </div>
  );
}
