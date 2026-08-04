import {
  carrierLabel,
  laneLabel,
  stationLabel,
  workerLabel,
} from "./factoryLabels";
import type {
  FactoryFloorModel,
  FactoryLens,
  FactoryStation,
  FactoryWorker,
  FactoryWorkerPlacement,
} from "./factoryModel";
import type {
  FactoryPlantLayout,
  PlantOverlayAnchor,
  PlantOverlayAnchorKind,
  PlantWorldPoint,
} from "./factoryPlantLayout";
import { isSelected, type FactorySelection } from "./factorySelection";
import {
  estimatePlantLabelWidth,
  PLANT_HIT_TARGET_MIN,
  PLANT_LABEL_HEIGHT,
  type PlantLabelTier,
} from "./plantLabelPacking";
import {
  assessCarrierRisk,
  assessStationRisk,
  plantRiskLevelLabel,
  plantRiskMarkerShape,
  type PlantRiskLevel,
  type PlantRiskMarkerShape,
} from "./plantRisk";

/**
 * One projected, selectable thing on the plant.
 *
 * The overlay is derived only from canonical layout anchors and the model, so
 * every positioned semantic on screen has exactly one world position and one
 * selection. Nothing here knows about pixels: the renderer projects, the packer
 * arranges, this file decides *what exists and what it means*.
 */
export interface PlantOverlayItem {
  /** Stable across frames: the canonical overlay anchor id. */
  id: string;
  anchorId: string;
  kind: PlantOverlayAnchorKind;
  /** Groups lower-priority collisions into one truthful `+N` chip. */
  groupId: string;
  entityId: string;
  world: PlantWorldPoint;
  tier: PlantLabelTier;
  selection: FactorySelection;
  selected: boolean;
  focused: boolean;
  /** Selected, focused or alarmed: must never be occluded or collapsed. */
  critical: boolean;
  ariaLabel: string;
  /** Short on-screen text; omitted for pure geometry handles. */
  label?: string;
  /** Screen-space size of the hit target, before touch expansion. */
  hit: { width: number; height: number };
  /** Screen-space size of the label chip, when the item carries one. */
  labelSize?: { width: number; height: number };
  data: PlantOverlayItemData;
}

function overlayRisk(level: PlantRiskLevel): PlantOverlayRisk {
  return {
    label: plantRiskLevelLabel(level),
    level,
    shape: plantRiskMarkerShape(level),
  };
}

export type PlantOverlayItemData =
  | { kind: "bay"; laneId: string; status: string; blocked: boolean }
  | {
      kind: "station";
      stationId: string;
      stageKind: FactoryStation["kind"];
      status: string;
      alarm?: string;
      risk?: PlantOverlayRisk;
    }
  | {
      kind: "carrier";
      runId: string;
      state: string;
      moved: boolean;
      risk?: PlantOverlayRisk;
    }
  | { kind: "worker"; workerId: string; active: boolean; working: boolean }
  | {
      kind: "overflow";
      overflow: "queued" | "runs" | "staff" | "ready";
      count: number;
    }
  | { kind: "overview" };

export interface PlantOverlayRisk {
  level: PlantRiskLevel;
  label: string;
  shape: PlantRiskMarkerShape;
}

export interface PlantOverlayInput {
  animateTransitions: boolean;
  focusId?: string;
  layout: FactoryPlantLayout;
  lens: FactoryLens;
  model: FactoryFloorModel;
  selection: FactorySelection;
}

const MACHINE_HIT = 46;
const CARRIER_HIT = PLANT_HIT_TARGET_MIN;
const WORKER_HIT = 34;
const SIGN_HIT = 34;

/**
 * Builds the semantic overlay for one model/layout generation.
 *
 * Ordering is deterministic — canonical anchor order, then tier — so the packer
 * and the probe see the same list on every run.
 */
