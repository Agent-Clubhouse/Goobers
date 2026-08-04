import type { EvaluatorKind, GraphNodeKind, GraphTerminal } from "./api/types";
import type {
  FactoryCarrier,
  FactoryConveyor,
  FactoryFloorModel,
  FactoryLane,
  FactoryStation,
  FactoryStationStatus,
  FactoryTopologySource,
  FactoryWorkerPlacement,
} from "./factoryModel";

/**
 * Renderer-neutral Factory Plant layout.
 *
 * Workflow bays live on a stable integer grid. The layout consumes only facts
 * already present in FactoryFloorModel and carries its own allocation state so
 * later model generations can retain every unchanged workflow and station.
 */

export const FACTORY_PLANT_LAYOUT_VERSION = 1;

export const FACTORY_PLANT_BAY_GRID = {
  cellDepth: 16,
  cellWidth: 20,
  gutter: 2.5,
  stationSlotsX: 12,
  stationSlotsZ: 10,
} as const;

export const FACTORY_PLANT_LOD_THRESHOLDS = {
  bayMinProjectedPixels: 64,
  detailMinProjectedPixels: 180,
  maxDetailDomItems: 240,
} as const;

export const FACTORY_PLANT_CAMERA_DIRECTION = {
  x: 18,
  y: 19,
  z: 22,
} as const;

const CELL_PITCH_X =
  FACTORY_PLANT_BAY_GRID.cellWidth + FACTORY_PLANT_BAY_GRID.gutter;
const CELL_PITCH_Z =
  FACTORY_PLANT_BAY_GRID.cellDepth + FACTORY_PLANT_BAY_GRID.gutter;
const STATION_MIN_X = 1.8;
const STATION_MAX_X = FACTORY_PLANT_BAY_GRID.cellWidth - 1.8;
const STATION_MIN_Z = 1.8;
const STATION_MAX_Z = FACTORY_PLANT_BAY_GRID.cellDepth - 1.8;
const STATION_PITCH_X =
  (STATION_MAX_X - STATION_MIN_X) /
  (FACTORY_PLANT_BAY_GRID.stationSlotsX - 1);
const STATION_PITCH_Z =
  (STATION_MAX_Z - STATION_MIN_Z) /
  (FACTORY_PLANT_BAY_GRID.stationSlotsZ - 1);
const STANDARD_BAY_STATION_COLUMNS = [1, 3, 6, 8, 10] as const;
const STANDARD_BAY_STATION_ROWS = [1, 3, 6, 8] as const;
const STANDARD_BAY_STATION_CAPACITY =
  STANDARD_BAY_STATION_COLUMNS.length * STANDARD_BAY_STATION_ROWS.length;
const MACHINE_HALF_FOOTPRINT = 0.98;
const FLOOR_MARGIN = 4;
const HALL_HEIGHT = 4.8;
const STATIC_DRAW_CALLS = 31;
const DEFAULT_CAMERA_PADDING = 1.6;

export interface PlantGridPoint {
  x: number;
  z: number;
}

export interface PlantWorldPoint {
  x: number;
  y: number;
  z: number;
}

export interface PlantScale {
  x: number;
  y: number;
  z: number;
}

export interface PlantTransform {
  position: PlantWorldPoint;
  rotationY: number;
  scale: PlantScale;
}

export interface PlantRect {
  minX: number;
  minZ: number;
  maxX: number;
  maxZ: number;
  width: number;
  depth: number;
}

export interface PlantAabb {
  min: PlantWorldPoint;
  max: PlantWorldPoint;
  size: PlantWorldPoint;
  center: PlantWorldPoint;
}

export interface PlantProjectedBounds {
  minX: number;
  minY: number;
  maxX: number;
  maxY: number;
  width: number;
  height: number;
}

export interface PlantCameraFit {
  aspect: number;
  padding: { x: number; y: number };
  target: PlantWorldPoint;
  position: PlantWorldPoint;
  viewWidth: number;
  viewHeight: number;
  near: number;
  far: number;
  /** Present when the fit was constrained by an obscured canvas. */
  safeArea?: PlantSafeAreaRect;
}

/** The unobscured part of a canvas, in canvas CSS pixels. */
export interface PlantSafeAreaRect {
  left: number;
  top: number;
  width: number;
  height: number;
}

export interface PlantStationSlot {
  column: number;
  row: number;
  modelColumn: number;
  modelRow: number;
}

export interface PlantBayAllocation {
  workflowId: string;
  origin: PlantGridPoint;
  span: { x: number; z: number };
  cells: PlantGridPoint[];
  stationSlots: Record<string, PlantStationSlot>;
}

export interface FactoryPlantAllocation {
  version: typeof FACTORY_PLANT_LAYOUT_VERSION;
  bays: Record<string, PlantBayAllocation>;
}

export type PlantAggregateStatus =
  | "blocked"
  | "held"
  | "unknown"
  | "running"
  | "idle";

export interface PlantStatusCounts {
  blocked: number;
  held: number;
  idle: number;
  impeded: number;
  running: number;
  unknown: number;
}

export interface PlantAggregateSummary {
  status: PlantAggregateStatus;
  workflows: number;
  stages: number;
  wip: number;
  carriers: number;
  workers: number;
  statusCounts: PlantStatusCounts;
  /** Blocked plus impeded stages; no station status is rewritten. */
  attentionStages: number;
}

export interface PlantBayCell {
  id: string;
  workflowId: string;
  grid: PlantGridPoint;
  bounds: PlantRect;
  transform: PlantTransform;
}

export interface PlantMachineLayout {
  id: string;
  workflowId: string;
  stageId: string;
  kind: GraphNodeKind;
  /** Present when the stage is a declared evaluator; drives its silhouette. */
  evaluator?: EvaluatorKind;
  status: FactoryStationStatus;
  source: FactoryTopologySource;
  column: number;
  row: number;
  slot: PlantStationSlot;
  transform: PlantTransform;
  footprint: PlantRect;
  overlayAnchorId: string;
}

export interface PlantDockLayout {
  id: string;
  workflowId: string;
  terminal: GraphTerminal;
  transform: PlantTransform;
}

export interface PlantYardLayout {
  id: string;
  workflowId: string;
  transform: PlantTransform;
}

export interface PlantTrackTopologyInput {
  source: FactoryTopologySource;
  from: { stationId: string; column: number; row: number };
  to?:
    | { kind: "station"; id: string; column: number; row: number }
    | { kind: "dock"; id: string; terminal?: GraphTerminal };
  branch?: string;
  outcome?: string;
  terminal?: GraphTerminal;
}

export interface PlantTrackSegment {
  id: string;
  trackId: string;
  from: PlantWorldPoint;
  to: PlantWorldPoint;
  transform: PlantTransform;
}

export interface PlantTrackLayout {
  id: string;
  workflowId: string;
  kind: FactoryConveyor["kind"];
  active: boolean;
  topology: PlantTrackTopologyInput;
  points: PlantWorldPoint[];
  segments: PlantTrackSegment[];
}

export interface PlantCarrierAnchor {
  id: string;
  workflowId: string;
  holderId: string;
  stationId: string;
  rendered: boolean;
  active: boolean;
  state: FactoryCarrier["state"];
  position: PlantWorldPoint;
  transitionFrom?: PlantWorldPoint;
  overlayAnchorId?: string;
}

export interface PlantWorkerAnchor {
  id: string;
  workerId: string;
  stationId?: string;
  rendered: boolean;
  active: boolean;
  position: PlantWorldPoint;
  overlayAnchorId?: string;
}

export type PlantOverlayAnchorKind =
  | "overview"
  | "bay"
  | "station"
  | "carrier"
  | "worker"
  | "overflow";

export type PlantOverflowAnchorKind = "queued" | "runs" | "staff" | "ready";

export interface PlantOverlayAnchor {
  id: string;
  entityId: string;
  workflowId?: string;
  kind: PlantOverlayAnchorKind;
  position: PlantWorldPoint;
  priority: number;
  /** Present only on `overflow` anchors: what was truncated, and how much. */
  overflow?: { kind: PlantOverflowAnchorKind; count: number };
}

export interface PlantWorkflowBay {
  id: string;
  gaggle: string;
  workflow: string;
  displayName: string;
  source: FactoryTopologySource;
  allocation: PlantBayAllocation;
  bounds: PlantRect;
  cells: PlantBayCell[];
  machineIds: string[];
  trackIds: string[];
  dockIds: string[];
  yardId: string;
  summary: PlantAggregateSummary;
  overlayAnchorId: string;
}

export interface PlantInstanceTransform {
  id: string;
  transform: PlantTransform;
  active: boolean;
  animationKey?: string;
}

export interface PlantInstanceBatch {
  key: string;
  meshArchetype: string;
  materialArchetype: string;
  dimmedInRisk: boolean;
  active: boolean;
  instances: PlantInstanceTransform[];
}

