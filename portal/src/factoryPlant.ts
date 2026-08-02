import type { GraphNodeKind, GraphTerminal } from "./api/types";
import type {
  FactoryAlarmKind,
  FactoryCarrier,
  FactoryConveyorKind,
  FactoryFloorModel,
  FactoryLane,
  FactoryStation,
  FactoryStationStatus,
  FactoryTopologySource,
} from "./factoryModel";

/**
 * The isometric plant projection.
 *
 * This module turns the Factory Floor model into plant geometry and nothing
 * else. It reads the model's own grid facts (which lane a stage belongs to, its
 * declared column and row, which runs stand on it, which goober owns it, which
 * belt joins which pair of stages) and places them on a tile grid, then
 * projects that grid to screen space.
 *
 * Three properties are load-bearing and are asserted in `factoryPlant.test.ts`:
 *
 *   1. Purity. No `Math.random`, no `Date`, no module state. The same model
 *      always produces byte-identical geometry, so a refresh that changes
 *      nothing moves nothing and a crate only slides when its run really
 *      changed stage.
 *   2. Derivation. Every district, machine, belt, crate and worker exists
 *      because the model has that entity. There is no decorative traffic and no
 *      invented stage, edge or owner.
 *   3. Identity. Machines, crates, workers and districts carry the model's own
 *      IDs, so both layouts select exactly the same entities.
 *
 * Privacy: this module only handles identifiers and geometry that the model
 * already exposes to the portal. It never touches journal text, error
 * messages, artifacts, repository refs or trigger refs.
 */

/** Tile footprints, in tiles. The hall grid is measured in these. */
const HALL_MARGIN = 1;
const COMMONS_TILES_X = 2;
const COMMONS_GAP = 1;
/** Where every production line starts, past the commons alcove and main aisle. */
const LINE_X0 = HALL_MARGIN + COMMONS_TILES_X + COMMONS_GAP;
const YARD_TILES_X = 2;
const YARD_TILES_Y = 2;
const AISLE_TILES = 1;
const MACHINE_TILES = 2;
const COLUMN_SPAN = 4;
const ROW_SPAN = 3;
const DOCK_TILES_X = 2;
const DOCK_TILES_Y = 2;
const DOCK_SPAN = 3;
const RETURN_TRACK_TILES = 0.72;
const DISTRICT_GUTTER = 2;
const COMMONS_COLUMNS = 2;
const COMMONS_ROW_PITCH = 0.72;

/** Heights and object sizes, in tile widths, so proportions survive resizing. */
const LIFT_PLINTH = 0.09;
const LIFT_ROOF = 0.07;
const LIFT_BODY: Record<GraphNodeKind, number> = {
  deterministic: 0.5,
  agentic: 0.72,
  gate: 0.4,
};
const LIFT_BELT = 0.13;
const LIFT_DOCK = 0.26;
const LIFT_WALL = 1.45;
const LIFT_RAIL = 0.62;
const CRATE_SIZE = 0.44;
const CRATE_LIFT = 0.34;
const STAFF_SIZE = 0.34;
const STAFF_LIFT = 0.56;

const CRATE_COLUMNS = 3;
const YARD_CRATE_COLUMNS = 2;

/**
 * Scene sizing. A fixed tile keeps every existing district at the same scale
 * and coordinates when another workflow or a later topology read appears.
 * Large plants scroll instead of shrinking under the operator.
 */
export const FACTORY_PLANT_TILE = 56;
const SCENE_PADDING = 18;
const SIGN_APRON = 150;

export interface PlantPoint {
  x: number;
  y: number;
}

export type PlantSolidRole =
  | "plinth"
  | "body"
  | "roof"
  | "stack"
  | "panel"
  | "column"
  | "arm"
  | "post"
  | "boom"
  | "pad";

/** One box of a machine, as its three visible faces in local coordinates. */
export interface PlantSolid {
  role: PlantSolidRole;
  top: string;
  right: string;
  front: string;
}

export interface PlantMachine {
  /** The model's station ID. Selection uses this unchanged. */
  id: string;
  /** The district this machine stands in, which is the model's lane ID. */
  districtId: string;
  stageId: string;
  kind: GraphNodeKind;
  status: FactoryStationStatus;
  source: FactoryTopologySource;
  alarm?: FactoryAlarmKind;
  isStart: boolean;
  wip: number;
  /** Painter order. Larger means nearer the viewer. */
  depth: number;
  left: number;
  top: number;
  width: number;
  height: number;
  solids: PlantSolid[];
  /**
   * Hit area, in local coordinates: the machine prism plus its placard. An
   * isometric scene overlaps rectangles constantly, so the clip is what stops
   * a near machine from swallowing clicks meant for the one behind it.
   */
  clip: string;
  /** Ground-layer geometry, in scene coordinates. */
  shadow: string;
  wash: string;
  /** Local anchors for the status lamp, alarm beacon and placard. */
  lamp: PlantPoint;
  beacon: PlantPoint;
  beaconSize: number;
  placard: { x: number; y: number; width: number; height: number };
  /** Scene anchor for apron controls such as the run overflow chip. */
  apron: PlantPoint;
}

export interface PlantCrate {
  /** The model's run ID. */
  id: string;
  /** The district this run stands in, which is the model's lane ID. */
  laneId: string;
  /** Station or inbound yard ID this run stands on. */
  holderId: string;
  depth: number;
  x: number;
  y: number;
  size: number;
  top: string;
  right: string;
  front: string;
  shadow: string;
  /** Hit area in local coordinates, so stacked crates stay separately clickable. */
  clip: string;
  moved: boolean;
}

