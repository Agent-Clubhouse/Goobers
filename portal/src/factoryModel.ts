import type {
  DefinitionStatus,
  EvaluatorKind,
  GraphNodeKind,
  GraphTerminal,
  Goober,
  Harness,
  RunEvent,
  RunPhase,
  RunSummary,
  RunTriggerKind,
  StageAttempt,
  WorkflowDetail,
} from "./api/types";
import type { GaggleInventory } from "./operationalData";
import { layoutWorkflowGraph } from "./workflowGraph";

/**
 * The Factory Floor view model.
 *
 * Everything here is derived from daemon reads: configured gaggles, goobers and
 * workflows, the pinned workflow graph, and the runs the daemon reports as
 * `phase=running`. Nothing is simulated. A stage exists because the workflow
 * declares it (or because a live run is standing on it); a work carrier exists
 * because a run is active; a goober stands at a stage because it owns that
 * stage and the stage currently holds work.
 *
 * The model is deliberately pure and layout-complete: geometry is a function of
 * the daemon data alone, with no randomness or wall clock, so two builds of
 * the same snapshot place every object at exactly the same coordinate. That is
 * what lets the view animate ONLY on a real stage transition instead of
 * drifting on every refresh.
 *
 * Privacy: only operational identifiers already meant for portal surfaces enter
 * this model: display names, stage ids, run ids, phases, timings, counts,
 * attempt statuses and closed-set reason categories. Free-form journal text,
 * error messages, artifact/transcript metadata, repository refs, external URLs
 * and trigger refs are deliberately excluded (see `factoryModel.test.ts`).
 */

const FLOOR_PADDING_X = 44;
const FLOOR_PADDING_TOP = 30;
const FLOOR_PADDING_BOTTOM = 28;
const FLOOR_MIN_WIDTH = 860;
const LANE_GAP = 24;
const LANE_HEADER_HEIGHT = 38;
const LANE_PADDING_TOP = 12;
const LANE_PADDING_BOTTOM = 22;
const YARD_WIDTH = 84;
const YARD_GAP = 20;
const STATION_WIDTH = 150;
const STATION_HEIGHT = 88;
const STATION_APRON_HEIGHT = 88;
const COLUMN_GAP = 66;
const ROW_GAP = 28;
const DOCK_WIDTH = 92;
const DOCK_HEIGHT = 52;
const DOCK_GAP = 14;
const REPASS_LANE_HEIGHT = 16;
const CARRIER_WIDTH = 28;
const CARRIER_HEIGHT = 22;
const CARRIER_GAP = 6;
const CARRIERS_PER_ROW = 3;
const YARD_CARRIERS_PER_ROW = 2;
const COMMONS_GAP = 22;
const COMMONS_HEIGHT = 104;
const WORKER_WIDTH = 30;
const WORKER_HEIGHT = 34;
const WORKER_GAP = 12;

export const FACTORY_RENDERED_CARRIERS_PER_STATION = 6;
export const FACTORY_RENDERED_CARRIERS_PER_YARD = 4;
export const FACTORY_RENDERED_WORKERS_PER_STATION = 1;
export const FACTORY_RENDERED_COMMONS_WORKERS = 12;

export type FactoryLens = "world" | "flow" | "risk";

export const FACTORY_LENSES: readonly FactoryLens[] = ["world", "flow", "risk"];

export function isFactoryLens(value: string | undefined): value is FactoryLens {
  return value === "world" || value === "flow" || value === "risk";
}

export interface FactoryScope {
  gaggle?: string;
  workflow?: string;
}

/** Why a run is not making progress, as a closed set. Never journal text. */
export type FactoryHoldReason =
  | "human-gate"
  | "stage-blocked"
  | "attempt-retry"
  | "awaiting-stage";

export type FactoryRunState =
  | "running"
  | "paused"
  | "blocked"
  | "starting"
  | "unknown";

export interface FactoryRunSignal {
  state: FactoryRunState;
  reason?: FactoryHoldReason;
  /** False only when the stage-level read was unavailable. */
  confirmed: boolean;
}

export type FactoryStationStatus =
  | "idle"
  | "running"
  | "impeded"
  | "held"
  | "blocked"
  | "unknown";

export type FactoryAlarmKind = "hold" | "blocked";

export type FactoryTopologySource = "declared" | "observed";

export interface FactoryStation {
  id: string;
  laneId: string;
  stageId: string;
  gaggle: string;
  workflow: string;
  workflowDisplayName: string;
  kind: GraphNodeKind;
  evaluator?: EvaluatorKind;
  owner?: { gaggle: string; name: string; displayName?: string };
  source: FactoryTopologySource;
  isStart: boolean;
  column: number;
  row: number;
  x: number;
  y: number;
  width: number;
  height: number;
  /** Runs currently standing on this stage. */
  wip: number;
  /** The workflow's concurrency limit; undefined when the daemon reports none. */
  limit?: number;
  saturation?: number;
  blockedCount: number;
  hardBlockedCount: number;
  pausedCount: number;
  unknownCount: number;
  status: FactoryStationStatus;
  alarm?: FactoryAlarmKind;
  runIds: string[];
  renderedRunIds: string[];
  overflowRunCount: number;
  workerIds: string[];
  renderedWorkerIds: string[];
  workerOverflowCount: number;
}

export interface FactoryYard {
  id: string;
  laneId: string;
  x: number;
  y: number;
  width: number;
  height: number;
  runIds: string[];
  renderedRunIds: string[];
  overflowRunCount: number;
}

export interface FactoryDock {
  id: string;
  laneId: string;
  terminal: GraphTerminal;
  x: number;
  y: number;
  width: number;
  height: number;
}

export type FactoryConveyorKind = "forward" | "repass" | "terminal";

export interface FactoryConveyor {
  id: string;
  laneId: string;
  kind: FactoryConveyorKind;
  fromStationId: string;
  toId: string;
  outcome?: string;
  terminal?: GraphTerminal;
  path: string;
  labelX: number;
  labelY: number;
  /** True only when a run actually moved along this edge between snapshots. */
  active: boolean;
}

