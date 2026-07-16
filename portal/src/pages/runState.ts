import type { NodeState, RunEvent, Workflow } from "../prototypeData";

export function deriveNodeStates(
  workflow: Workflow,
  events: RunEvent[],
  index: number,
): Record<string, NodeState> {
  const states = Object.fromEntries(workflow.stages.map((stage) => [stage.id, "pending" as NodeState]));
  let activeStageId: string | undefined;
  for (const event of events.slice(0, index + 1)) {
    if (!event.stageId) {
      continue;
    }
    if (event.type === "stage.started") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = "active";
      activeStageId = event.stageId;
    } else if (event.type === "stage.finished") {
      states[event.stageId] = event.tone === "danger" ? "failed" : "complete";
      if (activeStageId === event.stageId) {
        activeStageId = undefined;
      }
    } else if (event.type === "gate.evaluated") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = event.tone === "danger" ? "escalated" : "complete";
      activeStageId = undefined;
    } else if (event.type === "run.finished") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = event.tone === "danger" ? "escalated" : "complete";
      activeStageId = undefined;
    }
  }
  return states;
}

export function deriveTraversedEdges(events: RunEvent[], index: number): Set<string> {
  const traversed = new Set<string>();
  let currentStageId: string | undefined;
  for (const event of events.slice(0, index + 1)) {
    if (!event.stageId) {
      continue;
    }
    const entersStage =
      event.type === "stage.started" ||
      event.type === "stage.finished" ||
      event.type === "gate.evaluated" ||
      event.type === "run.finished";
    if (!entersStage) {
      continue;
    }
    if (currentStageId && currentStageId !== event.stageId) {
      traversed.add(`${currentStageId}->${event.stageId}`);
    }
    currentStageId = event.stageId;
  }
  return traversed;
}

export function traversalEdgeAtEvent(events: RunEvent[], index: number): string | undefined {
  const current = events[index];
  if (!current?.stageId || !["stage.started", "gate.evaluated", "run.finished"].includes(current.type)) {
    return undefined;
  }
  for (let previousIndex = index - 1; previousIndex >= 0; previousIndex -= 1) {
    const previous = events[previousIndex];
    if (!previous.stageId || !["stage.started", "gate.evaluated", "run.finished"].includes(previous.type)) {
      continue;
    }
    return previous.stageId === current.stageId
      ? undefined
      : `${previous.stageId}->${current.stageId}`;
  }
  return undefined;
}