export interface PlantStaff {
  /** Placement ID from the model, unique per goober and post. */
  id: string;
  workerId: string;
  stationId?: string;
  districtId?: string;
  active: boolean;
  depth: number;
  x: number;
  y: number;
  size: number;
}

export interface PlantTrack {
  /** The model's conveyor ID. */
  id: string;
  districtId: string;
  kind: FactoryConveyorKind;
  outcome?: string;
  terminal?: GraphTerminal;
  active: boolean;
  /** Projected centreline, retained for label and geometry verification. */
  points: PlantPoint[];
  bed: string;
  rails: string[];
  label?: PlantLabel;
}

export interface PlantLabel extends PlantPoint {
  text: string;
  width: number;
  height: number;
}

export interface PlantDock {
  /** The model's dock ID. */
  id: string;
  districtId: string;
  terminal: GraphTerminal;
  depth: number;
  left: number;
  top: number;
  width: number;
  height: number;
  solids: PlantSolid[];
  shadow: string;
  label: PlantPoint;
}

export interface PlantYard {
  id: string;
  pad: string;
  slots: string[];
  /** Operational label rendered in the upper annotation layer. */
  label: PlantLabel;
  overflowAnchor: PlantPoint;
}

export interface PlantDistrict {
  /** The model's lane ID. */
  id: string;
  index: number;
  source: FactoryTopologySource;
  plot: string;
  kerb: string;
  hazard: string;
  aisle: string;
  /** Painted floor identity from the safe gaggle and workflow display names. */
  floorLabel: { text: string; transform: string };
  sign: PlantPoint;
  yard: PlantYard;
  returnTracks: string[];
  machineIds: string[];
  emptyBay?: { pad: string; label: PlantLabel };
}

export interface PlantCommons {
  pad: string;
  label: PlantLabel;
  overflowAnchor: PlantPoint;
}

export interface PlantWall {
  id: string;
  face: string;
  cap: string;
  windows: string[];
}

export interface PlantHall {
  floor: string;
  grid: string[];
  walls: PlantWall[];
  rail: string;
  railPosts: string[];
  /** Painted main walkway down the hall, with its centre marks. */
  aisle: string;
  aisleMarks: string[];
}

export interface PlantScene {
  width: number;
  height: number;
  tile: number;
  tilesX: number;
  tilesY: number;
  hall: PlantHall;
  districts: PlantDistrict[];
  machines: PlantMachine[];
  tracks: PlantTrack[];
  docks: PlantDock[];
  crates: PlantCrate[];
  staff: PlantStaff[];
  commons?: PlantCommons;
}

interface DistrictPlan {
  lane: FactoryLane;
  index: number;
  originY: number;
  depthTiles: number;
  rowCount: number;
  columnCount: number;
  machineX0: number;
  dockX: number;
  endX: number;
  returnTrackYs: number[];
}

type Project = (tileX: number, tileY: number, lift?: number) => PlantPoint;