export interface FactoryLane {
  id: string;
  gaggle: string;
  gaggleDisplayName: string;
  workflow: string;
  displayName: string;
  source: FactoryTopologySource;
  stageCount: number;
  stations: FactoryStation[];
  docks: FactoryDock[];
  conveyors: FactoryConveyor[];
  yard: FactoryYard;
  activeRuns: number;
  blockedRuns: number;
  unreadRuns: number;
  limit?: number;
  saturation?: number;
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface FactoryCarrierTransition {
  kind: "stage-change" | "arrival";
  fromStageId?: string;
  fromStationId?: string;
}

export interface FactoryCarrier {
  runId: string;
  gaggle: string;
  workflow: string;
  workflowDisplayName: string;
  laneId: string;
  stageId?: string;
  stationId: string;
  phase: RunPhase;
  state: FactoryRunState;
  reason?: FactoryHoldReason;
  confirmed: boolean;
  triggerKind: RunTriggerKind;
  startedAt: string;
  lastActivityAt: string;
  durationMillis: number;
  retryCount: number;
  policyRetryCount: number;
  infraRetryCount: number;
  repassCount: number;
  ownerWorkerId?: string;
  queueIndex: number;
  rendered: boolean;
  renderSlot?: number;
  x: number;
  y: number;
  transition?: FactoryCarrierTransition;
}

export interface FactoryWorkerPlacement {
  id: string;
  workerId: string;
  stationId?: string;
  x: number;
  y: number;
  active: boolean;
  rendered: boolean;
}

export interface FactoryWorkerStage {
  gaggle: string;
  workflow: string;
  stage: string;
  kind: GraphNodeKind;
  stationId: string;
  inScope: boolean;
}

export interface FactoryWorker {
  id: string;
  gaggle: string;
  gaggleDisplayName: string;
  name: string;
  displayName: string;
  harness: Harness;
  status: DefinitionStatus;
  stages: FactoryWorkerStage[];
  activeRunCount: number;
  activeStationIds: string[];
  placements: FactoryWorkerPlacement[];
  idle: boolean;
}

export interface FactoryGaggleEntity {
  name: string;
  displayName: string;
  status: DefinitionStatus;
  workflowCount: number;
  gooberCount: number;
  activeRuns: number;
  unreadRuns: number;
  heldStages: number;
  blockedStages: number;
}

export interface FactoryWorkflowEntity {
  gaggle: string;
  name: string;
  displayName: string;
  laneId: string;
}

export type FactoryAttentionKind = "blocked-run" | "recent-failure";

export interface FactoryAttentionItem {
  id: string;
  kind: FactoryAttentionKind;
  runId: string;
  gaggle: string;
  workflow: string;
  workflowDisplayName: string;
  stageId?: string;
  stationId?: string;
  phase: RunPhase;
  reason?: FactoryHoldReason;
  at: string;
}

export interface FactoryCapacity {
  wip: number;
  /** Sum of the known workflow limits on the floor. Undefined when none is known. */
  limit?: number;
  /** How many shown workflows report no usable limit; their capacity is unknown. */
  unknownLimits: number;
  saturation?: number;
}

export interface FactoryCounts {
  gaggles: number;
  workflows: number;
  goobers: number;
  idleGoobers: number;
  activeRuns: number;
  blockedRuns: number;
  unreadRuns: number;
  heldStages: number;
  blockedStages: number;
  queuedRuns: number;
}

export type FactoryEmptyReason =
  | "no-gaggles"
  | "no-workflows"
  | "no-active-runs"
  | undefined;

export interface FactoryCommons {
  x: number;
  y: number;
  width: number;
  height: number;
  workerIds: string[];
  renderedWorkerIds: string[];
  overflowWorkerCount: number;
}

export interface FactoryFloorModel {
  scope: FactoryScope;
  gaggles: FactoryGaggleEntity[];
  workflows: FactoryWorkflowEntity[];
  lanes: FactoryLane[];
  stations: FactoryStation[];
  carriers: FactoryCarrier[];
  workers: FactoryWorker[];
  commons: FactoryCommons;
  attention: FactoryAttentionItem[];
  counts: FactoryCounts;
  capacity: FactoryCapacity;
  emptyReason: FactoryEmptyReason;
  /** True when the daemon reports more active runs beyond the 50-run floor bound. */
  runsTruncated: boolean;
  width: number;
  height: number;
}

export interface FactoryModelInput {
  inventories: readonly GaggleInventory[];
  workflowDetails?: ReadonlyMap<string, WorkflowDetail>;
  activeRuns: readonly RunSummary[];
  runSignals?: ReadonlyMap<string, FactoryRunSignal>;
  recentOutcomes?: readonly RunSummary[];
  scope?: FactoryScope;
  previous?: FactoryFloorModel;
  runsTruncated?: boolean;
}

const ATTENTION_LIMIT = 8;
const RECENT_FAILURE_LIMIT = 4;

export function laneKey(gaggle: string, workflow: string): string {
  return `${gaggle}/${workflow}`;
}

/**
 * Drops scope values the daemon does not report, so a stale bookmark or a
 * hand-edited hash degrades to the whole floor instead of an empty plant that
 * looks like an outage.
 */
export function validateFactoryScope(
  scope: FactoryScope,
  inventories: readonly GaggleInventory[],
): { scope: FactoryScope; dropped: FactoryScope } {
  const dropped: FactoryScope = {};
  const inventory = scope.gaggle
    ? inventories.find((candidate) => candidate.gaggle.name === scope.gaggle)
    : undefined;
  const gaggle = scope.gaggle && inventory ? scope.gaggle : undefined;
  if (scope.gaggle && !inventory) {
    dropped.gaggle = scope.gaggle;
  }

  let workflow: string | undefined;
  if (scope.workflow) {
    const pool = gaggle
      ? (inventory?.workflows ?? [])
      : inventories.flatMap((candidate) => candidate.workflows);
    workflow = pool.some((candidate) => candidate.identity.name === scope.workflow)
      ? scope.workflow
      : undefined;
    if (!workflow) {
      dropped.workflow = scope.workflow;
    }
  }

  return { scope: { gaggle, workflow }, dropped };
}

export function stationKey(gaggle: string, workflow: string, stage: string): string {
  return `${gaggle}/${workflow}/${stage}`;
}

export function workerKey(gaggle: string, goober: string): string {
  return `${gaggle}/${goober}`;
}

/**
 * Derives what a live run is actually doing at its current stage.
 *
 * Only two things make a run "not progressing", and both are journal facts:
 * a human gate that paused (`gate.paused`, with no later evaluation) and a
 * stage attempt the runner reported `blocked`. A *finished failure* is neither:
 * the run is still executing, and painting its stage as blocked would invent an
 * outage. When no stage-level read is available the run keeps its daemon-
 * reported phase and is marked unknown rather than guessed at.
 */
export function deriveRunSignal(input: {
  run: RunSummary;
  attempts?: readonly StageAttempt[];
  events?: readonly RunEvent[];
}): FactoryRunSignal {
  const { run } = input;
  const stage = run.currentStage;
  if (!stage) {
    return { state: "starting", reason: "awaiting-stage", confirmed: true };
  }

  const events = input.events;
  if (events && events.length > 0) {
    const gateState = latestGateState(events, stage);
    if (gateState === "paused") {
      return { state: "paused", reason: "human-gate", confirmed: true };
    }
    const stageStatus = latestStageStatus(events, stage);
    if (stageStatus === "blocked") {
      return { state: "blocked", reason: "stage-blocked", confirmed: true };
    }
    if (stageStatus === "failure") {
      return { state: "running", reason: "attempt-retry", confirmed: true };
    }
    if (gateState !== undefined || stageStatus !== undefined) {
      return { state: "running", confirmed: true };
    }
  }

  const attempts = input.attempts;
  if (attempts && attempts.length > 0) {
    const latest = latestAttempt(attempts);
    if (latest?.status === "blocked") {
      return { state: "blocked", reason: "stage-blocked", confirmed: true };
    }
    if (latest?.status === "failure") {
      return { state: "running", reason: "attempt-retry", confirmed: true };
    }
    return { state: "running", confirmed: true };
  }

  return { state: "unknown", confirmed: false };
}

function latestGateState(
  events: readonly RunEvent[],
  stage: string,
): "paused" | "started" | "evaluated" | undefined {
  let state: "paused" | "started" | "evaluated" | undefined;
  for (const event of [...events].sort(bySequence)) {
    if (event.gate !== stage) {
      continue;
    }
    if (event.type === "gate.paused") {
      state = "paused";
    } else if (event.type === "gate.started") {
      state = "started";
    } else if (event.type === "gate.evaluated") {
      state = "evaluated";
    }
  }
  return state;
}

function latestStageStatus(
  events: readonly RunEvent[],
  stage: string,
): "running" | "blocked" | "failure" | "success" | undefined {
  let status: "running" | "blocked" | "failure" | "success" | undefined;
  for (const event of [...events].sort(bySequence)) {
    if (event.stage !== stage) {
      continue;
    }
    if (event.type === "stage.started") {
      status = "running";
      continue;
    }
    if (event.type !== "stage.finished") {
      continue;
    }
    if (event.status === "blocked") {
      status = "blocked";
    } else if (event.status === "failure") {
      status = "failure";
    } else if (event.status === "success") {
      status = "success";
    }
  }
  return status;
}

function latestAttempt(attempts: readonly StageAttempt[]): StageAttempt | undefined {
  return [...attempts].sort(
    (left, right) =>
      left.visit - right.visit ||
      left.number - right.number ||
      (left.startedSeq ?? 0) - (right.startedSeq ?? 0),
  )[attempts.length - 1];
}

function bySequence(left: RunEvent, right: RunEvent): number {
  return left.seq - right.seq || left.branch - right.branch;
}

export function buildFactoryFloorModel(input: FactoryModelInput): FactoryFloorModel {
  const scope: FactoryScope = {
    gaggle: input.scope?.gaggle,
    workflow: input.scope?.workflow,
  };
  const details = input.workflowDetails ?? new Map<string, WorkflowDetail>();
  const signals = input.runSignals ?? new Map<string, FactoryRunSignal>();
  const inventories = [...input.inventories]
    .filter((inventory) => !scope.gaggle || inventory.gaggle.name === scope.gaggle)
    .sort((left, right) => left.gaggle.name.localeCompare(right.gaggle.name));

  const activeRuns = [...input.activeRuns]
    .filter(
      (run) =>
        run.phase === "running" &&
        (!scope.gaggle || run.gaggle === scope.gaggle) &&
        (!scope.workflow || run.workflow === scope.workflow),
    )
    .sort(
      (left, right) =>
        Date.parse(left.startedAt) - Date.parse(right.startedAt) ||
        left.id.localeCompare(right.id),
    );

  const laneSeeds = collectLaneSeeds(inventories, activeRuns, details, scope);
  const runsByLane = groupBy(activeRuns, (run) => laneKey(run.gaggle, run.workflow));

  const lanes: FactoryLane[] = [];
  const stations: FactoryStation[] = [];
  const carriers: FactoryCarrier[] = [];
  let cursorY = FLOOR_PADDING_TOP;
  let floorWidth = FLOOR_MIN_WIDTH;

  for (const seed of laneSeeds) {
    const laneRuns = runsByLane.get(seed.id) ?? [];
    const built = buildLane(seed, laneRuns, signals, cursorY);
    lanes.push(built.lane);
    stations.push(...built.lane.stations);
    carriers.push(...built.carriers);
    floorWidth = Math.max(floorWidth, built.lane.x + built.lane.width);
    cursorY += built.lane.height + LANE_GAP;
  }

  const laneBottom = lanes.length > 0 ? cursorY - LANE_GAP : FLOOR_PADDING_TOP;
  const canvasWidth = Math.max(FLOOR_MIN_WIDTH, floorWidth);
  const stationsById = new Map(stations.map((station) => [station.id, station]));
  const workers = buildWorkers(inventories, stationsById, scope, {
    y: laneBottom + COMMONS_GAP,
    width: canvasWidth - FLOOR_PADDING_X * 2,
  });

  for (const carrier of carriers) {
    const station = stationsById.get(carrier.stationId);
    const owner = station?.owner;
    if (owner) {
      carrier.ownerWorkerId = workerKey(owner.gaggle, owner.name);
    }
  }
  for (const station of stations) {
    station.workerIds = workers
      .filter((worker) => worker.activeStationIds.includes(station.id))
      .map((worker) => worker.id);
    station.renderedWorkerIds = workers
      .filter((worker) =>
        worker.placements.some(
          (placement) => placement.stationId === station.id && placement.rendered,
        ),
      )
      .map((worker) => worker.id);
    station.workerOverflowCount =
      station.workerIds.length - station.renderedWorkerIds.length;
  }

  stabilizeCarrierSlots(carriers, stations, lanes, input.previous);
  applyTransitions(carriers, input.previous);
  markActiveConveyors(lanes, carriers);

  const commonsWorkers = workers.filter((worker) =>
    worker.placements.some((placement) => placement.stationId === undefined),
  );
  // The commons only exists when someone is standing in it: an empty ready area
  // at the foot of every floor is dead space pretending to be content.
  const commons: FactoryCommons = {
    x: FLOOR_PADDING_X,
    y: laneBottom + COMMONS_GAP,
    width: canvasWidth - FLOOR_PADDING_X * 2,
    height: commonsWorkers.length > 0 ? COMMONS_HEIGHT : 0,
    workerIds: commonsWorkers.map((worker) => worker.id),
    renderedWorkerIds: commonsWorkers
      .filter((worker) => worker.placements.some((placement) => placement.rendered))
      .map((worker) => worker.id),
    overflowWorkerCount: commonsWorkers.filter(
      (worker) => !worker.placements.some((placement) => placement.rendered),
    ).length,
  };

  const heldStations = stations.filter((station) => station.alarm === "hold");
  const blockedStations = stations.filter((station) => station.alarm === "blocked");
  const blockedRuns = carriers.filter(
    (carrier) => carrier.state === "blocked" || carrier.state === "paused",
  );
  const unreadRuns = carriers.filter((carrier) => !carrier.confirmed);
  const queuedRuns = carriers.filter((carrier) => carrier.stageId === undefined);

  const counts: FactoryCounts = {
    gaggles: inventories.length,
    workflows: lanes.length,
    goobers: workers.length,
    idleGoobers: workers.filter((worker) => worker.idle).length,
    activeRuns: carriers.length,
    blockedRuns: blockedRuns.length,
    unreadRuns: unreadRuns.length,
    heldStages: heldStations.length,
    blockedStages: blockedStations.length,
    queuedRuns: queuedRuns.length,
  };

  const capacity = buildCapacity(lanes, carriers.length);
  const gaggles = buildGaggleEntities(inventories, lanes, stations, carriers);
  const workflows = lanes.map((lane) => ({
    gaggle: lane.gaggle,
    name: lane.workflow,
    displayName: lane.displayName,
    laneId: lane.id,
  }));

  return {
    scope,
    gaggles,
    workflows,
    lanes,
    stations,
    carriers,
    workers,
    commons,
    attention: buildAttention(carriers, input.recentOutcomes ?? [], lanes, scope),
    counts,
    capacity,
    emptyReason: emptyReason(inventories, lanes, carriers),
    runsTruncated: input.runsTruncated ?? false,
    width: canvasWidth,
    height:
      commons.height > 0
        ? commons.y + commons.height + FLOOR_PADDING_BOTTOM
        : laneBottom + FLOOR_PADDING_BOTTOM,
  };
}

interface LaneSeed {
  id: string;
  gaggle: string;
  gaggleDisplayName: string;
  workflow: string;
  displayName: string;
  limit?: number;
  detail?: WorkflowDetail;
  observedStages: string[];
  stageCount: number;
  gooberOwners: Map<string, { gaggle: string; name: string; displayName: string }>;
}

function collectLaneSeeds(
  inventories: readonly GaggleInventory[],
  activeRuns: readonly RunSummary[],
  details: ReadonlyMap<string, WorkflowDetail>,
  scope: FactoryScope,
): LaneSeed[] {
  const seeds = new Map<string, LaneSeed>();
  const gaggleNames = new Map<string, string>();
  for (const inventory of inventories) {
    gaggleNames.set(inventory.gaggle.name, inventory.gaggle.displayName);
  }

  for (const inventory of inventories) {
    const owners = gooberStageOwners(inventory.goobers, inventory.gaggle.name);
    for (const workflow of inventory.workflows) {
      if (scope.workflow && workflow.identity.name !== scope.workflow) {
        continue;
      }
      const id = laneKey(workflow.identity.gaggle, workflow.identity.name);
      seeds.set(id, {
        id,
        gaggle: workflow.identity.gaggle,
        gaggleDisplayName: inventory.gaggle.displayName,
        workflow: workflow.identity.name,
        displayName: workflow.displayName,
        limit: positiveLimit(workflow.concurrency.maxConcurrentRuns),
        detail: details.get(id),
        observedStages: [],
        stageCount: workflow.stageCount,
        gooberOwners: owners,
      });
    }
  }

  // A run can only be active against a workflow the daemon knows about, but the
  // inventory read and the run read are separate calls: keep the run visible
  // rather than dropping work off the floor because the pages disagreed.
  for (const run of activeRuns) {
    const id = laneKey(run.gaggle, run.workflow);
    const seed = seeds.get(id);
    if (seed) {
      if (run.currentStage && !seed.observedStages.includes(run.currentStage)) {
        seed.observedStages.push(run.currentStage);
      }
      continue;
    }
    const detail = details.get(id);
    seeds.set(id, {
      id,
      gaggle: run.gaggle,
      gaggleDisplayName: gaggleNames.get(run.gaggle) ?? run.gaggle,
      workflow: run.workflow,
      displayName: detail?.displayName ?? run.workflow,
      limit: positiveLimit(detail?.concurrency.maxConcurrentRuns),
      detail,
      observedStages: run.currentStage ? [run.currentStage] : [],
      stageCount: detail?.stageCount ?? 0,
      gooberOwners: new Map(),
    });
  }

  return [...seeds.values()].sort(
    (left, right) =>
      left.gaggle.localeCompare(right.gaggle) || left.workflow.localeCompare(right.workflow),
  );
}

function gooberStageOwners(
  goobers: readonly Goober[],
  gaggle: string,
): Map<string, { gaggle: string; name: string; displayName: string }> {
  const owners = new Map<string, { gaggle: string; name: string; displayName: string }>();
  for (const goober of goobers) {
    for (const ownership of goober.stages) {
      owners.set(
        stationKey(ownership.workflow.gaggle, ownership.workflow.name, ownership.stage),
        { gaggle, name: goober.name, displayName: goober.displayName },
      );
    }
  }
  return owners;
}

interface StagePlacement {
  stageId: string;
  kind: GraphNodeKind;
  evaluator?: EvaluatorKind;
  column: number;
  row: number;
  isStart: boolean;
}

function laneStagePlacements(seed: LaneSeed): {
  placements: StagePlacement[];
  source: FactoryTopologySource;
} {
  const detail = seed.detail;
  if (detail && detail.graph.nodes.length > 0) {
    const layout = layoutWorkflowGraph(detail.graph);
    const positioned = layout.nodes.flatMap((node) =>
      node.type === "stage" ? [node] : [],
    );
    const columns = new Map<number, number>();
    const placements = positioned
      .slice()
      .sort(
        (left, right) =>
          left.depth - right.depth || left.y - right.y || left.id.localeCompare(right.id),
      )
      .map((node) => {
        const row = columns.get(node.depth) ?? 0;
        columns.set(node.depth, row + 1);
        const stage = detail.stages.find((candidate) => candidate.name === node.node.id);
        const declaredEvaluator = stage?.evaluator ? stage.evaluator : undefined;
        return {
          stageId: node.node.id,
          kind: node.node.kind,
          evaluator: declaredEvaluator ?? node.node.evaluator,
          column: node.depth,
          row,
          isStart: node.node.id === detail.graph.start,
        };
      });
    return { placements, source: "declared" };
  }

  // No pinned graph read: place only the stages live runs are demonstrably
  // standing on, and label the lane as observed rather than declared.
  const placements = [...seed.observedStages]
    .sort((left, right) => left.localeCompare(right))
    .map((stageId, index) => ({
      stageId,
      kind: "deterministic" as GraphNodeKind,
      column: 0,
      row: index,
      isStart: false,
    }));
  return { placements, source: "observed" };
}

function buildLane(
  seed: LaneSeed,
  laneRuns: readonly RunSummary[],
  signals: ReadonlyMap<string, FactoryRunSignal>,
  top: number,
): { lane: FactoryLane; carriers: FactoryCarrier[] } {
  const { placements, source } = laneStagePlacements(seed);
  const columnCount = placements.reduce(
    (widest, placement) => Math.max(widest, placement.column + 1),
    0,
  );
  const rowCount = placements.reduce(
    (tallest, placement) => Math.max(tallest, placement.row + 1),
    0,
  );
  const contentX = FLOOR_PADDING_X + YARD_WIDTH + YARD_GAP;
  const contentTop = top + LANE_HEADER_HEIGHT + LANE_PADDING_TOP;
  const rowHeight = STATION_HEIGHT + STATION_APRON_HEIGHT;
  const contentHeight =
    rowCount > 0 ? rowCount * rowHeight + (rowCount - 1) * ROW_GAP : rowHeight;

  const repassEdges =
    seed.detail?.graph.edges.filter((edge) => !edge.terminal && edge.target) ?? [];
  const columnByStage = new Map(
    placements.map((placement) => [placement.stageId, placement.column]),
  );
  const repassCount = repassEdges.filter(
    (edge) =>
      (columnByStage.get(edge.target) ?? 0) <= (columnByStage.get(edge.source) ?? 0),
  ).length;
  const repassHeight = repassCount * REPASS_LANE_HEIGHT;
  const laneHeight =
    LANE_HEADER_HEIGHT + LANE_PADDING_TOP + contentHeight + repassHeight + LANE_PADDING_BOTTOM;

  const terminals = uniqueTerminals(seed);
  const dockX = contentX + Math.max(columnCount, 1) * (STATION_WIDTH + COLUMN_GAP);
  const docks: FactoryDock[] = terminals.map((terminal, index) => ({
    id: `${seed.id}#dock:${terminal}`,
    laneId: seed.id,
    terminal,
    x: dockX,
    y: contentTop + index * (DOCK_HEIGHT + DOCK_GAP),
    width: DOCK_WIDTH,
    height: DOCK_HEIGHT,
  }));

  const laneWidth =
    (docks.length > 0
      ? dockX + DOCK_WIDTH
      : contentX + Math.max(columnCount, 1) * STATION_WIDTH +
        Math.max(0, columnCount - 1) * COLUMN_GAP) + FLOOR_PADDING_X;

  const runsByStage = groupBy(laneRuns, (run) => run.currentStage ?? "");
  const stations: FactoryStation[] = placements.map((placement) => {
    const stageRuns = runsByStage.get(placement.stageId) ?? [];
    const owner = seed.gooberOwners.get(
      stationKey(seed.gaggle, seed.workflow, placement.stageId),
    ) ?? declaredOwner(seed, placement.stageId);
    const blockedCount = stageRuns.filter((run) => {
      const signal = signals.get(run.id);
      return signal?.state === "blocked" || signal?.state === "paused";
    }).length;
    const hardBlockedCount = stageRuns.filter(
      (run) => signals.get(run.id)?.state === "blocked",
    ).length;
    const pausedCount = stageRuns.filter((run) => {
      const signal = signals.get(run.id);
      return signal?.state === "paused" && signal.reason === "human-gate";
    }).length;
    const unknownCount = stageRuns.filter((run) => !signals.get(run.id)?.confirmed).length;
    const wip = stageRuns.length;
    const allHeld = wip > 0 && unknownCount === 0 && blockedCount === wip;
    const alarm: FactoryAlarmKind | undefined = allHeld
      ? hardBlockedCount > 0
        ? "blocked"
        : pausedCount === wip
          ? "hold"
          : undefined
      : undefined;
    const status: FactoryStationStatus =
      wip === 0
        ? "idle"
        : alarm === "blocked"
          ? "blocked"
          : alarm === "hold"
            ? "held"
          : unknownCount > 0
            ? "unknown"
          : blockedCount > 0
            ? "impeded"
            : "running";
    return {
      id: stationKey(seed.gaggle, seed.workflow, placement.stageId),
      laneId: seed.id,
      stageId: placement.stageId,
      gaggle: seed.gaggle,
      workflow: seed.workflow,
      workflowDisplayName: seed.displayName,
      kind: placement.kind,
      evaluator: placement.evaluator,
      owner,
      source,
      isStart: placement.isStart,
      column: placement.column,
      row: placement.row,
      x: contentX + placement.column * (STATION_WIDTH + COLUMN_GAP),
      y: contentTop + placement.row * (rowHeight + ROW_GAP),
      width: STATION_WIDTH,
      height: STATION_HEIGHT,
      wip,
      limit: seed.limit,
      saturation: seed.limit ? wip / seed.limit : undefined,
      blockedCount,
      hardBlockedCount,
      pausedCount,
      unknownCount,
      status,
      alarm,
      runIds: stageRuns.map((run) => run.id),
      renderedRunIds: [],
      overflowRunCount: 0,
      workerIds: [],
      renderedWorkerIds: [],
      workerOverflowCount: 0,
    };
  });

  const stationByStage = new Map(
    stations.map((station) => [station.stageId, station] as const),
  );
  const yardRuns = laneRuns.filter(
    (run) => !run.currentStage || !stationByStage.has(run.currentStage),
  );
  const yard: FactoryYard = {
    id: `${seed.id}#yard`,
    laneId: seed.id,
    x: FLOOR_PADDING_X,
    y: contentTop,
    width: YARD_WIDTH,
    height: Math.max(STATION_HEIGHT, contentHeight - STATION_APRON_HEIGHT),
    runIds: yardRuns.map((run) => run.id),
    renderedRunIds: [],
    overflowRunCount: 0,
  };

  const carriers: FactoryCarrier[] = [];
  for (const station of stations) {
    station.runIds.forEach((runId, index) => {
      const run = laneRuns.find((candidate) => candidate.id === runId);
      if (!run) {
        return;
      }
      carriers.push(
        buildCarrier(run, seed, station.id, signals.get(run.id), index, {
          x:
            station.x +
            8 +
            (index % CARRIERS_PER_ROW) * (CARRIER_WIDTH + CARRIER_GAP),
          y:
            station.y +
            STATION_HEIGHT +
            10 +
            Math.floor(index / CARRIERS_PER_ROW) * (CARRIER_HEIGHT + CARRIER_GAP),
        }),
      );
    });
  }
  yardRuns.forEach((run, index) => {
    carriers.push(
      buildCarrier(run, seed, yard.id, signals.get(run.id), index, {
        x: yard.x + 12 + (index % YARD_CARRIERS_PER_ROW) * (CARRIER_WIDTH + CARRIER_GAP),
        y:
          yard.y +
          14 +
          Math.floor(index / YARD_CARRIERS_PER_ROW) * (CARRIER_HEIGHT + CARRIER_GAP),
      }),
    );
  });

  const blockedRuns = carriers.filter(
    (carrier) => carrier.state === "blocked" || carrier.state === "paused",
  ).length;
  const unreadRuns = carriers.filter((carrier) => !carrier.confirmed).length;

  const lane: FactoryLane = {
    id: seed.id,
    gaggle: seed.gaggle,
    gaggleDisplayName: seed.gaggleDisplayName,
    workflow: seed.workflow,
    displayName: seed.displayName,
    source,
    stageCount: seed.stageCount || placements.length,
    stations,
    docks,
    conveyors: buildConveyors(seed, stationByStage, docks, contentTop + contentHeight),
    yard,
    activeRuns: laneRuns.length,
    blockedRuns,
    unreadRuns,
    limit: seed.limit,
    saturation: seed.limit ? laneRuns.length / seed.limit : undefined,
    x: 0,
    y: top,
    width: laneWidth,
    height: laneHeight,
  };
  return { lane, carriers };
}

function declaredOwner(
  seed: LaneSeed,
  stageId: string,
): { gaggle: string; name: string; displayName?: string } | undefined {
  const stage = seed.detail?.stages.find((candidate) => candidate.name === stageId);
  if (stage?.owner) {
    return { gaggle: stage.owner.gaggle, name: stage.owner.name };
  }
  const node = seed.detail?.graph.nodes.find((candidate) => candidate.id === stageId);
  if (!node?.owner) {
    return undefined;
  }
  const [gaggle, name] = node.owner.split("/");
  return name ? { gaggle, name } : { gaggle: seed.gaggle, name: node.owner };
}

function buildCarrier(
  run: RunSummary,
  seed: LaneSeed,
  stationId: string,
  signal: FactoryRunSignal | undefined,
  queueIndex: number,
  position: { x: number; y: number },
): FactoryCarrier {
  const resolved = signal ?? { state: "unknown" as FactoryRunState, confirmed: false };
  return {
    runId: run.id,
    gaggle: run.gaggle,
    workflow: run.workflow,
    workflowDisplayName: seed.displayName,
    laneId: seed.id,
    stageId: run.currentStage,
    stationId,
    phase: run.phase,
    state: resolved.state,
    reason: resolved.reason,
    confirmed: resolved.confirmed,
    triggerKind: run.trigger.kind,
    startedAt: run.startedAt,
    lastActivityAt: run.lastActivityAt,
    durationMillis: run.durationMillis,
    retryCount: run.retryCount,
    policyRetryCount: run.policyRetryCount,
    infraRetryCount: run.infraRetryCount,
    repassCount: run.repassCount,
    queueIndex,
    rendered: false,
    x: position.x,
    y: position.y,
  };
}

function uniqueTerminals(seed: LaneSeed): GraphTerminal[] {
  const order: GraphTerminal[] = ["complete", "escalate", "abort"];
  const present = new Set<GraphTerminal>();
  for (const edge of seed.detail?.graph.edges ?? []) {
    if (edge.terminal) {
      present.add(edge.terminal);
    }
  }
  return order.filter((terminal) => present.has(terminal));
}

function buildConveyors(
  seed: LaneSeed,
  stationByStage: ReadonlyMap<string, FactoryStation>,
  docks: readonly FactoryDock[],
  repassBaseline: number,
): FactoryConveyor[] {
  const edges = seed.detail?.graph.edges ?? [];
  const dockByTerminal = new Map(docks.map((dock) => [dock.terminal, dock]));
  const conveyors: FactoryConveyor[] = [];
  let repassLane = 0;

  edges.forEach((edge, index) => {
    const source = stationByStage.get(edge.source);
    if (!source) {
      return;
    }
    if (edge.terminal) {
      const dock = dockByTerminal.get(edge.terminal);
      if (!dock) {
        return;
      }
      const y1 = source.y + source.height / 2;
      const y2 = dock.y + dock.height / 2;
      const midX = (source.x + source.width + dock.x) / 2;
      conveyors.push({
        id: `${seed.id}#edge:${index}`,
        laneId: seed.id,
        kind: "terminal",
        fromStationId: source.id,
        toId: dock.id,
        outcome: edge.outcome,
        terminal: edge.terminal,
        path: elbowPath(source.x + source.width, y1, dock.x, y2, midX),
        labelX: midX,
        labelY: Math.min(y1, y2) - 8,
        active: false,
      });
      return;
    }

    const target = stationByStage.get(edge.target);
    if (!target) {
      return;
    }
    if (target.column > source.column) {
      const y1 = source.y + source.height / 2;
      const y2 = target.y + target.height / 2;
      const midX = (source.x + source.width + target.x) / 2;
      conveyors.push({
        id: `${seed.id}#edge:${index}`,
        laneId: seed.id,
        kind: "forward",
        fromStationId: source.id,
        toId: target.id,
        outcome: edge.outcome,
        path: elbowPath(source.x + source.width, y1, target.x, y2, midX),
        labelX: midX,
        labelY: Math.min(y1, y2) - 8,
        active: false,
      });
      return;
    }

    const laneY = repassBaseline + repassLane * REPASS_LANE_HEIGHT + REPASS_LANE_HEIGHT / 2;
    repassLane += 1;
    const x1 = source.x + source.width / 2;
    const x2 = target.x + target.width / 2;
    conveyors.push({
      id: `${seed.id}#edge:${index}`,
      laneId: seed.id,
      kind: "repass",
      fromStationId: source.id,
      toId: target.id,
      outcome: edge.outcome,
      path: `M ${round(x1)} ${round(source.y + source.height + STATION_APRON_HEIGHT - 8)} L ${round(x1)} ${round(laneY)} L ${round(x2)} ${round(laneY)} L ${round(x2)} ${round(target.y + target.height + STATION_APRON_HEIGHT - 8)}`,
      labelX: (x1 + x2) / 2,
      labelY: laneY - 5,
      active: false,
    });
  });

  return conveyors;
}

function elbowPath(
  x1: number,
  y1: number,
  x2: number,
  y2: number,
  midX: number,
): string {
  if (Math.abs(y1 - y2) < 1) {
    return `M ${round(x1)} ${round(y1)} L ${round(x2)} ${round(y2)}`;
  }
  return `M ${round(x1)} ${round(y1)} L ${round(midX)} ${round(y1)} L ${round(midX)} ${round(y2)} L ${round(x2)} ${round(y2)}`;
}

function round(value: number): number {
  return Math.round(value * 100) / 100;
}

function buildWorkers(
  inventories: readonly GaggleInventory[],
  stationsById: ReadonlyMap<string, FactoryStation>,
  scope: FactoryScope,
  commons: { y: number; width: number },
): FactoryWorker[] {
  const workers: FactoryWorker[] = [];
  const stationOccupancy = new Map<string, number>();
  let commonsIndex = 0;
  const commonsPerRow = Math.max(
    1,
    Math.floor((commons.width - 24) / (WORKER_WIDTH + WORKER_GAP)),
  );

  const roster = inventories
    .flatMap((inventory) =>
      inventory.goobers.map((goober) => ({ goober, gaggle: inventory.gaggle })),
    )
    .sort(
      (left, right) =>
        left.gaggle.name.localeCompare(right.gaggle.name) ||
        left.goober.name.localeCompare(right.goober.name),
    );

  for (const entry of roster) {
    const id = workerKey(entry.gaggle.name, entry.goober.name);
    const stages: FactoryWorkerStage[] = entry.goober.stages
      .map((ownership) => {
        const stationId = stationKey(
          ownership.workflow.gaggle,
          ownership.workflow.name,
          ownership.stage,
        );
        return {
          gaggle: ownership.workflow.gaggle,
          workflow: ownership.workflow.name,
          stage: ownership.stage,
          kind: ownership.kind,
          stationId,
          inScope:
            stationsById.has(stationId) &&
            (!scope.workflow || ownership.workflow.name === scope.workflow),
        };
      })
      .sort(
        (left, right) =>
          left.workflow.localeCompare(right.workflow) ||
          left.stage.localeCompare(right.stage),
      );

    // Ownership alone never places a goober on the floor: it stands at a stage
    // only when that stage is actually holding work right now.
    const activeStations = stages
      .flatMap((stage) => {
        const station = stationsById.get(stage.stationId);
        return station && stage.inScope && station.wip > 0 ? [station] : [];
      })
      .sort((left, right) => left.id.localeCompare(right.id));

    const placements: FactoryWorkerPlacement[] = activeStations.map((station) => {
      const occupancy = stationOccupancy.get(station.id) ?? 0;
      stationOccupancy.set(station.id, occupancy + 1);
      const rendered = occupancy < FACTORY_RENDERED_WORKERS_PER_STATION;
      // On the apron beside the machine, not on top of its readouts.
      return {
        id: `${id}@${station.id}`,
        workerId: id,
        stationId: station.id,
        x: station.x + station.width - WORKER_WIDTH - 4,
        y: station.y + station.height + 8,
        active: true,
        rendered,
      };
    });

    if (placements.length === 0) {
      const rendered = commonsIndex < FACTORY_RENDERED_COMMONS_WORKERS;
      const renderIndex = Math.min(
        commonsIndex,
        FACTORY_RENDERED_COMMONS_WORKERS - 1,
      );
      const column = renderIndex % commonsPerRow;
      const row = Math.floor(renderIndex / commonsPerRow);
      commonsIndex += 1;
      placements.push({
        id: `${id}@commons`,
        workerId: id,
        x: FLOOR_PADDING_X + 16 + column * (WORKER_WIDTH + WORKER_GAP),
        y: commons.y + 34 + row * (WORKER_HEIGHT + 6),
        active: false,
        rendered,
      });
    }

    workers.push({
      id,
      gaggle: entry.gaggle.name,
      gaggleDisplayName: entry.gaggle.displayName,
      name: entry.goober.name,
      displayName: entry.goober.displayName,
      harness: entry.goober.harness,
      status: entry.goober.status,
      stages,
      activeRunCount: activeStations.reduce((total, station) => total + station.wip, 0),
      activeStationIds: activeStations.map((station) => station.id),
      placements,
      idle: activeStations.length === 0,
    });
  }

  return workers;
}

/**
 * Keeps a run in the same physical slot while it remains at the same stage.
 * A sibling finishing must not make surviving carriers slide across an apron.
 */
function stabilizeCarrierSlots(
  carriers: FactoryCarrier[],
  stations: readonly FactoryStation[],
  lanes: readonly FactoryLane[],
  previous: FactoryFloorModel | undefined,
): void {
  const previousByRun = new Map(
    previous?.carriers.map((carrier) => [carrier.runId, carrier]) ?? [],
  );
  const stationsById = new Map(stations.map((station) => [station.id, station]));
  const yardsById = new Map(lanes.map((lane) => [lane.yard.id, lane.yard]));
  const groups = groupBy(carriers, (carrier) => carrier.stationId);

  for (const [stationId, stationCarriers] of groups) {
    const usedQueueSlots = new Set<number>();
    const unassignedQueueSlots: FactoryCarrier[] = [];
    for (const carrier of stationCarriers) {
      const prior = previousByRun.get(carrier.runId);
      if (
        prior?.stationId === stationId &&
        Number.isSafeInteger(prior.queueIndex) &&
        prior.queueIndex >= 0 &&
        !usedQueueSlots.has(prior.queueIndex)
      ) {
        carrier.queueIndex = prior.queueIndex;
        usedQueueSlots.add(prior.queueIndex);
      } else {
        unassignedQueueSlots.push(carrier);
      }
    }

    unassignedQueueSlots
      .sort((left, right) => left.runId.localeCompare(right.runId))
      .forEach((carrier) => {
        let slot = 0;
        while (usedQueueSlots.has(slot)) {
          slot += 1;
        }
        carrier.queueIndex = slot;
        usedQueueSlots.add(slot);
      });

    const station = stationsById.get(stationId);
    const yard = yardsById.get(stationId);
    const renderLimit = station
      ? FACTORY_RENDERED_CARRIERS_PER_STATION
      : FACTORY_RENDERED_CARRIERS_PER_YARD;
    const usedRenderSlots = new Set<number>();
    const unassignedRenderSlots: FactoryCarrier[] = [];
    for (const carrier of stationCarriers) {
      const prior = previousByRun.get(carrier.runId);
      if (
        prior?.stationId === stationId &&
        prior.rendered &&
        prior.renderSlot !== undefined &&
        prior.renderSlot >= 0 &&
        prior.renderSlot < renderLimit &&
        !usedRenderSlots.has(prior.renderSlot)
      ) {
        carrier.rendered = true;
        carrier.renderSlot = prior.renderSlot;
        usedRenderSlots.add(prior.renderSlot);
      } else {
        unassignedRenderSlots.push(carrier);
      }
    }
    unassignedRenderSlots
      .sort(
        (left, right) =>
          left.queueIndex - right.queueIndex || left.runId.localeCompare(right.runId),
      )
      .forEach((carrier) => {
        let slot = 0;
        while (usedRenderSlots.has(slot)) {
          slot += 1;
        }
        if (slot >= renderLimit) {
          carrier.rendered = false;
          carrier.renderSlot = undefined;
          return;
        }
        carrier.rendered = true;
        carrier.renderSlot = slot;
        usedRenderSlots.add(slot);
      });

    const rendered = stationCarriers
      .filter((carrier) => carrier.rendered)
      .sort(
        (left, right) =>
          (left.renderSlot ?? 0) - (right.renderSlot ?? 0) ||
          left.runId.localeCompare(right.runId),
      );
    if (station) {
      station.renderedRunIds = rendered.map((carrier) => carrier.runId);
      station.overflowRunCount = station.runIds.length - station.renderedRunIds.length;
    } else if (yard) {
      yard.renderedRunIds = rendered.map((carrier) => carrier.runId);
      yard.overflowRunCount = yard.runIds.length - yard.renderedRunIds.length;
    }

    for (const carrier of rendered) {
      const slot = carrier.renderSlot ?? 0;
      if (station) {
        carrier.x =
          station.x + 8 + (slot % CARRIERS_PER_ROW) * (CARRIER_WIDTH + CARRIER_GAP);
        carrier.y =
          station.y +
          STATION_HEIGHT +
          10 +
          Math.floor(slot / CARRIERS_PER_ROW) * (CARRIER_HEIGHT + CARRIER_GAP);
      } else if (yard) {
        carrier.x =
          yard.x + 12 + (slot % YARD_CARRIERS_PER_ROW) * (CARRIER_WIDTH + CARRIER_GAP);
        carrier.y =
          yard.y +
          14 +
          Math.floor(slot / YARD_CARRIERS_PER_ROW) * (CARRIER_HEIGHT + CARRIER_GAP);
      }
    }
  }
}

function applyTransitions(
  carriers: FactoryCarrier[],
  previous: FactoryFloorModel | undefined,
): void {
  if (!previous) {
    return;
  }
  const before = new Map(previous.carriers.map((carrier) => [carrier.runId, carrier]));
  for (const carrier of carriers) {
    const prior = before.get(carrier.runId);
    if (!prior) {
      carrier.transition = { kind: "arrival" };
      continue;
    }
    if (prior.stationId !== carrier.stationId || prior.stageId !== carrier.stageId) {
      carrier.transition = {
        kind: "stage-change",
        fromStageId: prior.stageId,
        fromStationId: prior.stationId,
      };
    }
  }
}

function markActiveConveyors(lanes: FactoryLane[], carriers: readonly FactoryCarrier[]): void {
  const moves = new Set<string>();
  for (const carrier of carriers) {
    if (carrier.transition?.kind === "stage-change" && carrier.transition.fromStationId) {
      moves.add(`${carrier.transition.fromStationId}\u0000${carrier.stationId}`);
    }
  }
  if (moves.size === 0) {
    return;
  }
  for (const lane of lanes) {
    for (const conveyor of lane.conveyors) {
      conveyor.active = moves.has(`${conveyor.fromStationId}\u0000${conveyor.toId}`);
    }
  }
}

function buildCapacity(lanes: readonly FactoryLane[], wip: number): FactoryCapacity {
  let limit = 0;
  let known = 0;
  let unknownLimits = 0;
  for (const lane of lanes) {
    if (lane.limit === undefined) {
      unknownLimits += 1;
      continue;
    }
    limit += lane.limit;
    known += 1;
  }
  if (known === 0) {
    return { wip, unknownLimits, limit: undefined, saturation: undefined };
  }
  return {
    wip,
    limit,
    unknownLimits,
    saturation: unknownLimits === 0 && limit > 0 ? wip / limit : undefined,
  };
}

function buildGaggleEntities(
  inventories: readonly GaggleInventory[],
  lanes: readonly FactoryLane[],
  stations: readonly FactoryStation[],
  carriers: readonly FactoryCarrier[],
): FactoryGaggleEntity[] {
  return inventories.map((inventory) => ({
    name: inventory.gaggle.name,
    displayName: inventory.gaggle.displayName,
    status: inventory.gaggle.status,
    workflowCount: lanes.filter((lane) => lane.gaggle === inventory.gaggle.name).length,
    gooberCount: inventory.goobers.length,
    activeRuns: carriers.filter((carrier) => carrier.gaggle === inventory.gaggle.name).length,
    unreadRuns: carriers.filter(
      (carrier) => carrier.gaggle === inventory.gaggle.name && !carrier.confirmed,
    ).length,
    heldStages: stations.filter(
      (station) => station.gaggle === inventory.gaggle.name && station.alarm === "hold",
    ).length,
    blockedStages: stations.filter(
      (station) =>
        station.gaggle === inventory.gaggle.name && station.alarm === "blocked",
    ).length,
  }));
}

function buildAttention(
  carriers: readonly FactoryCarrier[],
  recentOutcomes: readonly RunSummary[],
  lanes: readonly FactoryLane[],
  scope: FactoryScope,
): FactoryAttentionItem[] {
  const laneNames = new Map(lanes.map((lane) => [lane.id, lane.displayName]));
  const held = carriers
    .filter((carrier) => carrier.state === "blocked" || carrier.state === "paused")
    .sort(
      (left, right) =>
        Date.parse(left.lastActivityAt) - Date.parse(right.lastActivityAt) ||
        left.runId.localeCompare(right.runId),
    )
    .map<FactoryAttentionItem>((carrier) => ({
      id: `blocked:${carrier.runId}`,
      kind: "blocked-run",
      runId: carrier.runId,
      gaggle: carrier.gaggle,
      workflow: carrier.workflow,
      workflowDisplayName: carrier.workflowDisplayName,
      stageId: carrier.stageId,
      stationId: carrier.stationId,
      phase: carrier.phase,
      reason: carrier.reason,
      at: carrier.lastActivityAt,
    }));

  // Terminal outcomes are recent history, never live WIP. They are listed for
  // triage but never placed on the floor as work in progress.
  const failures = recentOutcomes
    .filter(
      (run) =>
        run.terminal &&
        (run.phase === "failed" || run.phase === "escalated") &&
        (!scope.gaggle || run.gaggle === scope.gaggle) &&
        (!scope.workflow || run.workflow === scope.workflow),
    )
    .sort(
      (left, right) =>
        Date.parse(right.finishedAt ?? right.startedAt) -
          Date.parse(left.finishedAt ?? left.startedAt) ||
        left.id.localeCompare(right.id),
    )
    .slice(0, RECENT_FAILURE_LIMIT)
    .map<FactoryAttentionItem>((run) => ({
      id: `recent:${run.id}`,
      kind: "recent-failure",
      runId: run.id,
      gaggle: run.gaggle,
      workflow: run.workflow,
      workflowDisplayName:
        laneNames.get(laneKey(run.gaggle, run.workflow)) ?? run.workflow,
      phase: run.phase,
      at: run.finishedAt ?? run.startedAt,
    }));

  return [...held, ...failures].slice(0, ATTENTION_LIMIT);
}

function emptyReason(
  inventories: readonly GaggleInventory[],
  lanes: readonly FactoryLane[],
  carriers: readonly FactoryCarrier[],
): FactoryEmptyReason {
  if (inventories.length === 0) {
    return "no-gaggles";
  }
  if (lanes.length === 0) {
    return "no-workflows";
  }
  if (carriers.length === 0) {
    return "no-active-runs";
  }
  return undefined;
}

function positiveLimit(value: number | undefined): number | undefined {
  return value !== undefined && Number.isFinite(value) && value > 0 ? value : undefined;
}

function groupBy<T>(items: readonly T[], key: (item: T) => string): Map<string, T[]> {
  const groups = new Map<string, T[]>();
  for (const item of items) {
    const group = groups.get(key(item));
    if (group) {
      group.push(item);
    } else {
      groups.set(key(item), [item]);
    }
  }
  return groups;
}

export function holdReasonLabel(reason: FactoryHoldReason | undefined): string {
  switch (reason) {
    case "human-gate":
      return "Paused at a human gate";
    case "stage-blocked":
      return "Stage reported blocked";
    case "attempt-retry":
      return "Last attempt failed; retrying";
    case "awaiting-stage":
      return "Waiting to enter its first stage";
    default:
      return "";
  }
}

export function runStateLabel(state: FactoryRunState): string {
  switch (state) {
    case "blocked":
      return "Blocked";
    case "paused":
      return "Paused";
    case "starting":
      return "Starting";
    case "running":
      return "Running";
    case "unknown":
      return "Signal unread";
  }
}

export function stationStatusLabel(status: FactoryStationStatus): string {
  switch (status) {
    case "blocked":
      return "Blocked";
    case "impeded":
      return "Partly blocked";
    case "held":
      return "Human hold";
    case "running":
      return "Running";
    case "idle":
      return "Idle";
    case "unknown":
      return "Signal unread";
  }
}

export function stageKindLabel(kind: GraphNodeKind): string {
  switch (kind) {
    case "agentic":
      return "Agentic stage";
    case "deterministic":
      return "Deterministic stage";
    case "gate":
      return "Gate";
    case "parallel":
      return "Parallel";
  }
}

export function capacityLabel(wip: number, limit: number | undefined): string {
  return limit === undefined
    ? `WIP ${wip} / workflow limit unknown`
    : `WIP ${wip} / workflow limit ${limit}`;
}

export function findStation(
  model: FactoryFloorModel,
  id: string,
): FactoryStation | undefined {
  return model.stations.find((station) => station.id === id);
}

export function findCarrier(
  model: FactoryFloorModel,
  runId: string,
): FactoryCarrier | undefined {
  return model.carriers.find((carrier) => carrier.runId === runId);
}

export function findWorker(model: FactoryFloorModel, id: string): FactoryWorker | undefined {
  return model.workers.find((worker) => worker.id === id);
}

export function findLane(model: FactoryFloorModel, id: string): FactoryLane | undefined {
  return model.lanes.find((lane) => lane.id === id);
}