export interface PlantLodLevel {
  anchorIds: string[];
  totalCandidates: number;
  truncated: boolean;
}

export interface PlantLodMetadata {
  thresholds: typeof FACTORY_PLANT_LOD_THRESHOLDS;
  levels: {
    detail: PlantLodLevel;
    bay: PlantLodLevel;
    overview: PlantLodLevel;
  };
  bays: Array<{
    id: string;
    bounds: PlantRect;
    projectedBounds: PlantProjectedBounds;
    summaryAnchorId: string;
    detailAnchorIds: string[];
  }>;
  maxDomItems: number;
}

export interface PlantAggregatePlan {
  workflows: number;
  stations: number;
  tracks: number;
  trackSegments: number;
  carriers: number;
  workers: number;
  instances: number;
  batches: number;
  drawCalls: {
    instancedPlan: number;
    currentRendererUpperBound: number;
    static: number;
  };
  dom: {
    detailCandidates: number;
    detailLimit: number;
    baySummaries: number;
    overview: number;
    maxAtAnyLod: number;
  };
}

export interface PlantLayoutMetrics {
  collisions: {
    bayCells: number;
    machines: number;
    duplicateStationCoordinates: number;
  };
  unresolvedTrackIds: string[];
  boundsFinite: boolean;
}

export interface PlantHallLayout {
  floor: PlantRect;
  wallHeight: number;
  beamCount: number;
  lightCount: number;
  commons?: PlantRect;
}

export interface FactoryPlantLayout {
  allocation: FactoryPlantAllocation;
  bays: PlantWorkflowBay[];
  machines: PlantMachineLayout[];
  docks: PlantDockLayout[];
  yards: PlantYardLayout[];
  tracks: PlantTrackLayout[];
  carriers: PlantCarrierAnchor[];
  workers: PlantWorkerAnchor[];
  overlayAnchors: PlantOverlayAnchor[];
  hall: PlantHallLayout;
  worldBounds: PlantAabb;
  projectedBounds: PlantProjectedBounds;
  aggregate: PlantAggregateSummary;
  instanceBatches: PlantInstanceBatch[];
  lod: PlantLodMetadata;
  aggregatePlan: PlantAggregatePlan;
  metrics: PlantLayoutMetrics;
}

export interface BuildFactoryPlantLayoutOptions {
  previous?: FactoryPlantAllocation | FactoryPlantLayout;
}

interface BayPlan {
  lane: FactoryLane;
  span: { x: number; z: number };
  stationSlots: Record<string, PlantStationSlot>;
}

interface BayGeometry {
  lane: FactoryLane;
  allocation: PlantBayAllocation;
  bounds: PlantRect;
  cells: PlantBayCell[];
  machines: PlantMachineLayout[];
  docks: PlantDockLayout[];
  yard: PlantYardLayout;
}

const CAMERA_BASIS = buildCameraBasis();

export function buildFactoryPlantLayout(
  model: FactoryFloorModel,
  options: BuildFactoryPlantLayoutOptions = {},
): FactoryPlantLayout {
  const previous = previousAllocation(options.previous);
  const plans = [...model.lanes]
    .sort((left, right) => left.id.localeCompare(right.id))
    .map((lane) => buildBayPlan(lane, previous?.bays[lane.id]));
  const allocations = allocateBays(plans, previous);
  const geometry = plans.map((plan) =>
    buildBayGeometry(plan.lane, allocations.get(plan.lane.id)!),
  );
  const machines = geometry
    .flatMap((bay) => bay.machines)
    .sort((left, right) => left.id.localeCompare(right.id));
  const docks = geometry
    .flatMap((bay) => bay.docks)
    .sort((left, right) => left.id.localeCompare(right.id));
  const yards = geometry
    .map((bay) => bay.yard)
    .sort((left, right) => left.id.localeCompare(right.id));
  const machineById = new Map(machines.map((machine) => [machine.id, machine]));
  const dockById = new Map(docks.map((dock) => [dock.id, dock]));
  const bayById = new Map(geometry.map((bay) => [bay.lane.id, bay]));
  const { tracks, unresolvedTrackIds } = buildTracks(
    plans.map((plan) => plan.lane),
    bayById,
    machineById,
    dockById,
  );
  orientMachinesFromTopology(machines, tracks);

  const carriers = buildCarrierAnchors(model, machineById, bayById);
  const commons = buildCommonsBounds(model);
  const workers = buildWorkerAnchors(model, machineById, commons);
  const carrierCountByBay = countBy(carriers, (carrier) => carrier.workflowId);
  const workerCountByBay = countBy(
    workers.filter((worker) => worker.stationId !== undefined),
    (worker) => machineById.get(worker.stationId ?? "")?.workflowId ?? "",
  );
  const trackIdsByBay = groupIds(tracks, (track) => track.workflowId);

  const bays = geometry.map((bay) => {
    const summary = summarizeStations(
      bay.lane.stations,
      carrierCountByBay.get(bay.lane.id) ?? 0,
      workerCountByBay.get(bay.lane.id) ?? 0,
      1,
    );
    return {
      id: bay.lane.id,
      gaggle: bay.lane.gaggle,
      workflow: bay.lane.workflow,
      displayName: bay.lane.displayName,
      source: bay.lane.source,
      allocation: bay.allocation,
      bounds: bay.bounds,
      cells: bay.cells,
      machineIds: bay.machines.map((machine) => machine.id).sort(),
      trackIds: trackIdsByBay.get(bay.lane.id) ?? [],
      dockIds: bay.docks.map((dock) => dock.id).sort(),
      yardId: bay.yard.id,
      summary,
      overlayAnchorId: `bay:${bay.lane.id}`,
    } satisfies PlantWorkflowBay;
  });

  const hall = buildHallLayout(bays, commons);
  const worldBounds = buildWorldBounds(hall);
  const projectedBounds = projectPlantAabb(worldBounds);
  const overlayAnchors = buildOverlayAnchors(
    bays,
    machines,
    carriers,
    workers,
    worldBounds,
    { hall, model, yards },
  );
  const aggregate = summarizeStations(
    model.stations,
    model.carriers.length,
    workers.length,
    bays.length,
  );
  const instanceBatches = buildInstanceBatches({
    bays,
    carriers,
    docks,
    hall,
    machines,
    tracks,
    workers,
    yards,
  });
  const lod = buildLodMetadata(bays, overlayAnchors);
  const aggregatePlan = buildAggregatePlan(
    bays,
    machines,
    tracks,
    carriers,
    workers,
    instanceBatches,
    lod,
  );
  const allocation: FactoryPlantAllocation = {
    version: FACTORY_PLANT_LAYOUT_VERSION,
    bays: Object.fromEntries(
      [...allocations]
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([id, value]) => [id, copyAllocation(value)]),
    ),
  };
  const metrics: PlantLayoutMetrics = {
    collisions: {
      bayCells: countBayCellCollisions(bays),
      machines: countMachineCollisions(machines),
      duplicateStationCoordinates: countDuplicateMachineCoordinates(machines),
    },
    unresolvedTrackIds: unresolvedTrackIds.sort(),
    boundsFinite: finiteValues(worldBounds) && finiteValues(projectedBounds),
  };

  return {
    allocation,
    bays,
    machines,
    docks,
    yards,
    tracks,
    carriers,
    workers,
    overlayAnchors,
    hall,
    worldBounds,
    projectedBounds,
    aggregate,
    instanceBatches,
    lod,
    aggregatePlan,
    metrics,
  };
}

export function fitFactoryPlantCamera(
  layoutOrBounds: FactoryPlantLayout | PlantAabb,
  aspect: number,
  padding:
    | number
    | {
        x: number;
        y: number;
      } = DEFAULT_CAMERA_PADDING,
): PlantCameraFit {
  const bounds =
    "worldBounds" in layoutOrBounds ? layoutOrBounds.worldBounds : layoutOrBounds;
  const projected =
    "projectedBounds" in layoutOrBounds
      ? layoutOrBounds.projectedBounds
      : projectPlantAabb(bounds);
  const safeAspect = Number.isFinite(aspect) && aspect > 0 ? aspect : 1;
  const safePadding =
    typeof padding === "number"
      ? {
          x: Math.max(0, finiteOr(padding, DEFAULT_CAMERA_PADDING)),
          y: Math.max(0, finiteOr(padding, DEFAULT_CAMERA_PADDING)),
        }
      : {
          x: Math.max(0, finiteOr(padding.x, DEFAULT_CAMERA_PADDING)),
          y: Math.max(0, finiteOr(padding.y, DEFAULT_CAMERA_PADDING)),
        };
  const contentWidth = projected.width + safePadding.x * 2;
  const contentHeight = projected.height + safePadding.y * 2;
  const viewHeight = Math.max(contentHeight, contentWidth / safeAspect);
  const viewWidth = viewHeight * safeAspect;
  const projectedCenterX = (projected.minX + projected.maxX) / 2;
  const projectedCenterY = (projected.minY + projected.maxY) / 2;
  const depthCenter = dot(bounds.center, CAMERA_BASIS.back);
  const target = add(
    add(
      multiply(CAMERA_BASIS.right, projectedCenterX),
      multiply(CAMERA_BASIS.up, projectedCenterY),
    ),
    multiply(CAMERA_BASIS.back, depthCenter),
  );
  const distance = Math.max(
    24,
    bounds.size.x * 1.4 + bounds.size.y * 2 + bounds.size.z * 1.4,
  );
  const position = add(target, multiply(CAMERA_BASIS.back, distance));

  return {
    aspect: round(safeAspect),
    padding: { x: round(safePadding.x), y: round(safePadding.y) },
    target: roundPoint(target),
    position: roundPoint(position),
    viewWidth: round(viewWidth),
    viewHeight: round(viewHeight),
    near: 0.1,
    far: round(Math.max(100, distance * 4)),
  };
}