/** Projects the model onto the plant hall. Pure: same model, same geometry. */
export function buildFactoryPlant(model: FactoryFloorModel): PlantScene {
  const plans = planDistricts(model.lanes);
  const tilesX = Math.max(
    LINE_X0 + YARD_TILES_X + AISLE_TILES + COLUMN_SPAN + DOCK_TILES_X + HALL_MARGIN,
    ...plans.map((plan) => plan.endX + HALL_MARGIN),
  );
  const commonsCount = model.commons.renderedWorkerIds.length;
  const hasCommons = commonsCount > 0;
  const commonsDepth = hasCommons
    ? Math.ceil(commonsCount / COMMONS_COLUMNS) * COMMONS_ROW_PITCH + 1.1
    : 0;
  const districtDepth = plans.reduce((total, plan) => total + plan.depthTiles, 0);
  const tilesY =
    HALL_MARGIN * 2 + Math.max(districtDepth, commonsDepth, ROW_SPAN);

  const tile = FACTORY_PLANT_TILE;
  const half = tile / 2;
  const quarter = tile / 4;
  const originX = SCENE_PADDING + SIGN_APRON;
  const originY = SCENE_PADDING + LIFT_WALL * tile;
  const project: Project = (tileX, tileY, lift = 0) => ({
    x: round(originX + tileX * half),
    y: round(originY + tileX * quarter + tileY * half - lift * tile),
  });

  const machines: PlantMachine[] = [];
  const districts: PlantDistrict[] = [];
  const docks: PlantDock[] = [];
  const tracks: PlantTrack[] = [];

  const machineTiles = new Map<string, { x: number; y: number }>();
  for (const plan of plans) {
    for (const station of plan.lane.stations) {
      machineTiles.set(station.id, {
        x: plan.machineX0 + station.column * COLUMN_SPAN,
        y: plan.originY + station.row * ROW_SPAN,
      });
    }
  }
  const dockTiles = new Map<string, { x: number; y: number }>();
  for (const plan of plans) {
    plan.lane.docks.forEach((dock, index) => {
      dockTiles.set(dock.id, { x: plan.dockX, y: plan.originY + index * DOCK_SPAN });
    });
  }

  for (const plan of plans) {
    districts.push(buildDistrict(plan, project, tile));
    for (const station of plan.lane.stations) {
      const at = machineTiles.get(station.id);
      if (at) {
        machines.push(buildMachine(station, plan, at.x, at.y, project, tile));
      }
    }
    plan.lane.docks.forEach((dock) => {
      const at = dockTiles.get(dock.id);
      if (at) {
        docks.push(buildDock(dock.id, dock.terminal, plan, at.x, at.y, project, tile));
      }
    });
    const repassIndexes = new Map(
      plan.lane.conveyors
        .filter((conveyor) => conveyor.kind === "repass")
        .map((conveyor, index) => [conveyor.id, index]),
    );
    const outcomeSiblings = new Map<string, typeof plan.lane.conveyors>();
    for (const conveyor of plan.lane.conveyors) {
      if (!conveyor.outcome) {
        continue;
      }
      const siblings = outcomeSiblings.get(conveyor.fromStationId) ?? [];
      siblings.push(conveyor);
      outcomeSiblings.set(conveyor.fromStationId, siblings);
    }
    for (const conveyor of plan.lane.conveyors) {
      const siblings = conveyor.outcome
        ? outcomeSiblings.get(conveyor.fromStationId) ?? []
        : [];
      const track = buildTrack(
        conveyor,
        plan,
        machineTiles,
        dockTiles,
        project,
        repassIndexes.get(conveyor.id),
        siblings.indexOf(conveyor),
        siblings.length,
      );
      if (track) {
        tracks.push(track);
      }
    }
  }

  const crates = buildCrates(model, plans, machineTiles, project);
  // The commons alcove sits at the foot of the aisle, clear of the line signage
  // that hangs at the head of every district.
  const commonsTop =
    HALL_MARGIN + Math.max(0, Math.max(districtDepth, commonsDepth) - commonsDepth);
  const staff = buildStaff(model, plans, machineTiles, project, tile, commonsTop);
  const commons = hasCommons
    ? {
        pad: polygon([
          project(HALL_MARGIN, commonsTop),
          project(HALL_MARGIN + COMMONS_TILES_X, commonsTop),
          project(HALL_MARGIN + COMMONS_TILES_X, commonsTop + commonsDepth),
          project(HALL_MARGIN, commonsTop + commonsDepth),
        ]),
        label: annotationLabel(
          project(HALL_MARGIN + COMMONS_TILES_X / 2, commonsTop + commonsDepth - 0.18),
          "Ready commons",
        ),
        overflowAnchor: project(
          HALL_MARGIN + COMMONS_TILES_X / 2,
          commonsTop + commonsDepth - 0.58,
        ),
      }
    : undefined;

  return {
    width: round(SCENE_PADDING * 2 + SIGN_APRON + tilesX * half),
    height: round(
      SCENE_PADDING * 2 + LIFT_WALL * tile + tilesX * quarter + tilesY * half,
    ),
    tile,
    tilesX,
    tilesY,
    hall: buildHall(tilesX, tilesY, project),
    districts,
    machines,
    tracks,
    docks,
    crates,
    staff,
    commons,
  };
}

function planDistricts(lanes: readonly FactoryLane[]): DistrictPlan[] {
  const plans: DistrictPlan[] = [];
  let cursor = HALL_MARGIN;
  lanes.forEach((lane, index) => {
    const rowCount = Math.max(
      1,
      lane.stations.reduce((tallest, station) => Math.max(tallest, station.row + 1), 0),
    );
    const columnCount = Math.max(
      1,
      lane.stations.reduce((widest, station) => Math.max(widest, station.column + 1), 0),
    );
    const returnCount = lane.conveyors.filter(
      (conveyor) => conveyor.kind === "repass",
    ).length;
    const depthTiles =
      rowCount * ROW_SPAN + returnCount * RETURN_TRACK_TILES + DISTRICT_GUTTER;
    const machineX0 = LINE_X0 + YARD_TILES_X + AISLE_TILES;
    const dockX = machineX0 + columnCount * COLUMN_SPAN;
    plans.push({
      lane,
      index,
      originY: cursor,
      depthTiles,
      rowCount,
      columnCount,
      machineX0,
      dockX,
      endX: (lane.docks.length > 0 ? dockX + DOCK_TILES_X : dockX) + 1,
      returnTrackYs: Array.from(
        { length: returnCount },
        (_, returnIndex) =>
          cursor + rowCount * ROW_SPAN + 0.35 + returnIndex * RETURN_TRACK_TILES,
      ),
    });
    cursor += depthTiles;
  });
  return plans;
}