export function buildPlantOverlayItems(
  input: PlantOverlayInput,
): PlantOverlayItem[] {
  const { layout, model, selection } = input;
  const stationsById = new Map(
    model.stations.map((station) => [station.id, station]),
  );
  const lanesById = new Map(model.lanes.map((lane) => [lane.id, lane]));
  const carriersByRun = new Map(
    model.carriers.map((carrier) => [carrier.runId, carrier]),
  );
  const workersById = new Map(model.workers.map((worker) => [worker.id, worker]));
  const workerLabels = buildWorkerLabels(model.workers);
  const placementsById = new Map(
    model.workers.flatMap((worker) =>
      worker.placements.map((placement) => [placement.id, placement] as const),
    ),
  );
  const workersByStation = new Map<string, FactoryWorker[]>();
  for (const worker of model.workers) {
    for (const stationId of worker.activeStationIds) {
      const bucket = workersByStation.get(stationId) ?? [];
      bucket.push(worker);
      workersByStation.set(stationId, bucket);
    }
  }
  const baysById = new Map(layout.bays.map((bay) => [bay.overlayAnchorId, bay]));

  const items: PlantOverlayItem[] = [];
  for (const anchor of layout.overlayAnchors) {
    const item = buildItem(anchor, {
      ...input,
      baysById,
      carriersByRun,
      lanesById,
      placementsById,
      stationsById,
      workersById,
      workerLabels,
      workersByStation,
    });
    if (item) {
      items.push(item);
    }
  }
  return items;

  function buildItem(
    anchor: PlantOverlayAnchor,
    context: BuildContext,
  ): PlantOverlayItem | undefined {
    switch (anchor.kind) {
      case "bay":
        return buildBayItem(anchor, context);
      case "station":
        return buildStationItem(anchor, context);
      case "carrier":
        return buildCarrierItem(anchor, context);
      case "worker":
        return buildWorkerItem(anchor, context);
      case "overflow":
        return buildOverflowItem(anchor, context);
      case "overview":
        return undefined;
      default:
        return undefined;
    }
  }
}

interface BuildContext extends PlantOverlayInput {
  baysById: Map<string, FactoryPlantLayout["bays"][number]>;
  carriersByRun: Map<string, FactoryFloorModel["carriers"][number]>;
  lanesById: Map<string, FactoryFloorModel["lanes"][number]>;
  placementsById: Map<string, FactoryWorkerPlacement>;
  stationsById: Map<string, FactoryStation>;
  workersById: Map<string, FactoryWorker>;
  workerLabels: Map<string, string>;
  workersByStation: Map<string, FactoryWorker[]>;
}

function buildBayItem(
  anchor: PlantOverlayAnchor,
  context: BuildContext,
): PlantOverlayItem | undefined {
  const bay = context.baysById.get(anchor.id);
  const lane = context.lanesById.get(anchor.entityId);
  if (!bay || !lane) {
    return undefined;
  }
  const target: FactorySelection = { kind: "lane", id: lane.id };
  const selected = isAnchorSelected(context.selection, target);
  const focused = context.focusId === anchor.id;
  const alarm = lane.blockedRuns > 0;
  // Identity, not display name alone: two gaggles may both call a workflow
  // "Implementation", and the bay sign must still distinguish them at Fit All.
  const label = `${lane.gaggle} · ${lane.workflow}`;
  return {
    anchorId: anchor.id,
    ariaLabel: laneLabel(lane, context.model.runsTruncated),
    critical: selected || focused || alarm,
    data: {
      blocked: alarm,
      kind: "bay",
      laneId: lane.id,
      status: bay.summary.status,
    },
    entityId: lane.id,
    focused,
    groupId: `bay:${lane.id}`,
    hit: { height: SIGN_HIT, width: Math.max(SIGN_HIT, MACHINE_HIT) },
    id: anchor.id,
    kind: "bay",
    label,
    labelSize: {
      height: PLANT_LABEL_HEIGHT,
      width: estimatePlantLabelWidth(label),
    },
    selected,
    selection: target,
    tier: resolveTier(alarm ? "alarm" : "sign", selected, focused),
    world: anchor.position,
  };
}

function buildStationItem(
  anchor: PlantOverlayAnchor,
  context: BuildContext,
): PlantOverlayItem | undefined {
  const station = context.stationsById.get(anchor.entityId);
  if (!station) {
    return undefined;
  }
  const target: FactorySelection = { kind: "station", id: station.id };
  const selected = isAnchorSelected(context.selection, target);
  const focused = context.focusId === anchor.id;
  const workers = context.workersByStation.get(station.id) ?? [];
  const verdict = assessStationRisk(station);
  const risk =
    context.lens === "risk" &&
    (verdict.level !== "healthy" || verdict.incomplete)
      ? overlayRisk(verdict.level)
      : undefined;
  const label = station.stageId;
  return {
    anchorId: anchor.id,
    ariaLabel: stationLabel(station, workers),
    critical: selected || focused || station.alarm !== undefined || risk !== undefined,
    data: {
      ...(station.alarm ? { alarm: station.alarm } : {}),
      kind: "station",
      ...(risk ? { risk } : {}),
      stationId: station.id,
      stageKind: station.kind,
      status: station.status,
    },
    entityId: station.id,
    focused,
    groupId: `bay:${station.laneId}`,
    hit: { height: MACHINE_HIT, width: MACHINE_HIT },
    id: anchor.id,
    kind: "station",
    label,
    labelSize: {
      height: risk ? PLANT_LABEL_HEIGHT * 2 + 5 : PLANT_LABEL_HEIGHT,
      width: Math.max(
        estimatePlantLabelWidth(label),
        risk ? estimatePlantLabelWidth(risk.label) : 0,
      ),
    },
    selected,
    selection: target,
    tier: resolveTier(stationTier(station), selected, focused),
    world: anchor.position,
  };
}