/**
 * Fits the plant into the unobscured part of the canvas.
 *
 * The inspector is an edge overlay, not a resize: the canvas keeps its full
 * size and the drawer covers part of it. Fitting to the whole canvas would
 * therefore park a share of the plant underneath the panel that was opened to
 * explain it. This computes the fit for the safe rectangle and then widens the
 * camera back out to the full canvas, offsetting the target so the content
 * still lands inside the rectangle the operator can actually see.
 */
export function fitFactoryPlantCameraToSafeArea(
  layoutOrBounds: FactoryPlantLayout | PlantAabb,
  viewport: { width: number; height: number },
  safeArea?: PlantSafeAreaRect,
  padding:
    | number
    | {
        x: number;
        y: number;
      } = DEFAULT_CAMERA_PADDING,
): PlantCameraFit {
  const canvasWidth = finiteOr(viewport.width, 1) > 0 ? viewport.width : 1;
  const canvasHeight = finiteOr(viewport.height, 1) > 0 ? viewport.height : 1;
  const safe =
    safeArea && safeArea.width > 1 && safeArea.height > 1
      ? safeArea
      : { height: canvasHeight, left: 0, top: 0, width: canvasWidth };
  const base = fitFactoryPlantCamera(
    layoutOrBounds,
    safe.width / safe.height,
    padding,
  );
  const widthScale = canvasWidth / safe.width;
  const heightScale = canvasHeight / safe.height;
  const viewWidth = base.viewWidth * widthScale;
  const viewHeight = base.viewHeight * heightScale;
  const offsetX =
    ((safe.left + safe.width / 2 - canvasWidth / 2) / canvasWidth) * viewWidth;
  const offsetY =
    ((safe.top + safe.height / 2 - canvasHeight / 2) / canvasHeight) * viewHeight;
  const target = add(
    base.target,
    add(
      multiply(CAMERA_BASIS.right, -offsetX),
      multiply(CAMERA_BASIS.up, offsetY),
    ),
  );
  const distance = distanceBetween(base.position, base.target);
  const position = add(target, multiply(CAMERA_BASIS.back, distance));

  return {
    aspect: round(canvasWidth / canvasHeight),
    far: base.far,
    near: base.near,
    padding: base.padding,
    position: roundPoint(position),
    safeArea: {
      height: round(safe.height),
      left: round(safe.left),
      top: round(safe.top),
      width: round(safe.width),
    },
    target: roundPoint(target),
    viewHeight: round(viewHeight),
    viewWidth: round(viewWidth),
  };
}

export function projectPlantAabb(bounds: PlantAabb): PlantProjectedBounds {
  const points = aabbCorners(bounds).map(projectWorldPoint);
  const minX = Math.min(...points.map((point) => point.x));
  const maxX = Math.max(...points.map((point) => point.x));
  const minY = Math.min(...points.map((point) => point.y));
  const maxY = Math.max(...points.map((point) => point.y));
  return {
    minX: round(minX),
    minY: round(minY),
    maxX: round(maxX),
    maxY: round(maxY),
    width: round(maxX - minX),
    height: round(maxY - minY),
  };
}

function buildBayPlan(
  lane: FactoryLane,
  previous: PlantBayAllocation | undefined,
): BayPlan {
  const stationSlots = assignStationSlots(lane.stations, previous);
  const slots = Object.values(stationSlots);
  const maxColumn = slots.reduce(
    (maximum, slot) => Math.max(maximum, slot.column),
    0,
  );
  const maxRow = slots.reduce((maximum, slot) => Math.max(maximum, slot.row), 0);
  return {
    lane,
    span: {
      x: Math.max(
        previous?.span.x ?? 1,
        Math.ceil((maxColumn + 1) / FACTORY_PLANT_BAY_GRID.stationSlotsX),
      ),
      z: Math.max(
        previous?.span.z ?? 1,
        Math.ceil((maxRow + 1) / FACTORY_PLANT_BAY_GRID.stationSlotsZ),
      ),
    },
    stationSlots,
  };
}

function assignStationSlots(
  stations: readonly FactoryStation[],
  previous: PlantBayAllocation | undefined,
): Record<string, PlantStationSlot> {
  const ordered = [...stations].sort(
    (left, right) =>
      left.column - right.column ||
      left.row - right.row ||
      left.id.localeCompare(right.id),
  );
  const stationById = new Map(ordered.map((station) => [station.id, station]));
  const slots = new Map<string, PlantStationSlot>();
  const occupied = new Set<string>();

  for (const [stationId, slot] of Object.entries(previous?.stationSlots ?? {}).sort(
    ([left], [right]) => left.localeCompare(right),
  )) {
    if (
      !stationById.has(stationId) ||
      !Number.isInteger(slot.column) ||
      !Number.isInteger(slot.row) ||
      slot.column < 0 ||
      slot.row < 0
    ) {
      continue;
    }
    const key = slotKey(slot.column, slot.row);
    if (occupied.has(key)) {
      continue;
    }
    const station = stationById.get(stationId)!;
    slots.set(stationId, {
      ...slot,
      modelColumn: station.column,
      modelRow: station.row,
    });
    occupied.add(key);
  }

  let nextCompactSlot = 0;
  for (const station of ordered) {
    if (slots.has(station.id)) {
      continue;
    }
    let slot = compactStationSlot(nextCompactSlot);
    while (occupied.has(slotKey(slot.column, slot.row))) {
      nextCompactSlot += 1;
      slot = compactStationSlot(nextCompactSlot);
    }
    nextCompactSlot += 1;
    occupied.add(slotKey(slot.column, slot.row));
    slots.set(station.id, {
      ...slot,
      modelColumn: station.column,
      modelRow: station.row,
    });
  }

  return Object.fromEntries(
    [...slots].sort(([left], [right]) => left.localeCompare(right)),
  );
}

function compactStationSlot(index: number): { column: number; row: number } {
  const cell = Math.floor(index / STANDARD_BAY_STATION_CAPACITY);
  const local = index % STANDARD_BAY_STATION_CAPACITY;
  return {
    column:
      cell * FACTORY_PLANT_BAY_GRID.stationSlotsX +
      STANDARD_BAY_STATION_COLUMNS[
        local % STANDARD_BAY_STATION_COLUMNS.length
      ]!,
    row:
      STANDARD_BAY_STATION_ROWS[
        local % STANDARD_BAY_STATION_ROWS.length
      ]!,
  };
}

function allocateBays(
  plans: readonly BayPlan[],
  previous: FactoryPlantAllocation | undefined,
): Map<string, PlantBayAllocation> {
  const planById = new Map(plans.map((plan) => [plan.lane.id, plan]));
  const assignments = new Map<string, PlantBayAllocation>();
  const occupied = new Map<string, string>();
  const pending = new Set<string>();

  for (const plan of plans) {
    const prior = previous?.bays[plan.lane.id];
    if (!prior || !validGridPoint(prior.origin)) {
      pending.add(plan.lane.id);
      continue;
    }
    const allocation = allocationAt(
      plan,
      prior.origin,
      {
        x: Math.max(1, prior.span.x),
        z: Math.max(1, prior.span.z),
      },
    );
    assignments.set(plan.lane.id, allocation);
    occupy(occupied, allocation, plan.lane.id);
  }

  for (const plan of plans) {
    const allocation = assignments.get(plan.lane.id);
    if (!allocation) {
      continue;
    }
    if (
      plan.span.x <= allocation.span.x &&
      plan.span.z <= allocation.span.z
    ) {
      assignments.set(
        plan.lane.id,
        allocationAt(plan, allocation.origin, allocation.span),
      );
      continue;
    }

    vacate(occupied, allocation, plan.lane.id);
    const expanded = allocationAt(plan, allocation.origin, plan.span);
    if (canOccupy(occupied, expanded)) {
      assignments.set(plan.lane.id, expanded);
      occupy(occupied, expanded, plan.lane.id);
    } else {
      assignments.delete(plan.lane.id);
      pending.add(plan.lane.id);
    }
  }

  const totalArea = plans.reduce(
    (sum, plan) => sum + plan.span.x * plan.span.z,
    0,
  );
  const existingWidth = [...assignments.values()].reduce(
    (maximum, allocation) =>
      Math.max(maximum, allocation.origin.x + allocation.span.x),
    0,
  );
  let scanWidth = Math.max(1, existingWidth, Math.ceil(Math.sqrt(totalArea)));

  for (const workflowId of [...pending].sort()) {
    const plan = planById.get(workflowId);
    if (!plan) {
      continue;
    }
    scanWidth = Math.max(scanWidth, plan.span.x);
    const origin = findFirstFreeBayOrigin(occupied, plan.span, scanWidth);
    const allocation = allocationAt(plan, origin, plan.span);
    assignments.set(workflowId, allocation);
    occupy(occupied, allocation, workflowId);
  }

  return assignments;
}