function buildDistrict(
  plan: DistrictPlan,
  project: Project,
  tile: number,
): PlantDistrict {
  const top = plan.originY - 0.4;
  const bottom = plan.originY + plan.depthTiles - 0.55;
  const left = LINE_X0 - 0.4;
  const right = Math.max(plan.endX - 0.5, LINE_X0 + YARD_TILES_X + 2);
  const corners = [
    project(left, top),
    project(right, top),
    project(right, bottom),
    project(left, bottom),
  ];
  const kerbCorners = [
    project(left + 0.12, top + 0.12),
    project(right - 0.12, top + 0.12),
    project(right - 0.12, bottom - 0.12),
    project(left + 0.12, bottom - 0.12),
  ];
  const yardTop = plan.originY;
  const yard: PlantYard = {
    id: plan.lane.yard.id,
    pad: polygon([
      project(LINE_X0, yardTop),
      project(LINE_X0 + YARD_TILES_X, yardTop),
      project(LINE_X0 + YARD_TILES_X, yardTop + YARD_TILES_Y),
      project(LINE_X0, yardTop + YARD_TILES_Y),
    ]),
    slots: [0, 1, 2, 3].map((slot) => {
      const sx = LINE_X0 + 0.2 + (slot % 2) * 0.9;
      const sy = yardTop + 0.2 + Math.floor(slot / 2) * 0.9;
      return polygon([
        project(sx, sy),
        project(sx + 0.7, sy),
        project(sx + 0.7, sy + 0.7),
        project(sx, sy + 0.7),
      ]);
    }),
    label: {
      ...annotationLabel(
        project(LINE_X0 + YARD_TILES_X / 2, yardTop + YARD_TILES_Y - 0.22, LIFT_BELT),
        "Inbound",
      ),
    },
    overflowAnchor: project(
      LINE_X0 + YARD_TILES_X / 2,
      yardTop + YARD_TILES_Y - 0.58,
    ),
  };

  const aisleY = plan.originY + plan.depthTiles - 0.75;
  return {
    id: plan.lane.id,
    index: plan.index,
    source: plan.lane.source,
    plot: polygon(corners),
    kerb: polygon(kerbCorners),
    hazard: polygon([
      project(left, top),
      project(left + 0.22, top),
      project(left + 0.22, bottom),
      project(left, bottom),
    ]),
    aisle: `M ${corners[3].x} ${corners[3].y} L ${corners[2].x} ${corners[2].y}`,
    floorLabel: {
      text: `${plan.lane.gaggleDisplayName} · ${plan.lane.displayName}`,
      transform: floorTextTransform(project(LINE_X0 + 0.1, aisleY), tile),
    },
    sign: project(LINE_X0 - 0.5, plan.originY - 0.35),
    yard,
    returnTracks: plan.returnTrackYs.map(
      (returnTrackY) =>
        `M ${project(plan.machineX0 - 0.4, returnTrackY).x} ${
          project(plan.machineX0 - 0.4, returnTrackY).y
        } L ${project(plan.dockX - 0.6, returnTrackY).x} ${
          project(plan.dockX - 0.6, returnTrackY).y
        }`,
    ),
    machineIds: plan.lane.stations.map((station) => station.id),
    emptyBay:
      plan.lane.stations.length === 0
        ? {
            pad: polygon([
              project(plan.machineX0, plan.originY),
              project(plan.machineX0 + MACHINE_TILES, plan.originY),
              project(plan.machineX0 + MACHINE_TILES, plan.originY + MACHINE_TILES),
              project(plan.machineX0, plan.originY + MACHINE_TILES),
            ]),
            label: annotationLabel(
              project(plan.machineX0 + 1, plan.originY + 1, LIFT_BELT),
              plan.lane.stageCount > 0
                ? `${plan.lane.stageCount} stages unread`
                : "Topology unread",
            ),
          }
        : undefined,
  };
}