function buildCarrierItem(
  anchor: PlantOverlayAnchor,
  context: BuildContext,
): PlantOverlayItem | undefined {
  const carrier = context.carriersByRun.get(anchor.entityId);
  if (!carrier) {
    return undefined;
  }
  const target: FactorySelection = { kind: "run", id: carrier.runId };
  const selected = isAnchorSelected(context.selection, target);
  const focused = context.focusId === anchor.id;
  const attention = carrier.state === "blocked" || carrier.state === "paused";
  const verdict = assessCarrierRisk(carrier);
  const risk =
    context.lens === "risk" &&
    (verdict.level !== "healthy" || verdict.incomplete)
      ? overlayRisk(verdict.level)
      : undefined;
  const label = risk ? `…${carrier.runId.slice(-5)}` : undefined;
  return {
    anchorId: anchor.id,
    ariaLabel: carrierLabel(carrier),
    critical: selected || focused || risk !== undefined,
    data: {
      kind: "carrier",
      moved:
        context.animateTransitions &&
        carrier.transition?.kind === "stage-change",
      ...(risk ? { risk } : {}),
      runId: carrier.runId,
      state: carrier.state,
    },
    entityId: carrier.runId,
    focused,
    groupId: `carrier:${carrier.stageId ?? carrier.laneId}`,
    hit: { height: CARRIER_HIT, width: CARRIER_HIT },
    id: anchor.id,
    kind: "carrier",
    ...(label
      ? {
          label,
          labelSize: {
            height: PLANT_LABEL_HEIGHT * 2 + 5,
            width: Math.max(
              estimatePlantLabelWidth(label),
              estimatePlantLabelWidth(risk?.label ?? ""),
            ),
          },
        }
      : {}),
    selected,
    selection: target,
    tier: resolveTier(
      attention ? "attention" : carrier.state === "unknown" ? "attention" : "active",
      selected,
      focused,
    ),
    world: anchor.position,
  };
}

function buildWorkerItem(
  anchor: PlantOverlayAnchor,
  context: BuildContext,
): PlantOverlayItem | undefined {
  const placement = context.placementsById.get(anchor.entityId);
  const worker = placement
    ? context.workersById.get(placement.workerId)
    : undefined;
  if (!placement || !worker) {
    return undefined;
  }
  const target: FactorySelection = { kind: "worker", id: worker.id };
  const selected = isAnchorSelected(context.selection, target);
  const focused = context.focusId === anchor.id;
  const station = placement.stationId
    ? context.stationsById.get(placement.stationId)
    : undefined;
  const label = context.workerLabels.get(worker.id) ?? worker.displayName;
  return {
    anchorId: anchor.id,
    ariaLabel: workerLabel(worker, placement),
    critical: selected || focused,
    data: {
      active: placement.active,
      kind: "worker",
      workerId: worker.id,
      working: station?.status === "running",
    },
    entityId: placement.id,
    focused,
    groupId: placement.stationId
      ? `station:${placement.stationId}`
      : "commons:ready",
    hit: { height: WORKER_HIT, width: WORKER_HIT },
    id: anchor.id,
    kind: "worker",
    label,
    labelSize: {
      height: PLANT_LABEL_HEIGHT,
      width: estimatePlantLabelWidth(label),
    },
    selected,
    selection: target,
    tier: resolveTier(placement.active ? "active" : "idle", selected, focused),
    world: anchor.position,
  };
}

function buildWorkerLabels(workers: readonly FactoryWorker[]): Map<string, string> {
  const labels = new Map<string, string>();
  const groups = new Map<string, FactoryWorker[]>();
  for (const worker of workers) {
    const base =
      worker.displayName.length > 16
        ? `${worker.displayName.slice(0, 15)}…`
        : worker.displayName;
    const group = groups.get(base) ?? [];
    group.push(worker);
    groups.set(base, group);
  }
  for (const [base, group] of groups) {
    group
      .slice()
      .sort((left, right) => left.id.localeCompare(right.id))
      .forEach((worker, index) => {
        labels.set(worker.id, group.length === 1 ? base : `${base} #${index + 1}`);
      });
  }
  return labels;
}