function findFirstFreeBayOrigin(
  occupied: ReadonlyMap<string, string>,
  span: { x: number; z: number },
  scanWidth: number,
): PlantGridPoint {
  for (let z = 0; ; z += 1) {
    for (let x = 0; x <= scanWidth - span.x; x += 1) {
      const candidate: PlantBayAllocation = {
        workflowId: "",
        origin: { x, z },
        span,
        cells: rectangleCells({ x, z }, span),
        stationSlots: {},
      };
      if (canOccupy(occupied, candidate)) {
        return { x, z };
      }
    }
  }
}

function allocationAt(
  plan: BayPlan,
  origin: PlantGridPoint,
  span: { x: number; z: number },
): PlantBayAllocation {
  return {
    workflowId: plan.lane.id,
    origin: { ...origin },
    span: { ...span },
    cells: rectangleCells(origin, span),
    stationSlots: Object.fromEntries(
      Object.entries(plan.stationSlots).map(([id, slot]) => [id, { ...slot }]),
    ),
  };
}

function buildBayGeometry(
  lane: FactoryLane,
  allocation: PlantBayAllocation,
): BayGeometry {
  const cells = allocation.cells.map((grid) => {
    const bounds = cellBounds(grid);
    return {
      id: `${lane.id}#cell:${grid.x},${grid.z}`,
      workflowId: lane.id,
      grid: { ...grid },
      bounds,
      transform: transform(
        (bounds.minX + bounds.maxX) / 2,
        -0.02,
        (bounds.minZ + bounds.maxZ) / 2,
        0,
        bounds.width,
        0.16,
        bounds.depth,
      ),
    } satisfies PlantBayCell;
  });
  const bounds = unionRects(cells.map((cell) => cell.bounds));
  const machines = [...lane.stations]
    .sort((left, right) => left.id.localeCompare(right.id))
    .map((station) => {
      const slot = allocation.stationSlots[station.id];
      if (!slot) {
        throw new Error(`Factory Plant station slot missing for ${station.id}`);
      }
      return buildMachineLayout(station, allocation, slot);
    });
  const docks = [...lane.docks]
    .sort(
      (left, right) =>
        left.terminal.localeCompare(right.terminal) ||
        left.id.localeCompare(right.id),
    )
    .map((dock, index) => {
      const z = Math.min(
        bounds.maxZ - 1.2,
        bounds.minZ + 2 + index * 1.7,
      );
      return {
        id: dock.id,
        workflowId: lane.id,
        terminal: dock.terminal,
        transform: transform(bounds.maxX - 0.7, 0.16, z, 0, 1.1, 0.32, 1.1),
      } satisfies PlantDockLayout;
    });
  const yard: PlantYardLayout = {
    id: lane.yard.id,
    workflowId: lane.id,
    transform: transform(
      bounds.minX + 2.2,
      0.1,
      bounds.maxZ - 0.85,
      0,
      3.2,
      0.2,
      1.25,
    ),
  };
  return { allocation, bounds, cells, docks, lane, machines, yard };
}

function buildMachineLayout(
  station: FactoryStation,
  allocation: PlantBayAllocation,
  slot: PlantStationSlot,
): PlantMachineLayout {
  const position = stationSlotPosition(allocation, slot);
  const height =
    station.kind === "agentic"
      ? 1.9
      : station.kind === "gate"
        ? 1.55
        : station.kind === "parallel"
          ? 1.72
          : 1.62;
  return {
    id: station.id,
    workflowId: station.laneId,
    stageId: station.stageId,
    kind: station.kind,
    ...(station.evaluator ? { evaluator: station.evaluator } : {}),
    status: station.status,
    source: station.source,
    column: station.column,
    row: station.row,
    slot: { ...slot },
    transform: transform(
      position.x,
      height / 2 + 0.08,
      position.z,
      0,
      1.8,
      height,
      1.8,
    ),
    footprint: rect(
      position.x - MACHINE_HALF_FOOTPRINT,
      position.z - MACHINE_HALF_FOOTPRINT,
      position.x + MACHINE_HALF_FOOTPRINT,
      position.z + MACHINE_HALF_FOOTPRINT,
    ),
    overlayAnchorId: `station:${station.id}`,
  };
}

function stationSlotPosition(
  allocation: PlantBayAllocation,
  slot: PlantStationSlot,
): PlantWorldPoint {
  const cellX = Math.floor(
    slot.column / FACTORY_PLANT_BAY_GRID.stationSlotsX,
  );
  const cellZ = Math.floor(slot.row / FACTORY_PLANT_BAY_GRID.stationSlotsZ);
  const localColumn =
    slot.column - cellX * FACTORY_PLANT_BAY_GRID.stationSlotsX;
  const localRow = slot.row - cellZ * FACTORY_PLANT_BAY_GRID.stationSlotsZ;
  const gridX = allocation.origin.x + cellX;
  const gridZ = allocation.origin.z + cellZ;
  return roundPoint({
    x: gridX * CELL_PITCH_X + STATION_MIN_X + localColumn * STATION_PITCH_X,
    y: 0,
    z: gridZ * CELL_PITCH_Z + STATION_MIN_Z + localRow * STATION_PITCH_Z,
  });
}

function buildTracks(
  lanes: readonly FactoryLane[],
  bayById: ReadonlyMap<string, BayGeometry>,
  machineById: ReadonlyMap<string, PlantMachineLayout>,
  dockById: ReadonlyMap<string, PlantDockLayout>,
): { tracks: PlantTrackLayout[]; unresolvedTrackIds: string[] } {
  const tracks: PlantTrackLayout[] = [];
  const unresolvedTrackIds: string[] = [];

  for (const lane of [...lanes].sort((left, right) => left.id.localeCompare(right.id))) {
    const bay = bayById.get(lane.id);
    if (!bay) {
      continue;
    }
    const repasses = [...lane.conveyors]
      .filter((conveyor) => conveyor.kind === "repass")
      .sort((left, right) => left.id.localeCompare(right.id));
    for (const conveyor of [...lane.conveyors].sort((left, right) =>
      left.id.localeCompare(right.id),
    )) {
      const fromMachine = machineById.get(conveyor.fromStationId);
      const toMachine = machineById.get(conveyor.toId);
      const toDock = dockById.get(conveyor.toId);
      if (!fromMachine || (!toMachine && !toDock)) {
        unresolvedTrackIds.push(conveyor.id);
        continue;
      }
      const fromStation = lane.stations.find(
        (station) => station.id === conveyor.fromStationId,
      );
      const targetStation = toMachine
        ? lane.stations.find((station) => station.id === toMachine.id)
        : undefined;
      const from = groundPoint(fromMachine.transform.position);
      const to = groundPoint(
        toMachine?.transform.position ?? toDock!.transform.position,
      );
      const points = buildTrackPoints(
        conveyor,
        from,
        to,
        bay.bounds,
        Math.max(0, repasses.findIndex((candidate) => candidate.id === conveyor.id)),
      );
      const segments = points.slice(1).flatMap((point, index) => {
        const start = points[index];
        if (!start) {
          return [];
        }
        const segment = buildTrackSegment(conveyor.id, index, start, point);
        return segment ? [segment] : [];
      });
      tracks.push({
        id: conveyor.id,
        workflowId: lane.id,
        kind: conveyor.kind,
        active: conveyor.active,
        topology: {
          source: lane.source,
          from: {
            stationId: conveyor.fromStationId,
            column: fromStation?.column ?? fromMachine.column,
            row: fromStation?.row ?? fromMachine.row,
          },
          ...(targetStation
            ? {
                to: {
                  kind: "station" as const,
                  id: targetStation.id,
                  column: targetStation.column,
                  row: targetStation.row,
                },
              }
            : {
                to: {
                  kind: "dock" as const,
                  id: toDock!.id,
                  terminal: conveyor.terminal,
                },
              }),
          ...(conveyor.branch ? { branch: conveyor.branch } : {}),
          ...(conveyor.outcome ? { outcome: conveyor.outcome } : {}),
          ...(conveyor.terminal ? { terminal: conveyor.terminal } : {}),
        },
        points,
        segments,
      });
    }
  }

  return {
    tracks: tracks.sort((left, right) => left.id.localeCompare(right.id)),
    unresolvedTrackIds,
  };
}