function buildMachine(
  station: FactoryStation,
  plan: DistrictPlan,
  mx: number,
  my: number,
  project: Project,
  tile: number,
): PlantMachine {
  const height = LIFT_BODY[station.kind];
  const bodyBase = LIFT_PLINTH;
  const roofBase = bodyBase + height;
  const parts: { role: PlantSolidRole; quads: PlantPoint[][] }[] = [
    { role: "plinth", quads: boxQuads(project, mx, my, 2, 2, 0, LIFT_PLINTH) },
    {
      role: "body",
      quads: boxQuads(project, mx + 0.18, my + 0.18, 1.64, 1.64, bodyBase, height),
    },
    {
      role: "roof",
      quads: boxQuads(project, mx + 0.08, my + 0.08, 1.84, 1.84, roofBase, LIFT_ROOF),
    },
  ];

  if (station.kind === "deterministic") {
    parts.push({
      role: "stack",
      quads: boxQuads(project, mx + 0.34, my + 0.34, 0.34, 0.34, roofBase + LIFT_ROOF, 0.3),
    });
    parts.push({
      role: "panel",
      quads: boxQuads(project, mx + 1.5, my + 0.5, 0.24, 0.7, bodyBase, height * 0.62),
    });
  } else if (station.kind === "agentic") {
    parts.push({
      role: "column",
      quads: boxQuads(
        project,
        mx + 0.62,
        my + 0.5,
        0.4,
        0.4,
        roofBase + LIFT_ROOF,
        0.42,
      ),
    });
    parts.push({
      role: "arm",
      quads: boxQuads(
        project,
        mx + 0.7,
        my + 0.86,
        0.24,
        0.86,
        roofBase + LIFT_ROOF + 0.24,
        0.14,
      ),
    });
    parts.push({
      role: "panel",
      quads: boxQuads(project, mx + 1.52, my + 0.42, 0.22, 0.86, bodyBase, height * 0.5),
    });
  } else {
    parts.push({
      role: "post",
      quads: boxQuads(project, mx + 0.06, my + 1.62, 0.24, 0.3, bodyBase, height + 0.42),
    });
    parts.push({
      role: "post",
      quads: boxQuads(project, mx + 1.7, my + 1.62, 0.24, 0.3, bodyBase, height + 0.42),
    });
    parts.push({
      role: "boom",
      quads: boxQuads(project, mx + 0.06, my + 1.7, 1.88, 0.14, bodyBase + height + 0.3, 0.1),
    });
  }

  const shadowQuad = [
    project(mx - 0.12, my - 0.12),
    project(mx + 2.12, my - 0.12),
    project(mx + 2.12, my + 2.12),
    project(mx - 0.12, my + 2.12),
  ];
  const washQuad = [
    project(mx - 0.55, my - 0.55),
    project(mx + 2.55, my - 0.55),
    project(mx + 2.55, my + 2.55),
    project(mx - 0.55, my + 2.55),
  ];
  const tallest = parts.reduce(
    (peak, part) => Math.min(peak, Math.min(...part.quads[0].map((point) => point.y))),
    Number.POSITIVE_INFINITY,
  );
  const outline = boxSilhouette(project, mx, my, 2, 2, 0, roofBase + LIFT_ROOF);
  const lamp = project(mx + 0.34, my + 1.82, bodyBase + height * 0.6);
  const beacon = project(mx + 1.72, my + 1.72, roofBase + LIFT_ROOF);
  const beaconSize = round(tile * 0.3);
  const beaconRect = [
    { x: round(beacon.x - beaconSize * 0.6), y: round(beacon.y - beaconSize * 1.3) },
    { x: round(beacon.x + beaconSize * 0.6), y: round(beacon.y - beaconSize * 1.3) },
    { x: round(beacon.x + beaconSize * 0.6), y: round(beacon.y + beaconSize * 0.3) },
    { x: round(beacon.x - beaconSize * 0.6), y: round(beacon.y + beaconSize * 0.3) },
  ];
  // The placard hangs over the machine it belongs to: the apron in front of it
  // is where the runs standing at this stage are parked.
  const placardAt = project(mx + 1, my + 1, roofBase + LIFT_ROOF + 0.04);
  const placardWidth = round(tile * 2);
  const placardHeight = round(tile * 0.72);
  const placardRect = [
    { x: round(placardAt.x - placardWidth / 2), y: round(placardAt.y - placardHeight) },
    { x: round(placardAt.x + placardWidth / 2), y: round(placardAt.y - placardHeight) },
    { x: round(placardAt.x + placardWidth / 2), y: round(placardAt.y) },
    { x: round(placardAt.x - placardWidth / 2), y: round(placardAt.y) },
  ];

  const bounds = boundsOf(
    [
      ...parts.flatMap((part) => part.quads.flat()),
      ...outline,
      ...placardRect,
      ...beaconRect,
      { x: lamp.x, y: round(Math.min(lamp.y, tallest)) },
    ],
    tile * 0.06,
  );
  const shift = (point: PlantPoint) => ({
    x: round(point.x - bounds.left),
    y: round(point.y - bounds.top),
  });

  return {
    id: station.id,
    districtId: plan.lane.id,
    stageId: station.stageId,
    kind: station.kind,
    status: station.status,
    source: station.source,
    alarm: station.alarm,
    isStart: station.isStart,
    wip: station.wip,
    depth: mx + my + MACHINE_TILES,
    left: bounds.left,
    top: bounds.top,
    width: bounds.width,
    height: bounds.height,
    solids: parts.map((part) => ({
      role: part.role,
      top: polygon(part.quads[0].map(shift)),
      right: polygon(part.quads[1].map(shift)),
      front: polygon(part.quads[2].map(shift)),
    })),
    clip: `${closedPath(outline.map(shift))} ${closedPath(beaconRect.map(shift))} ${closedPath(placardRect.map(shift))}`,
    shadow: polygon(shadowQuad),
    wash: polygon(washQuad),
    lamp: shift(lamp),
    beacon: shift(beacon),
    beaconSize,
    placard: {
      x: round(placardAt.x - bounds.left - placardWidth / 2),
      y: round(placardAt.y - bounds.top - placardHeight),
      width: placardWidth,
      height: placardHeight,
    },
    apron: project(mx + 0.35, my + 2.48),
  };
}

function buildDock(
  id: string,
  terminal: GraphTerminal,
  plan: DistrictPlan,
  dx: number,
  dy: number,
  project: Project,
  tile: number,
): PlantDock {
  const parts: { role: PlantSolidRole; quads: PlantPoint[][] }[] = [
    { role: "pad", quads: boxQuads(project, dx, dy, DOCK_TILES_X, DOCK_TILES_Y, 0, 0.06) },
    {
      role: "body",
      quads: boxQuads(project, dx + 0.2, dy + 0.2, 1.6, 1.6, 0.06, LIFT_DOCK),
    },
    {
      role: "roof",
      quads: boxQuads(project, dx + 0.1, dy + 0.1, 1.8, 1.8, 0.06 + LIFT_DOCK, 0.05),
    },
  ];
  const shadowQuad = [
    project(dx - 0.1, dy - 0.1),
    project(dx + DOCK_TILES_X + 0.1, dy - 0.1),
    project(dx + DOCK_TILES_X + 0.1, dy + DOCK_TILES_Y + 0.1),
    project(dx - 0.1, dy + DOCK_TILES_Y + 0.1),
  ];
  const label = project(dx + 1, dy + 1, 0.06 + LIFT_DOCK + 0.05);
  const bounds = boundsOf(
    [...parts.flatMap((part) => part.quads.flat()), label],
    tile * 0.08,
  );
  const shift = (point: PlantPoint) => ({
    x: round(point.x - bounds.left),
    y: round(point.y - bounds.top),
  });
  return {
    id,
    districtId: plan.lane.id,
    terminal,
    depth: dx + dy + DOCK_TILES_X,
    left: bounds.left,
    top: bounds.top,
    width: bounds.width,
    height: bounds.height,
    solids: parts.map((part) => ({
      role: part.role,
      top: polygon(part.quads[0].map(shift)),
      right: polygon(part.quads[1].map(shift)),
      front: polygon(part.quads[2].map(shift)),
    })),
    shadow: polygon(shadowQuad),
    label: shift(label),
  };
}

