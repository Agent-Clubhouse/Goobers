import type {
  NodeState,
  Run,
  RunEvent,
  StageAttempt,
  Workflow,
} from "./prototypeData";

export function deriveNodeStates(
  workflow: Workflow,
  events: RunEvent[],
  index: number,
): Record<string, NodeState> {
  const states = Object.fromEntries(
    workflow.stages.map((stage) => [stage.id, "pending" as NodeState]),
  );
  let activeStageId: string | undefined;
  for (const event of events.slice(0, index + 1)) {
    if (!event.stageId) {
      continue;
    }
    if (event.type === "stage.started") {
      if (
        activeStageId &&
        activeStageId !== event.stageId &&
        states[activeStageId] === "active"
      ) {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = "active";
      activeStageId = event.stageId;
    } else if (event.type === "stage.finished") {
      states[event.stageId] = event.tone === "danger" ? "failed" : "complete";
      if (activeStageId === event.stageId) {
        activeStageId = undefined;
      }
    } else if (event.type === "gate.evaluated" || event.type === "condition.evaluated") {
      if (
        activeStageId &&
        activeStageId !== event.stageId &&
        states[activeStageId] === "active"
      ) {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = event.tone === "danger" ? "escalated" : "complete";
      activeStageId = undefined;
    } else if (event.type === "run.finished") {
      if (
        activeStageId &&
        activeStageId !== event.stageId &&
        states[activeStageId] === "active"
      ) {
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
      event.type === "condition.evaluated" ||
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
  if (
    !current?.stageId ||
    !["stage.started", "gate.evaluated", "condition.evaluated", "run.finished"].includes(current.type)
  ) {
    return undefined;
  }
  for (let previousIndex = index - 1; previousIndex >= 0; previousIndex -= 1) {
    const previous = events[previousIndex];
    if (
      !previous.stageId ||
      !["stage.started", "gate.evaluated", "condition.evaluated", "run.finished"].includes(previous.type)
    ) {
      continue;
    }
    return previous.stageId === current.stageId
      ? undefined
      : `${previous.stageId}->${current.stageId}`;
  }
  return undefined;
}

export function visibleAttempts(
  run: Run,
  stageId: string,
  eventSeq: number,
): StageAttempt[] {
  return run.attempts
    .filter((attempt) => attempt.stageId === stageId && attempt.startedSeq <= eventSeq)
    .map((attempt) => {
      if (attempt.endedSeq !== undefined && attempt.endedSeq <= eventSeq) {
        return attempt;
      }
      return {
        ...attempt,
        status: "running",
        duration: "In progress",
        output: undefined,
        artifacts: [],
      };
    });
}

/**
 * Locates the durable event that caused an escalated run to stop, so the UI
 * can anchor its escalation explanation to a specific, replayable event
 * instead of just "the end of the journal".
 */
export function causalEventIndex(run: Run, events: RunEvent[]): number | undefined {
  if (run.status !== "escalated" || !run.escalation) {
    return undefined;
  }
  const index = events.findIndex((event) => event.seq === run.escalation?.causalEventSeq);
  return index >= 0 ? index : undefined;
}