function buildTrackPoints(
  conveyor: FactoryConveyor,
  from: PlantWorldPoint,
  to: PlantWorldPoint,
  bay: PlantRect,
  repassIndex: number,
): PlantWorldPoint[] {
  if (conveyor.kind === "repass") {
    const returnZ = Math.max(
      bay.minZ + 0.35,
      bay.maxZ - 0.8 - repassIndex * 0.22,
    );
    return compactPoints([
      from,
      { x: from.x, y: from.y, z: returnZ },
      { x: to.x, y: to.y, z: returnZ },
      to,
    ]);
  }
  if (Math.abs(from.z - to.z) < 0.01 || Math.abs(from.x - to.x) < 0.01) {
    return compactPoints([from, to]);
  }
  const middleX = round((from.x + to.x) / 2);
  return compactPoints([
    from,
    { x: middleX, y: from.y, z: from.z },
    { x: middleX, y: to.y, z: to.z },
    to,
  ]);
}

function buildTrackSegment(
  trackId: string,
  index: number,
  from: PlantWorldPoint,
  to: PlantWorldPoint,
): PlantTrackSegment | undefined {
  const dx = to.x - from.x;
  const dz = to.z - from.z;
  const length = Math.hypot(dx, dz);
  if (length < 0.01) {
    return undefined;
  }
  return {
    id: `${trackId}#segment:${index}`,
    trackId,
    from: roundPoint(from),
    to: roundPoint(to),
    transform: transform(
      (from.x + to.x) / 2,
      0.13,
      (from.z + to.z) / 2,
      Math.atan2(-dz, dx),
      length,
      0.18,
      0.42,
    ),
  };
}

function orientMachinesFromTopology(
  machines: PlantMachineLayout[],
  tracks: readonly PlantTrackLayout[],
): void {
  const outgoing = new Map<string, PlantTrackLayout[]>();
  for (const track of tracks) {
    const stationId = track.topology.from.stationId;
    const values = outgoing.get(stationId) ?? [];
    values.push(track);
    outgoing.set(stationId, values);
  }
  for (const machine of machines) {
    const track = outgoing
      .get(machine.id)
      ?.slice()
      .sort((left, right) => left.id.localeCompare(right.id))[0];
    const next = track?.points[1];
    const start = track?.points[0];
    if (!next || !start) {
      continue;
    }
    machine.transform.rotationY = round(
      Math.atan2(-(next.z - start.z), next.x - start.x),
    );
  }
}

function buildCarrierAnchors(
  model: FactoryFloorModel,
  machineById: ReadonlyMap<string, PlantMachineLayout>,
  bayById: ReadonlyMap<string, BayGeometry>,
): PlantCarrierAnchor[] {
  const groups = groupBy(
    model.carriers.filter((carrier) => carrier.rendered).sort(
      (left, right) =>
        left.stationId.localeCompare(right.stationId) ||
        left.queueIndex - right.queueIndex ||
        left.runId.localeCompare(right.runId),
    ),
    (carrier) => carrier.stationId,
  );
  const anchors: PlantCarrierAnchor[] = [];
  for (const [holderId, carriers] of [...groups].sort(([left], [right]) =>
    left.localeCompare(right),
  )) {
    carriers.forEach((carrier, index) => {
      const machine = machineById.get(holderId);
      const bay = bayById.get(carrier.laneId);
      const base = machine?.transform.position ??
        bay?.yard.transform.position ?? { x: 0, y: 0, z: 0 };
      const slot = machine ? carrier.renderSlot : index;
      if (slot === undefined) {
        return;
      }
      const columns = machine ? 2 : 4;
      const position = machine
        ? {
            x: base.x + 0.85 + (slot % columns) * 0.42,
            y: 0.34,
            z: base.z - 0.42 + Math.floor(slot / columns) * 0.42,
          }
        : {
            x: base.x - 1.1 + (slot % columns) * 0.72,
            y: 0.34,
            z: base.z + Math.floor(slot / columns) * 0.48,
          };
      const from = carrier.transition?.fromStationId
        ? machineById.get(carrier.transition.fromStationId)?.transform.position
        : undefined;
      anchors.push({
        id: carrier.runId,
        workflowId: carrier.laneId,
        holderId,
        stationId: carrier.stationId,
        rendered: carrier.rendered,
        active:
          carrier.confirmed &&
          (carrier.state === "running" || carrier.state === "starting"),
        state: carrier.state,
        position: roundPoint(position),
        ...(from ? { transitionFrom: roundPoint({ ...from, y: 0.34 }) } : {}),
        ...(carrier.rendered
          ? { overlayAnchorId: `carrier:${carrier.runId}` }
          : {}),
      });
    });
  }
  return anchors.sort((left, right) => left.id.localeCompare(right.id));
}

function buildCommonsBounds(model: FactoryFloorModel): PlantRect | undefined {
  const hasCommonsWorkers = model.workers.some((worker) =>
    worker.placements.some((placement) => !placement.stationId),
  );
  if (!hasCommonsWorkers) {
    return undefined;
  }
  return rect(
    -CELL_PITCH_X,
    0,
    -CELL_PITCH_X + FACTORY_PLANT_BAY_GRID.cellWidth,
    FACTORY_PLANT_BAY_GRID.cellDepth,
  );
}

function buildWorkerAnchors(
  model: FactoryFloorModel,
  machineById: ReadonlyMap<string, PlantMachineLayout>,
  commons: PlantRect | undefined,
): PlantWorkerAnchor[] {
  const entries = model.workers.flatMap((worker) =>
    worker.placements.map((placement) => ({ placement, workerId: worker.id })),
  );
  const groups = groupBy(
    entries.sort(
      (left, right) =>
        (left.placement.stationId ?? "").localeCompare(
          right.placement.stationId ?? "",
        ) || left.placement.id.localeCompare(right.placement.id),
    ),
    (entry) => entry.placement.stationId ?? "__commons__",
  );
  const anchors: PlantWorkerAnchor[] = [];
  for (const [holderId, placements] of [...groups].sort(([left], [right]) =>
    left.localeCompare(right),
  )) {
    placements.forEach(({ placement, workerId }, index) => {
      const machine = placement.stationId
        ? machineById.get(placement.stationId)
        : undefined;
      const position = machine
        ? {
            x: machine.transform.position.x + 0.74 + (index % 2) * 0.38,
            y: 0,
            z: machine.transform.position.z - 0.42 + Math.floor(index / 2) * 0.42,
          }
        : commons
          ? {
              x: commons.minX + 2 + (index % 3) * 5,
              y: 0,
              z: commons.minZ + 2 + Math.floor(index / 3) * 4,
            }
          : { x: -2, y: 0, z: 2 + index };
      anchors.push(workerAnchor(placement, workerId, position));
    });
  }
  return anchors.sort((left, right) => left.id.localeCompare(right.id));
}

function workerAnchor(
  placement: FactoryWorkerPlacement,
  workerId: string,
  position: PlantWorldPoint,
): PlantWorkerAnchor {
  return {
    id: placement.id,
    workerId,
    ...(placement.stationId ? { stationId: placement.stationId } : {}),
    rendered: placement.rendered,
    active: placement.active,
    position: roundPoint(position),
    ...(placement.rendered ? { overlayAnchorId: `worker:${placement.id}` } : {}),
  };
}

function buildHallLayout(
  bays: readonly PlantWorkflowBay[],
  commons: PlantRect | undefined,
): PlantHallLayout {
  const cells = bays.flatMap((bay) => bay.cells.map((cell) => cell.bounds));
  const occupied = [...cells, ...(commons ? [commons] : [])];
  const content =
    occupied.length > 0 ? unionRects(occupied) : rect(0, 0, 32, 32);
  return {
    floor: rect(
      content.minX - FLOOR_MARGIN,
      content.minZ - FLOOR_MARGIN,
      content.maxX + FLOOR_MARGIN,
      content.maxZ + FLOOR_MARGIN,
    ),
    wallHeight: HALL_HEIGHT,
    beamCount: 8,
    lightCount: 6,
    ...(commons ? { commons } : {}),
  };
}

function buildWorldBounds(hall: PlantHallLayout): PlantAabb {
  const minimum = {
    x: hall.floor.minX - 0.25,
    y: -0.55,
    z: hall.floor.minZ - 0.25,
  };
  const maximum = {
    x: hall.floor.maxX + 0.25,
    y: hall.wallHeight,
    z: hall.floor.maxZ + 0.25,
  };
  return aabb(minimum, maximum);
}