function buildTrack(
  conveyor: FactoryLane["conveyors"][number],
  plan: DistrictPlan,
  machineTiles: ReadonlyMap<string, { x: number; y: number }>,
  dockTiles: ReadonlyMap<string, { x: number; y: number }>,
  project: Project,
  repassIndex: number | undefined,
  outcomeIndex: number,
  outcomeCount: number,
): PlantTrack | undefined {
  const from = machineTiles.get(conveyor.fromStationId);
  if (!from) {
    return undefined;
  }
  const to = machineTiles.get(conveyor.toId) ?? dockTiles.get(conveyor.toId);
  if (!to) {
    return undefined;
  }

  const startY = from.y + 1;
  const endY = to.y + 1;
  let route: { x: number; y: number }[];
  if (conveyor.kind === "repass") {
    const trackY =
      plan.returnTrackYs[repassIndex ?? 0] ??
      plan.originY + plan.rowCount * ROW_SPAN + 0.35;
    route = [
      { x: from.x + 1, y: from.y + MACHINE_TILES },
      { x: from.x + 1, y: trackY },
      { x: to.x + 1, y: trackY },
      { x: to.x + 1, y: to.y + MACHINE_TILES },
    ];
  } else if (Math.abs(startY - endY) < 0.001) {
    route = [
      { x: from.x + MACHINE_TILES, y: startY },
      { x: to.x, y: endY },
    ];
  } else {
    const mid = (from.x + MACHINE_TILES + to.x) / 2;
    route = [
      { x: from.x + MACHINE_TILES, y: startY },
      { x: mid, y: startY },
      { x: mid, y: endY },
      { x: to.x, y: endY },
    ];
  }
  const centre = route.map((step) => project(step.x, step.y, LIFT_BELT));
  return {
    id: conveyor.id,
    districtId: plan.lane.id,
    kind: conveyor.kind,
    outcome: conveyor.outcome,
    terminal: conveyor.terminal,
    active: conveyor.active,
    points: centre,
    bed: path(centre),
    rails: [
      path(route.map((step) => project(step.x, step.y - 0.16, LIFT_BELT))),
      path(route.map((step) => project(step.x, step.y + 0.16, LIFT_BELT))),
    ],
    label: conveyor.outcome
      ? outcomeLabel(centre, conveyor.outcome, outcomeIndex, outcomeCount)
      : undefined,
  };
}

function outcomeLabel(
  points: readonly PlantPoint[],
  text: string,
  siblingIndex: number,
  siblingCount: number,
): PlantLabel {
  const total = polylineLength(points);
  const fromTarget = Math.min(46, total * 0.28);
  const sample = pointAtPolylineDistance(points, Math.max(0, total - fromTarget));
  const centred = siblingIndex - (siblingCount - 1) / 2;
  const semanticSide = negativeOutcome(text) ? 1 : -1;
  const side = siblingCount > 1 && centred !== 0 ? Math.sign(centred) : semanticSide;
  const clearance = 12 + Math.abs(centred) * 9;
  const tangentShift = centred * 7;
  return annotationLabel(
    {
      x: round(
        sample.point.x +
          sample.normal.x * clearance * side +
          sample.tangent.x * tangentShift,
      ),
      y: round(
        sample.point.y +
          sample.normal.y * clearance * side +
          sample.tangent.y * tangentShift,
      ),
    },
    text,
  );
}

function negativeOutcome(outcome: string): boolean {
  return /fail|reject|error|escalat|needs|retry|no\b/i.test(outcome);
}

function polylineLength(points: readonly PlantPoint[]): number {
  let total = 0;
  for (let index = 1; index < points.length; index += 1) {
    total += pointDistance(points[index - 1], points[index]);
  }
  return total;
}

function pointAtPolylineDistance(
  points: readonly PlantPoint[],
  distance: number,
): {
  point: PlantPoint;
  tangent: PlantPoint;
  normal: PlantPoint;
} {
  let remaining = distance;
  for (let index = 1; index < points.length; index += 1) {
    const start = points[index - 1];
    const end = points[index];
    const length = pointDistance(start, end);
    if (remaining <= length || index === points.length - 1) {
      const ratio = length === 0 ? 0 : Math.min(1, remaining / length);
      const tangent = {
        x: length === 0 ? 1 : (end.x - start.x) / length,
        y: length === 0 ? 0 : (end.y - start.y) / length,
      };
      return {
        point: {
          x: start.x + (end.x - start.x) * ratio,
          y: start.y + (end.y - start.y) * ratio,
        },
        tangent,
        normal: { x: -tangent.y, y: tangent.x },
      };
    }
    remaining -= length;
  }
  const point = points.at(-1) ?? { x: 0, y: 0 };
  return {
    point,
    tangent: { x: 1, y: 0 },
    normal: { x: 0, y: 1 },
  };
}

function pointDistance(left: PlantPoint, right: PlantPoint): number {
  return Math.hypot(right.x - left.x, right.y - left.y);
}

function annotationLabel(point: PlantPoint, text: string): PlantLabel {
  return {
    ...point,
    text,
    width: Math.max(42, Math.min(150, text.length * 6 + 16)),
    height: 18,
  };
}

