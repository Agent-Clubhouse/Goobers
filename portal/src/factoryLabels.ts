import type { GraphTerminal } from "./api/types";
import type {
  FactoryCarrier,
  FactoryLane,
  FactoryStation,
  FactoryWorker,
  FactoryWorkerPlacement,
} from "./factoryModel";
import {
  capacityLabel,
  holdReasonLabel,
  runStateLabel,
  stageKindLabel,
  stationStatusLabel,
} from "./factoryModel";

/**
 * The words both Factory Floor layouts use.
 *
 * A line and a machine are the same fact whether they are drawn as a topology
 * row or as a building in the plant, so the accessible name is written once
 * here and shared. That is what lets an operator switch layout without
 * relearning the floor, and what lets the tests assert that both layouts
 * describe exactly the same entities.
 *
 * Every string is built from operational identifiers and closed-set states.
 * Nothing here reads journal text, error messages, repository refs or trigger
 * refs.
 */

export function laneLabel(lane: FactoryLane, partial: boolean): string {
  const limit =
    lane.limit === undefined
      ? "workflow limit unknown"
      : `workflow limit ${lane.limit}`;
  const topology =
    lane.source === "observed"
      ? lane.stations.length === 0
        ? ` Workflow topology was not read in this batch. ${lane.stageCount} stages are configured and none are drawn.`
        : ` Workflow definition unread; showing ${lane.stations.length} observed stages with order unknown.`
      : "";
  const unread =
    lane.unreadRuns > 0 ? ` ${lane.unreadRuns} run signals unread.` : "";
  return `Workflow ${lane.displayName}, gaggle ${lane.gaggleDisplayName}. ${lane.activeRuns}${partial ? " or more" : ""} active runs, ${limit}. ${lane.blockedRuns} held.${unread}${topology}`;
}

export function stationLabel(
  station: FactoryStation,
  workers: readonly FactoryWorker[],
): string {
  const parts = [
    `Stage ${station.stageId}`,
    stageKindLabel(station.kind),
    `workflow ${station.workflowDisplayName}`,
    `gaggle ${station.gaggle}`,
    stationStatusLabel(station.status),
    capacityLabel(station.wip, station.limit),
  ];
  if (station.alarm === "blocked") {
    parts.push("blocked alarm: every run here is held and hard blocked work is present");
  }
  if (station.alarm === "hold") {
    parts.push("human hold alarm: every run here is paused at a human gate");
  }
  if (station.unknownCount > 0) {
    parts.push(`${station.unknownCount} run signals unread`);
  }
  if (workers.length > 0) {
    parts.push(`staffed by ${workers.map((worker) => worker.displayName).join(", ")}`);
  } else if (station.owner) {
    parts.push(`owner ${station.owner.displayName ?? station.owner.name}`);
  }
  if (station.source === "observed") {
    parts.push("stage observed from live runs; workflow definition unread");
  }
  return `${parts.join(". ")}.`;
}

export function carrierLabel(carrier: FactoryCarrier): string {
  const where = carrier.stageId
    ? `at stage ${carrier.stageId}`
    : "waiting to enter its first stage";
  const reason = holdReasonLabel(carrier.reason);
  const moved =
    carrier.transition?.kind === "stage-change"
      ? ` Moved from ${carrier.transition.fromStageId ?? "the inbound yard"}.`
      : "";
  return `Run ${carrier.runId}, workflow ${carrier.workflowDisplayName}, ${where}. ${runStateLabel(
    carrier.state,
  )}.${reason ? ` ${reason}.` : ""}${moved}`;
}

export function workerLabel(
  worker: FactoryWorker,
  placement: FactoryWorkerPlacement,
): string {
  if (!placement.active) {
    return `Goober ${worker.displayName}, gaggle ${worker.gaggleDisplayName}. Idle in the ready commons. Owns ${worker.stages.length} stages.`;
  }
  return `Goober ${worker.displayName}, gaggle ${worker.gaggleDisplayName}. Working ${worker.activeRunCount} active runs across ${worker.activeStationIds.length} owned stages.`;
}

/** The short word on a machine readout. Never colour alone. */
export function machineStatusText(station: FactoryStation): string {
  switch (station.status) {
    case "held":
      return "HOLD";
    case "blocked":
      return "BLOCKED";
    case "impeded":
      return "PARTIAL";
    case "unknown":
      return "UNREAD";
    case "running":
      return "RUNNING";
    case "idle":
      return "IDLE";
  }
}

export function shortKind(station: FactoryStation): string {
  switch (station.kind) {
    case "gate":
      return "gate";
    case "agentic":
      return "agentic";
    case "deterministic":
      return "deterministic";
    case "parallel":
      return "parallel";
  }
}

export function dockLabel(terminal: GraphTerminal): string {
  switch (terminal) {
    case "complete":
      return "Shipping";
    case "escalate":
      return "Escalation";
    case "abort":
      return "Abort";
  }
}