function buildOverlayAnchors(
  bays: readonly PlantWorkflowBay[],
  machines: readonly PlantMachineLayout[],
  carriers: readonly PlantCarrierAnchor[],
  workers: readonly PlantWorkerAnchor[],
  worldBounds: PlantAabb,
  context: {
    hall: PlantHallLayout;
    model: FactoryFloorModel;
    yards: readonly PlantYardLayout[];
  },
): PlantOverlayAnchor[] {
  const anchors: PlantOverlayAnchor[] = [
    {
      id: "overview:plant",
      entityId: "plant",
      kind: "overview",
      position: roundPoint({
        x: worldBounds.center.x,
        y: worldBounds.max.y,
        z: worldBounds.center.z,
      }),
      priority: 0,
    },
  ];
  for (const bay of bays) {
    anchors.push({
      id: bay.overlayAnchorId,
      entityId: bay.id,
      workflowId: bay.id,
      kind: "bay",
      position: roundPoint({
        x: bay.bounds.minX + 1.2,
        y: 1.55,
        z: bay.bounds.maxZ - 0.75,
      }),
      priority: 10,
    });
  }
  for (const machine of machines) {
    anchors.push({
      id: machine.overlayAnchorId,
      entityId: machine.id,
      workflowId: machine.workflowId,
      kind: "station",
      position: roundPoint({
        ...machine.transform.position,
        y: machine.transform.position.y + machine.transform.scale.y / 2 + 0.35,
      }),
      priority:
        machine.status === "blocked"
          ? 100
          : machine.status === "held"
            ? 90
            : machine.status === "unknown"
              ? 80
              : machine.status === "running"
                ? 70
                : 50,
    });
  }
  for (const carrier of carriers) {
    if (!carrier.overlayAnchorId) {
      continue;
    }
    anchors.push({
      id: carrier.overlayAnchorId,
      entityId: carrier.id,
      workflowId: carrier.workflowId,
      kind: "carrier",
      position: roundPoint({ ...carrier.position, y: carrier.position.y + 0.35 }),
      priority:
        carrier.state === "blocked"
          ? 98
          : carrier.state === "paused"
            ? 88
            : 65,
    });
  }
  for (const worker of workers) {
    if (!worker.overlayAnchorId) {
      continue;
    }
    anchors.push({
      id: worker.overlayAnchorId,
      entityId: worker.id,
      ...(worker.stationId
        ? { workflowId: machines.find((machine) => machine.id === worker.stationId)?.workflowId }
        : {}),
      kind: "worker",
      position: roundPoint({ ...worker.position, y: 0.9 }),
      priority: worker.active ? 60 : 40,
    });
  }
  anchors.push(
    ...buildOverflowAnchors(bays, machines, worldBounds, context),
  );
  return anchors.sort(
    (left, right) =>
      right.priority - left.priority || left.id.localeCompare(right.id),
  );
}

/**
 * Truncation affordances, anchored in world space like everything else.
 *
 * The plant caps how many crates and goobers it draws. Those caps are facts
 * about the read, so the `+N` controls that disclose them are projected from
 * the same camera as the machines they belong to instead of being pinned to
 * bitmap coordinates that no longer describe the scene.
 */
function buildOverflowAnchors(
  bays: readonly PlantWorkflowBay[],
  machines: readonly PlantMachineLayout[],
  worldBounds: PlantAabb,
  context: {
    hall: PlantHallLayout;
    model: FactoryFloorModel;
    yards: readonly PlantYardLayout[];
  },
): PlantOverlayAnchor[] {
  const anchors: PlantOverlayAnchor[] = [];
  const yardByWorkflow = new Map(
    context.yards.map((yard) => [yard.workflowId, yard]),
  );
  const bayById = new Map(bays.map((bay) => [bay.id, bay]));
  const machineById = new Map(machines.map((machine) => [machine.id, machine]));

  for (const lane of context.model.lanes) {
    if (lane.yard.overflowRunCount > 0) {
      const yard = yardByWorkflow.get(lane.id);
      const bay = bayById.get(lane.id);
      const position = yard
        ? { x: yard.transform.position.x, y: 1.1, z: yard.transform.position.z }
        : bay
          ? { x: bay.bounds.minX + 1.2, y: 1.1, z: bay.bounds.minZ + 1.2 }
          : undefined;
      if (position) {
        anchors.push({
          id: `overflow:queued:${lane.id}`,
          entityId: lane.id,
          workflowId: lane.id,
          kind: "overflow",
          overflow: { count: lane.yard.overflowRunCount, kind: "queued" },
          position: roundPoint(position),
          priority: 55,
        });
      }
    }
    for (const station of lane.stations) {
      const machine = machineById.get(station.id);
      if (!machine) {
        continue;
      }
      const top =
        machine.transform.position.y + machine.transform.scale.y / 2 + 0.35;
      if (station.overflowRunCount > 0) {
        anchors.push({
          id: `overflow:runs:${station.id}`,
          entityId: station.id,
          workflowId: machine.workflowId,
          kind: "overflow",
          overflow: { count: station.overflowRunCount, kind: "runs" },
          position: roundPoint({
            x: machine.transform.position.x,
            y: top,
            z: machine.transform.position.z + 1,
          }),
          priority: 66,
        });
      }
      if (station.workerOverflowCount > 0) {
        anchors.push({
          id: `overflow:staff:${station.id}`,
          entityId: station.id,
          workflowId: machine.workflowId,
          kind: "overflow",
          overflow: { count: station.workerOverflowCount, kind: "staff" },
          position: roundPoint({
            x: machine.transform.position.x + 1,
            y: top,
            z: machine.transform.position.z,
          }),
          priority: 64,
        });
      }
    }
  }

  if (context.model.commons.overflowWorkerCount > 0) {
    const commons = context.hall.commons;
    anchors.push({
      id: "overflow:ready:commons",
      entityId: "commons",
      kind: "overflow",
      overflow: {
        count: context.model.commons.overflowWorkerCount,
        kind: "ready",
      },
      position: roundPoint(
        commons
          ? {
              x: (commons.minX + commons.maxX) / 2,
              y: 1.1,
              z: (commons.minZ + commons.maxZ) / 2,
            }
          : { x: worldBounds.center.x, y: 1.1, z: worldBounds.max.z - 1.5 },
      ),
      priority: 45,
    });
  }

  return anchors;
}

function buildInstanceBatches(input: {
  bays: readonly PlantWorkflowBay[];
  machines: readonly PlantMachineLayout[];
  docks: readonly PlantDockLayout[];
  yards: readonly PlantYardLayout[];
  tracks: readonly PlantTrackLayout[];
  carriers: readonly PlantCarrierAnchor[];
  workers: readonly PlantWorkerAnchor[];
  hall: PlantHallLayout;
}): PlantInstanceBatch[] {
  const batches = new Map<string, PlantInstanceBatch>();
  const add = (
    meshArchetype: string,
    materialArchetype: string,
    dimmedInRisk: boolean,
    instance: PlantInstanceTransform,
  ) => {
    const key = `${meshArchetype}|${materialArchetype}|${
      dimmedInRisk ? "dim" : "full"
    }`;
    const batch = batches.get(key) ?? {
      key,
      meshArchetype,
      materialArchetype,
      dimmedInRisk,
      active: false,
      instances: [],
    };
    batch.instances.push(instance);
    batch.active ||= instance.active;
    batches.set(key, batch);
  };

  for (const bay of input.bays) {
    for (const cell of bay.cells) {
      add("bay-pad", `bay:${bay.summary.status}`, healthyStatus(bay.summary.status), {
        id: cell.id,
        transform: cell.transform,
        active: bay.summary.status === "running",
      });
      add("bay-edge", `bay-edge:${bay.summary.status}`, healthyStatus(bay.summary.status), {
        id: `${cell.id}#edge`,
        transform: transform(
          (cell.bounds.minX + cell.bounds.maxX) / 2,
          0.09,
          cell.bounds.minZ + 0.1,
          0,
          cell.bounds.width,
          0.08,
          0.12,
        ),
        active: bay.summary.status === "running",
      });
    }
  }
  if (input.hall.commons) {
    const commons = input.hall.commons;
    add("commons-pad", "commons", true, {
      id: "commons",
      transform: transform(
        (commons.minX + commons.maxX) / 2,
        -0.01,
        (commons.minZ + commons.maxZ) / 2,
        0,
        commons.width,
        0.14,
        commons.depth,
      ),
      active: false,
    });
  }
  for (const machine of input.machines) {
    add(
      `machine:${machine.evaluator ? "evaluator" : machine.kind}`,
      `machine:${machine.status}`,
      machine.status === "idle" || machine.status === "running",
      {
        id: machine.id,
        transform: machine.transform,
        active: machine.status === "running",
        animationKey: machine.id,
      },
    );
  }
  for (const track of input.tracks) {
    for (const segment of track.segments) {
      add(
        `track:${track.kind}`,
        `track:${track.kind}:${track.active ? "active" : "idle"}`,
        !track.active,
        {
          id: segment.id,
          transform: segment.transform,
          active: track.active,
          animationKey: track.id,
        },
      );
    }
  }
  for (const dock of input.docks) {
    add("dock", `dock:${dock.terminal}`, false, {
      id: dock.id,
      transform: dock.transform,
      active: false,
    });
  }
  for (const yard of input.yards) {
    add("yard", "yard", true, {
      id: yard.id,
      transform: yard.transform,
      active: false,
    });
  }
  for (const carrier of input.carriers.filter((candidate) => candidate.rendered)) {
    add(
      "carrier",
      `carrier:${carrier.state}`,
      carrier.state !== "blocked" && carrier.state !== "paused",
      {
        id: carrier.id,
        transform: transform(
          carrier.position.x,
          carrier.position.y,
          carrier.position.z,
          0,
          0.52,
          0.4,
          0.52,
        ),
        active: carrier.active,
        animationKey: carrier.id,
      },
    );
  }
  for (const worker of input.workers.filter((candidate) => candidate.rendered)) {
    add("worker", worker.active ? "worker:active" : "worker:idle", !worker.active, {
      id: worker.id,
      transform: transform(
        worker.position.x,
        0.48,
        worker.position.z,
        0,
        0.42,
        0.88,
        0.42,
      ),
      active: worker.active,
      animationKey: worker.id,
    });
  }

  return [...batches.values()]
    .map((batch) => ({
      ...batch,
      instances: batch.instances.sort((left, right) => left.id.localeCompare(right.id)),
    }))
    .sort((left, right) => left.key.localeCompare(right.key));
}