function buildCrates(
  model: FactoryFloorModel,
  plans: readonly DistrictPlan[],
  machineTiles: ReadonlyMap<string, { x: number; y: number }>,
  project: Project,
): PlantCrate[] {
  const yardTiles = new Map<string, { x: number; y: number; laneId: string }>();
  for (const plan of plans) {
    yardTiles.set(plan.lane.yard.id, {
      x: LINE_X0,
      y: plan.originY,
      laneId: plan.lane.id,
    });
  }

  const crates: PlantCrate[] = [];
  for (const carrier of model.carriers) {
    if (!carrier.rendered) {
      continue;
    }
    const slot = carrier.renderSlot ?? 0;
    const machine = machineTiles.get(carrier.stationId);
    let tileX: number;
    let tileY: number;
    if (machine) {
      // The apron in front of the machine: work standing at that stage, always
      // nearer the viewer than the building so it is never hidden by it.
      tileX = machine.x + 0.1 + (slot % CRATE_COLUMNS) * 0.62;
      tileY = machine.y + 2.12 + Math.floor(slot / CRATE_COLUMNS) * 0.52;
    } else {
      const yard = yardTiles.get(carrier.stationId);
      if (!yard) {
        continue;
      }
      tileX = yard.x + 0.24 + (slot % YARD_CRATE_COLUMNS) * 0.86;
      tileY = yard.y + 0.24 + Math.floor(slot / YARD_CRATE_COLUMNS) * 0.86;
    }
    crates.push(buildCrate(carrier, tileX, tileY, project));
  }
  return crates;
}

function buildCrate(
  carrier: FactoryCarrier,
  tileX: number,
  tileY: number,
  project: Project,
): PlantCrate {
  const quads = boxQuads(project, tileX, tileY, CRATE_SIZE, CRATE_SIZE, 0, CRATE_LIFT);
  const outline = boxSilhouette(project, tileX, tileY, CRATE_SIZE, CRATE_SIZE, 0, CRATE_LIFT);
  const shadowQuad = [
    project(tileX - 0.04, tileY - 0.04),
    project(tileX + CRATE_SIZE + 0.04, tileY - 0.04),
    project(tileX + CRATE_SIZE + 0.04, tileY + CRATE_SIZE + 0.04),
    project(tileX - 0.04, tileY + CRATE_SIZE + 0.04),
  ];
  const bounds = boundsOf([...quads.flat(), ...shadowQuad], 1);
  const shift = (point: PlantPoint) => ({
    x: round(point.x - bounds.left),
    y: round(point.y - bounds.top),
  });
  return {
    id: carrier.runId,
    laneId: carrier.laneId,
    holderId: carrier.stationId,
    depth: tileX + tileY + CRATE_SIZE,
    x: bounds.left,
    y: bounds.top,
    size: Math.max(bounds.width, bounds.height),
    top: polygon(quads[0].map(shift)),
    right: polygon(quads[1].map(shift)),
    front: polygon(quads[2].map(shift)),
    shadow: polygon(shadowQuad.map(shift)),
    clip: closedPath(outline.map(shift)),
    moved: carrier.transition?.kind === "stage-change",
  };
}

function buildStaff(
  model: FactoryFloorModel,
  plans: readonly DistrictPlan[],
  machineTiles: ReadonlyMap<string, { x: number; y: number }>,
  project: Project,
  tile: number,
  commonsTop: number,
): PlantStaff[] {
  const districtByStation = new Map<string, string>();
  for (const plan of plans) {
    for (const station of plan.lane.stations) {
      districtByStation.set(station.id, plan.lane.id);
    }
  }
  let commonsIndex = 0;
  const staff: PlantStaff[] = [];
  for (const worker of model.workers) {
    for (const placement of worker.placements) {
      if (!placement.rendered) {
        continue;
      }
      let tileX: number;
      let tileY: number;
      if (placement.stationId) {
        const at = machineTiles.get(placement.stationId);
        if (!at) {
          continue;
        }
        tileX = at.x + 2.08;
        tileY = at.y + 1.5;
      } else {
        const column = commonsIndex % COMMONS_COLUMNS;
        const row = Math.floor(commonsIndex / COMMONS_COLUMNS);
        commonsIndex += 1;
        tileX = HALL_MARGIN + 0.5 + column * 0.95;
        tileY = commonsTop + 0.5 + row * COMMONS_ROW_PITCH;
      }
      const head = project(tileX, tileY, STAFF_LIFT);
      staff.push({
        id: placement.id,
        workerId: worker.id,
        stationId: placement.stationId,
        districtId: placement.stationId
          ? districtByStation.get(placement.stationId)
          : undefined,
        active: placement.active,
        depth: tileX + tileY,
        x: round(head.x - (STAFF_SIZE * tile) / 2),
        y: round(head.y - tile * 0.1),
        size: round(STAFF_SIZE * tile),
      });
    }
  }
  return staff;
}

