import { useMemo, useRef } from "react";
import type { NodeState, Workflow } from "../prototypeData";

interface GraphFrameProps {
  action?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  eyebrow?: string;
  title?: string;
}

export function GraphFrame({
  action,
  children,
  className = "",
  eyebrow = "Structure",
  title = "Execution graph",
}: GraphFrameProps) {
  return (
    <div className={`graph-panel ${className}`.trim()}>
      <div className="panel-heading-row">
        <div>
          <p className="section-kicker">{eyebrow}</p>
          <h2>{title}</h2>
        </div>
        {action}
      </div>
      {children}
    </div>
  );
}

interface TopologyGraphProps {
  activeEdges?: ReadonlySet<string>;
  causalStageId?: string;
  onSelectStage: (stageId: string) => void;
  selectedStageId?: string;
  states?: Record<string, NodeState>;
  traversingEdge?: string;
  workflow: Workflow;
}

export function TopologyGraph({
  workflow,
  states,
  activeEdges,
  traversingEdge,
  selectedStageId,
  causalStageId,
  onSelectStage,
}: TopologyGraphProps) {
  const stageById = useMemo(
    () => new Map(workflow.stages.map((stage) => [stage.id, stage])),
    [workflow.stages],
  );
  const nodeRefs = useRef<Array<HTMLButtonElement | null>>([]);

  const moveSelection = (index: number, direction: number) => {
    const targetIndex = (index + direction + workflow.stages.length) % workflow.stages.length;
    const target = workflow.stages[targetIndex];
    if (target) {
      onSelectStage(target.id);
      nodeRefs.current[targetIndex]?.focus();
    }
  };

  return (
    <div
      aria-label={`${workflow.name} execution graph`}
      className="graph-canvas"
      data-responsive-layout="compact-under-820"
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
        const causal = causalStageId === stage.id;
        return (
          <button
            aria-label={`${stage.name}, ${state}${causal ? ", causal event" : ""}`}
            aria-pressed={selectedStageId === stage.id}
            className={`graph-node node-${stage.kind} node-${state} ${
              selectedStageId === stage.id ? "graph-node-selected" : ""
            } ${causal ? "graph-node-causal" : ""}`}
            key={stage.id}
            onClick={() => onSelectStage(stage.id)}
            onKeyDown={(event) => {
              if (event.key === "ArrowRight" || event.key === "ArrowDown") {
                event.preventDefault();
                moveSelection(index, 1);
              } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
                event.preventDefault();
                moveSelection(index, -1);
              }
            }}
            ref={(node) => {
              nodeRefs.current[index] = node;
            }}
            style={{ left: `${stage.x}%`, top: `${stage.y}%` }}
            type="button"
          >
            <span className="graph-node-kind">{stage.kind === "gate" ? "gate" : stage.kind}</span>
            <strong>{stage.name}</strong>
            <span className="graph-node-state">{state}</span>
            {causal && <span className="graph-node-cause">Causal event</span>}
          </button>
        );
      })}
      <ul className="sr-only">
        {workflow.stages.map((stage) => {
          const state = states?.[stage.id] ?? "pending";
          const outgoing = workflow.edges
            .filter((edge) => edge.from === stage.id)
            .map((edge) => `${edge.label ?? "next"} to ${stageById.get(edge.to)?.name ?? edge.to}`);
          return (
            <li key={`topology-${stage.id}`}>
              {stage.name}, {stage.kind}, {state}.{" "}
              {outgoing.length > 0 ? `Outgoing: ${outgoing.join("; ")}.` : "Terminal stage."}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