function buildLodMetadata(
  bays: readonly PlantWorkflowBay[],
  overlayAnchors: readonly PlantOverlayAnchor[],
): PlantLodMetadata {
  const detailCandidates = overlayAnchors
    .filter((anchor) => anchor.kind !== "overview" && anchor.kind !== "bay")
    .map((anchor) => anchor.id);
  const detailAnchorIds = detailCandidates.slice(
    0,
    FACTORY_PLANT_LOD_THRESHOLDS.maxDetailDomItems,
  );
  const bayCandidates = overlayAnchors
    .filter((anchor) => anchor.kind === "bay")
    .map((anchor) => anchor.id);
  const bayAnchorIds = bayCandidates.slice(
    0,
    FACTORY_PLANT_LOD_THRESHOLDS.maxDetailDomItems,
  );
  const overviewAnchorIds = overlayAnchors
    .filter((anchor) => anchor.kind === "overview")
    .map((anchor) => anchor.id)
    .slice(0, 1);
  const detailSet = new Set(detailAnchorIds);
  return {
    thresholds: FACTORY_PLANT_LOD_THRESHOLDS,
    levels: {
      detail: {
        anchorIds: detailAnchorIds,
        totalCandidates: detailCandidates.length,
        truncated: detailAnchorIds.length < detailCandidates.length,
      },
      bay: {
        anchorIds: bayAnchorIds,
        totalCandidates: bayCandidates.length,
        truncated: bayAnchorIds.length < bayCandidates.length,
      },
      overview: {
        anchorIds: overviewAnchorIds,
        totalCandidates: 1,
        truncated: false,
      },
    },
    bays: bays.map((bay) => ({
      id: bay.id,
      bounds: bay.bounds,
      projectedBounds: projectPlantAabb(
        aabb(
          { x: bay.bounds.minX, y: -0.1, z: bay.bounds.minZ },
          { x: bay.bounds.maxX, y: 1.8, z: bay.bounds.maxZ },
        ),
      ),
      summaryAnchorId: bay.overlayAnchorId,
      detailAnchorIds: overlayAnchors
        .filter(
          (anchor) => anchor.workflowId === bay.id && detailSet.has(anchor.id),
        )
        .map((anchor) => anchor.id),
    })),
    maxDomItems: Math.max(
      detailAnchorIds.length,
      bayAnchorIds.length,
      overviewAnchorIds.length,
    ),
  };
}

function buildAggregatePlan(
  bays: readonly PlantWorkflowBay[],
  machines: readonly PlantMachineLayout[],
  tracks: readonly PlantTrackLayout[],
  carriers: readonly PlantCarrierAnchor[],
  workers: readonly PlantWorkerAnchor[],
  batches: readonly PlantInstanceBatch[],
  lod: PlantLodMetadata,
): PlantAggregatePlan {
  const renderedCarriers = carriers.filter((carrier) => carrier.rendered).length;
  const renderedWorkers = workers.filter((worker) => worker.rendered).length;
  const currentBatchCount = batches.filter(
    (batch) =>
      batch.meshArchetype !== "carrier" && batch.meshArchetype !== "worker",
  ).length;
  const mainPassDrawCalls =
    STATIC_DRAW_CALLS +
    currentBatchCount +
    renderedCarriers +
    renderedWorkers * 2;
  const instances = batches.reduce(
    (total, batch) => total + batch.instances.length,
    0,
  );
  return {
    workflows: bays.length,
    stations: machines.length,
    tracks: tracks.length,
    trackSegments: tracks.reduce(
      (total, track) => total + track.segments.length,
      0,
    ),
    carriers: carriers.length,
    workers: workers.length,
    instances,
    batches: batches.length,
    drawCalls: {
      instancedPlan: STATIC_DRAW_CALLS + batches.length,
      // Main colour pass plus a conservative allowance for shadow/depth passes.
      // The multiplier stays independent of station and track cardinality.
      currentRendererUpperBound: mainPassDrawCalls * 3,
      static: STATIC_DRAW_CALLS,
    },
    dom: {
      detailCandidates: lod.levels.detail.totalCandidates,
      detailLimit: FACTORY_PLANT_LOD_THRESHOLDS.maxDetailDomItems,
      baySummaries: lod.levels.bay.totalCandidates,
      overview: 1,
      maxAtAnyLod: lod.maxDomItems,
    },
  };
}

function summarizeStations(
  stations: readonly FactoryStation[],
  carriers: number,
  workers: number,
  workflows: number,
): PlantAggregateSummary {
  const statusCounts: PlantStatusCounts = {
    blocked: 0,
    held: 0,
    idle: 0,
    impeded: 0,
    running: 0,
    unknown: 0,
  };
  let wip = 0;
  for (const station of stations) {
    statusCounts[station.status] += 1;
    wip += station.wip;
  }
  return {
    status: aggregateStatus(statusCounts),
    workflows,
    stages: stations.length,
    wip,
    carriers,
    workers,
    statusCounts,
    attentionStages: statusCounts.blocked + statusCounts.impeded,
  };
}

function aggregateStatus(counts: PlantStatusCounts): PlantAggregateStatus {
  if (counts.blocked + counts.impeded > 0) {
    return "blocked";
  }
  if (counts.held > 0) {
    return "held";
  }
  if (counts.unknown > 0) {
    return "unknown";
  }
  if (counts.running > 0) {
    return "running";
  }
  return "idle";
}

function countBayCellCollisions(bays: readonly PlantWorkflowBay[]): number {
  const occupied = new Set<string>();
  let collisions = 0;
  for (const cell of bays.flatMap((bay) => bay.allocation.cells)) {
    const key = slotKey(cell.x, cell.z);
    if (occupied.has(key)) {
      collisions += 1;
    } else {
      occupied.add(key);
    }
  }
  return collisions;
}

function countMachineCollisions(
  machines: readonly PlantMachineLayout[],
): number {
  const ordered = [...machines].sort(
    (left, right) =>
      left.footprint.minX - right.footprint.minX ||
      left.footprint.minZ - right.footprint.minZ ||
      left.id.localeCompare(right.id),
  );
  let collisions = 0;
  for (let leftIndex = 0; leftIndex < ordered.length; leftIndex += 1) {
    const left = ordered[leftIndex]!;
    for (
      let rightIndex = leftIndex + 1;
      rightIndex < ordered.length;
      rightIndex += 1
    ) {
      const right = ordered[rightIndex]!;
      if (right.footprint.minX >= left.footprint.maxX) {
        break;
      }
      if (rectsOverlap(left.footprint, right.footprint)) {
        collisions += 1;
      }
    }
  }
  return collisions;
}

function countDuplicateMachineCoordinates(
  machines: readonly PlantMachineLayout[],
): number {
  const coordinates = new Set<string>();
  let duplicates = 0;
  for (const machine of machines) {
    const key = `${machine.transform.position.x},${machine.transform.position.z}`;
    if (coordinates.has(key)) {
      duplicates += 1;
    } else {
      coordinates.add(key);
    }
  }
  return duplicates;
}