function buildOverflowItem(
  anchor: PlantOverlayAnchor,
  context: BuildContext,
): PlantOverlayItem | undefined {
  const overflow = anchor.overflow;
  if (!overflow || overflow.count <= 0) {
    return undefined;
  }
  const detail = overflowDetail(overflow.kind, overflow.count, anchor, context);
  if (!detail) {
    return undefined;
  }
  const selected = isAnchorSelected(context.selection, detail.selection);
  const focused = context.focusId === anchor.id;
  return {
    anchorId: anchor.id,
    ariaLabel: detail.ariaLabel,
    critical: selected || focused,
    data: { count: overflow.count, kind: "overflow", overflow: overflow.kind },
    entityId: anchor.entityId,
    focused,
    groupId: detail.groupId,
    hit: {
      height: PLANT_LABEL_HEIGHT + 6,
      width: estimatePlantLabelWidth(detail.label),
    },
    id: anchor.id,
    kind: "overflow",
    label: detail.label,
    labelSize: {
      height: PLANT_LABEL_HEIGHT,
      width: estimatePlantLabelWidth(detail.label),
    },
    selected,
    selection: detail.selection,
    tier: resolveTier("attention", selected, focused),
    world: anchor.position,
  };
}

function overflowDetail(
  kind: "queued" | "runs" | "staff" | "ready",
  count: number,
  anchor: PlantOverlayAnchor,
  context: BuildContext,
):
  | {
      ariaLabel: string;
      groupId: string;
      label: string;
      selection: FactorySelection;
    }
  | undefined {
  if (kind === "ready") {
    return {
      ariaLabel: `${count} additional ready goobers. Select the floor summary.`,
      groupId: "commons:ready",
      label: `+${count} ready`,
      selection: { kind: "overview" },
    };
  }
  if (kind === "queued") {
    const lane = context.lanesById.get(anchor.entityId);
    if (!lane) {
      return undefined;
    }
    return {
      ariaLabel: `${count} additional runs waiting at inbound for ${lane.displayName}. Select the workflow line.`,
      groupId: `bay:${lane.id}`,
      label: `+${count} queued`,
      selection: { kind: "lane", id: lane.id },
    };
  }
  const station = context.stationsById.get(anchor.entityId);
  if (!station) {
    return undefined;
  }
  const selection: FactorySelection = { kind: "station", id: station.id };
  if (kind === "staff") {
    return {
      ariaLabel: `${count} additional goobers at stage ${station.stageId}. Select the stage to inspect staffing.`,
      groupId: `station:${station.id}`,
      label: `+${count} staff`,
      selection,
    };
  }
  return {
    ariaLabel: `${count} additional runs at stage ${station.stageId}. Select the stage to inspect all runs.`,
    groupId: `station:${station.id}`,
    label: `+${count} more`,
    selection,
  };
}

function stationTier(station: FactoryStation): PlantLabelTier {
  if (station.alarm) {
    return "alarm";
  }
  if (
    station.status === "blocked" ||
    station.status === "held" ||
    station.status === "unknown" ||
    station.unknownCount > 0
  ) {
    return "attention";
  }
  if (station.status === "running") {
    return "active";
  }
  return "idle";
}

function resolveTier(
  tier: PlantLabelTier,
  selected: boolean,
  focused: boolean,
): PlantLabelTier {
  if (selected) {
    return "selected";
  }
  if (focused) {
    return "focused";
  }
  return tier;
}

/**
 * Whether an anchor is the specific thing the operator selected.
 *
 * `isSelected` treats the overview as matching everything, which is the right
 * answer for the inspector — the overview really does describe every anchor.
 * It is the wrong answer here: if every anchor claimed the selected tier the
 * collision packer would have no priority order left to work with, and a ring
 * around all 400 anchors tells nobody anything. The overview therefore means
 * "no anchor in particular".
 */
function isAnchorSelected(
  selection: FactorySelection,
  target: FactorySelection,
): boolean {
  if (selection.kind === "overview") {
    return false;
  }
  return isSelected(selection, target);
}

/** The anchor id a selection should keep visible, if the plant draws one. */
export function findPlantOverlayAnchorId(
  items: readonly PlantOverlayItem[],
  selection: FactorySelection,
): string | undefined {
  if (selection.kind === "overview") {
    return undefined;
  }
  return items.find((item) => isSelected(selection, item.selection))?.id;
}