function buildHall(
  tilesX: number,
  tilesY: number,
  project: Project,
): PlantHall {
  const grid: string[] = [];
  for (let x = 0; x <= tilesX; x += 1) {
    grid.push(path([project(x, 0), project(x, tilesY)]));
  }
  for (let y = 0; y <= tilesY; y += 1) {
    grid.push(path([project(0, y), project(tilesX, y)]));
  }

  const windowsFor = (
    span: number,
    at: (start: number, end: number) => PlantPoint[],
  ): string[] => {
    const panes: string[] = [];
    for (let start = 1; start + 1.6 <= span - 0.6; start += 2.6) {
      panes.push(polygon(at(start, start + 1.6)));
    }
    return panes;
  };

  const wallBack: PlantWall = {
    id: "back",
    face: polygon([
      project(0, 0, LIFT_WALL),
      project(tilesX, 0, LIFT_WALL),
      project(tilesX, 0),
      project(0, 0),
    ]),
    cap: polygon([
      project(0, -0.28, LIFT_WALL),
      project(tilesX, -0.28, LIFT_WALL),
      project(tilesX, 0, LIFT_WALL),
      project(0, 0, LIFT_WALL),
    ]),
    windows: windowsFor(tilesX, (start, end) => [
      project(start, 0, LIFT_WALL * 0.86),
      project(end, 0, LIFT_WALL * 0.86),
      project(end, 0, LIFT_WALL * 0.5),
      project(start, 0, LIFT_WALL * 0.5),
    ]),
  };
  const wallSide: PlantWall = {
    id: "side",
    face: polygon([
      project(0, 0, LIFT_WALL),
      project(0, tilesY, LIFT_WALL),
      project(0, tilesY),
      project(0, 0),
    ]),
    cap: polygon([
      project(-0.28, 0, LIFT_WALL),
      project(-0.28, tilesY, LIFT_WALL),
      project(0, tilesY, LIFT_WALL),
      project(0, 0, LIFT_WALL),
    ]),
    windows: windowsFor(tilesY, (start, end) => [
      project(0, start, LIFT_WALL * 0.86),
      project(0, end, LIFT_WALL * 0.86),
      project(0, end, LIFT_WALL * 0.5),
      project(0, start, LIFT_WALL * 0.5),
    ]),
  };

  const railPosts: string[] = [];
  for (let x = 0; x <= tilesX; x += 4) {
    railPosts.push(path([project(x, 0.16, LIFT_RAIL), project(x, 0.16, 0)]));
  }
  for (let y = 4; y <= tilesY; y += 4) {
    railPosts.push(path([project(0.16, y, LIFT_RAIL), project(0.16, y, 0)]));
  }

  const aisleMarks: string[] = [];
  for (let y = 1; y + 0.6 <= tilesY - 0.4; y += 1.4) {
    aisleMarks.push(path([project(LINE_X0 - 0.62, y), project(LINE_X0 - 0.62, y + 0.6)]));
  }

  return {
    floor: polygon([
      project(0, 0),
      project(tilesX, 0),
      project(tilesX, tilesY),
      project(0, tilesY),
    ]),
    grid,
    walls: [wallSide, wallBack],
    rail: path([
      project(0.16, tilesY, LIFT_RAIL),
      project(0.16, 0.16, LIFT_RAIL),
      project(tilesX, 0.16, LIFT_RAIL),
    ]),
    railPosts,
    aisle: polygon([
      project(LINE_X0 - 0.94, 0.6),
      project(LINE_X0 - 0.3, 0.6),
      project(LINE_X0 - 0.3, tilesY - 0.4),
      project(LINE_X0 - 0.94, tilesY - 0.4),
    ]),
    aisleMarks,
  };
}

/** Top, right and front faces of an axis-aligned box on the tile grid. */
function boxQuads(
  project: Project,
  tileX: number,
  tileY: number,
  width: number,
  depth: number,
  base: number,
  height: number,
): PlantPoint[][] {
  const lift = base + height;
  const top = [
    project(tileX, tileY, lift),
    project(tileX + width, tileY, lift),
    project(tileX + width, tileY + depth, lift),
    project(tileX, tileY + depth, lift),
  ];
  const right = [
    project(tileX + width, tileY, lift),
    project(tileX + width, tileY + depth, lift),
    project(tileX + width, tileY + depth, base),
    project(tileX + width, tileY, base),
  ];
  const front = [
    project(tileX, tileY + depth, lift),
    project(tileX + width, tileY + depth, lift),
    project(tileX + width, tileY + depth, base),
    project(tileX, tileY + depth, base),
  ];
  return [top, right, front];
}

/** The outline of an isometric box: a hexagon, used as the click target. */
function boxSilhouette(
  project: Project,
  tileX: number,
  tileY: number,
  width: number,
  depth: number,
  base: number,
  height: number,
): PlantPoint[] {
  const lift = base + height;
  return [
    project(tileX, tileY, lift),
    project(tileX + width, tileY, lift),
    project(tileX + width, tileY, base),
    project(tileX + width, tileY + depth, base),
    project(tileX, tileY + depth, base),
    project(tileX, tileY + depth, lift),
  ];
}

/** Lays flat text along the tile grid so floor paint sits on the floor. */
function floorTextTransform(at: PlantPoint, tile: number): string {
  const half = tile / 2;
  const quarter = tile / 4;
  const length = Math.hypot(half, quarter);
  const along = { x: half / length, y: quarter / length };
  const down = { x: 0, y: 1 };
  return `matrix(${round(along.x)} ${round(along.y)} ${round(down.x)} ${round(down.y)} ${at.x} ${at.y})`;
}

function boundsOf(
  points: readonly PlantPoint[],
  padding: number,
): { left: number; top: number; width: number; height: number } {
  let minX = Number.POSITIVE_INFINITY;
  let minY = Number.POSITIVE_INFINITY;
  let maxX = Number.NEGATIVE_INFINITY;
  let maxY = Number.NEGATIVE_INFINITY;
  for (const point of points) {
    minX = Math.min(minX, point.x);
    minY = Math.min(minY, point.y);
    maxX = Math.max(maxX, point.x);
    maxY = Math.max(maxY, point.y);
  }
  return {
    left: round(minX - padding),
    top: round(minY - padding),
    width: round(maxX - minX + padding * 2),
    height: round(maxY - minY + padding * 2),
  };
}

function polygon(points: readonly PlantPoint[]): string {
  return points.map((point) => `${point.x},${point.y}`).join(" ");
}

function path(points: readonly PlantPoint[]): string {
  return points
    .map((point, index) => `${index === 0 ? "M" : "L"} ${point.x} ${point.y}`)
    .join(" ");
}

function closedPath(points: readonly PlantPoint[]): string {
  return `${path(points)} Z`;
}

function round(value: number): number {
  return Math.round(value * 100) / 100;
}