function buildCameraBasis() {
  const back = normalize(FACTORY_PLANT_CAMERA_DIRECTION);
  const right = normalize({ x: back.z, y: 0, z: -back.x });
  const up = normalize(cross(back, right));
  return { back, right, up };
}

function projectWorldPoint(point: PlantWorldPoint): { x: number; y: number } {
  return {
    x: dot(point, CAMERA_BASIS.right),
    y: dot(point, CAMERA_BASIS.up),
  };
}

function aabbCorners(bounds: PlantAabb): PlantWorldPoint[] {
  const points: PlantWorldPoint[] = [];
  for (const x of [bounds.min.x, bounds.max.x]) {
    for (const y of [bounds.min.y, bounds.max.y]) {
      for (const z of [bounds.min.z, bounds.max.z]) {
        points.push({ x, y, z });
      }
    }
  }
  return points;
}

function aabb(minimum: PlantWorldPoint, maximum: PlantWorldPoint): PlantAabb {
  return {
    min: roundPoint(minimum),
    max: roundPoint(maximum),
    size: roundPoint({
      x: maximum.x - minimum.x,
      y: maximum.y - minimum.y,
      z: maximum.z - minimum.z,
    }),
    center: roundPoint({
      x: (minimum.x + maximum.x) / 2,
      y: (minimum.y + maximum.y) / 2,
      z: (minimum.z + maximum.z) / 2,
    }),
  };
}

function transform(
  x: number,
  y: number,
  z: number,
  rotationY: number,
  scaleX: number,
  scaleY: number,
  scaleZ: number,
): PlantTransform {
  return {
    position: roundPoint({ x, y, z }),
    rotationY: round(rotationY),
    scale: {
      x: round(scaleX),
      y: round(scaleY),
      z: round(scaleZ),
    },
  };
}

function cellBounds(grid: PlantGridPoint): PlantRect {
  const minX = grid.x * CELL_PITCH_X;
  const minZ = grid.z * CELL_PITCH_Z;
  return rect(
    minX,
    minZ,
    minX + FACTORY_PLANT_BAY_GRID.cellWidth,
    minZ + FACTORY_PLANT_BAY_GRID.cellDepth,
  );
}

function rect(
  minX: number,
  minZ: number,
  maxX: number,
  maxZ: number,
): PlantRect {
  return {
    minX: round(minX),
    minZ: round(minZ),
    maxX: round(maxX),
    maxZ: round(maxZ),
    width: round(maxX - minX),
    depth: round(maxZ - minZ),
  };
}

function unionRects(rectangles: readonly PlantRect[]): PlantRect {
  if (rectangles.length === 0) {
    return rect(0, 0, 0, 0);
  }
  return rect(
    Math.min(...rectangles.map((value) => value.minX)),
    Math.min(...rectangles.map((value) => value.minZ)),
    Math.max(...rectangles.map((value) => value.maxX)),
    Math.max(...rectangles.map((value) => value.maxZ)),
  );
}

function rectangleCells(
  origin: PlantGridPoint,
  span: { x: number; z: number },
): PlantGridPoint[] {
  const cells: PlantGridPoint[] = [];
  for (let z = 0; z < span.z; z += 1) {
    for (let x = 0; x < span.x; x += 1) {
      cells.push({ x: origin.x + x, z: origin.z + z });
    }
  }
  return cells;
}

function occupy(
  occupied: Map<string, string>,
  allocation: PlantBayAllocation,
  workflowId: string,
): void {
  for (const cell of allocation.cells) {
    occupied.set(slotKey(cell.x, cell.z), workflowId);
  }
}

function vacate(
  occupied: Map<string, string>,
  allocation: PlantBayAllocation,
  workflowId: string,
): void {
  for (const cell of allocation.cells) {
    const key = slotKey(cell.x, cell.z);
    if (occupied.get(key) === workflowId) {
      occupied.delete(key);
    }
  }
}

function canOccupy(
  occupied: ReadonlyMap<string, string>,
  allocation: PlantBayAllocation,
): boolean {
  return allocation.cells.every(
    (cell) => !occupied.has(slotKey(cell.x, cell.z)),
  );
}

function previousAllocation(
  previous: FactoryPlantAllocation | FactoryPlantLayout | undefined,
): FactoryPlantAllocation | undefined {
  if (!previous) {
    return undefined;
  }
  return "allocation" in previous ? previous.allocation : previous;
}

function copyAllocation(value: PlantBayAllocation): PlantBayAllocation {
  return {
    workflowId: value.workflowId,
    origin: { ...value.origin },
    span: { ...value.span },
    cells: value.cells.map((cell) => ({ ...cell })),
    stationSlots: Object.fromEntries(
      Object.entries(value.stationSlots)
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([id, slot]) => [id, { ...slot }]),
    ),
  };
}

function compactPoints(points: readonly PlantWorldPoint[]): PlantWorldPoint[] {
  const result: PlantWorldPoint[] = [];
  for (const point of points) {
    const rounded = roundPoint(point);
    const previous = result.at(-1);
    if (
      previous &&
      previous.x === rounded.x &&
      previous.y === rounded.y &&
      previous.z === rounded.z
    ) {
      continue;
    }
    result.push(rounded);
  }
  return result;
}

function groundPoint(point: PlantWorldPoint): PlantWorldPoint {
  return roundPoint({ x: point.x, y: 0.12, z: point.z });
}

function healthyStatus(status: PlantAggregateStatus): boolean {
  return status === "idle" || status === "running";
}

function groupBy<T>(
  values: readonly T[],
  keyOf: (value: T) => string,
): Map<string, T[]> {
  const groups = new Map<string, T[]>();
  for (const value of values) {
    const key = keyOf(value);
    const entries = groups.get(key) ?? [];
    entries.push(value);
    groups.set(key, entries);
  }
  return groups;
}

function countBy<T>(
  values: readonly T[],
  keyOf: (value: T) => string,
): Map<string, number> {
  const counts = new Map<string, number>();
  for (const value of values) {
    const key = keyOf(value);
    if (!key) {
      continue;
    }
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  return counts;
}

function groupIds<T extends { id: string }>(
  values: readonly T[],
  keyOf: (value: T) => string,
): Map<string, string[]> {
  const groups = new Map<string, string[]>();
  for (const value of values) {
    const key = keyOf(value);
    const ids = groups.get(key) ?? [];
    ids.push(value.id);
    groups.set(key, ids);
  }
  for (const ids of groups.values()) {
    ids.sort();
  }
  return groups;
}

function slotKey(x: number, z: number): string {
  return `${x},${z}`;
}

function validGridPoint(point: PlantGridPoint): boolean {
  return (
    Number.isInteger(point.x) &&
    Number.isInteger(point.z) &&
    point.x >= 0 &&
    point.z >= 0
  );
}

function rectsOverlap(left: PlantRect, right: PlantRect): boolean {
  return (
    left.minX < right.maxX &&
    left.maxX > right.minX &&
    left.minZ < right.maxZ &&
    left.maxZ > right.minZ
  );
}

function finiteValues(value: unknown): boolean {
  if (typeof value === "number") {
    return Number.isFinite(value);
  }
  if (Array.isArray(value)) {
    return value.every(finiteValues);
  }
  if (value && typeof value === "object") {
    return Object.values(value).every(finiteValues);
  }
  return true;
}

function dot(left: PlantWorldPoint, right: PlantWorldPoint): number {
  return left.x * right.x + left.y * right.y + left.z * right.z;
}

function cross(left: PlantWorldPoint, right: PlantWorldPoint): PlantWorldPoint {
  return {
    x: left.y * right.z - left.z * right.y,
    y: left.z * right.x - left.x * right.z,
    z: left.x * right.y - left.y * right.x,
  };
}

function normalize(point: PlantWorldPoint): PlantWorldPoint {
  const length = Math.hypot(point.x, point.y, point.z) || 1;
  return {
    x: point.x / length,
    y: point.y / length,
    z: point.z / length,
  };
}

function add(left: PlantWorldPoint, right: PlantWorldPoint): PlantWorldPoint {
  return {
    x: left.x + right.x,
    y: left.y + right.y,
    z: left.z + right.z,
  };
}

function multiply(point: PlantWorldPoint, scalar: number): PlantWorldPoint {
  return {
    x: point.x * scalar,
    y: point.y * scalar,
    z: point.z * scalar,
  };
}

function distanceBetween(left: PlantWorldPoint, right: PlantWorldPoint): number {
  return Math.hypot(left.x - right.x, left.y - right.y, left.z - right.z);
}

function finiteOr(value: number, fallback: number): number {
  return Number.isFinite(value) ? value : fallback;
}

function round(value: number): number {
  return Math.round(value * 1_000_000) / 1_000_000;
}

function roundPoint(point: PlantWorldPoint): PlantWorldPoint {
  return {
    x: round(point.x),
    y: round(point.y),
    z: round(point.z),
  };
}
