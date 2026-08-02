import type {
  FactoryCarrier,
  FactoryFloorModel,
  FactoryLane,
  FactoryStation,
  FactoryWorkerPlacement,
} from "./factoryModel";

export const CLASSIC_PLANT_WIDTH = 1450;
export const CLASSIC_PLANT_HEIGHT = 950;

export interface ClassicPoint {
  x: number;
  y: number;
}

export interface ClassicLanePlacement {
  lane: FactoryLane;
  sign: ClassicPoint;
  inbound: ClassicPoint;
}

export interface ClassicStationPlacement {
  station: FactoryStation;
  machine: ClassicPoint;
  callout: ClassicPoint;
}

export interface ClassicCarrierPlacement {
  carrier: FactoryCarrier;
  point: ClassicPoint;
}

export interface ClassicWorkerPlacement {
  placement: FactoryWorkerPlacement;
  workerId: string;
  point: ClassicPoint;
}

export interface ClassicBeltPlacement {
  id: string;
  active: boolean;
  kind: string;
  outcome?: string;
  from: ClassicPoint;
  to: ClassicPoint;
}

export interface ClassicPlantScene {
  lanes: ClassicLanePlacement[];
  stations: ClassicStationPlacement[];
  carriers: ClassicCarrierPlacement[];
  workers: ClassicWorkerPlacement[];
  belts: ClassicBeltPlacement[];
}

const MACHINE_ANCHORS: readonly ClassicPoint[] = [
  { x: 190, y: 430 },
  { x: 420, y: 350 },
  { x: 570, y: 455 },
  { x: 745, y: 430 },
  { x: 965, y: 430 },
  { x: 1160, y: 555 },
];

const CALLOUT_ANCHORS: readonly ClassicPoint[] = [
  { x: 74, y: 690 },
  { x: 70, y: 350 },
  { x: 450, y: 790 },
  { x: 665, y: 805 },
  { x: 1115, y: 300 },
  { x: 1135, y: 735 },
];

const LANE_SHIFTS: readonly ClassicPoint[] = [
  { x: 0, y: 0 },
  { x: 30, y: 26 },
  { x: -30, y: 48 },
  { x: 55, y: -30 },
  { x: -55, y: -26 },
  { x: 78, y: 52 },
  { x: -78, y: 70 },
];

const SIGN_ANCHORS: readonly ClassicPoint[] = [
  { x: 28, y: 18 },
  { x: 260, y: 18 },
  { x: 492, y: 18 },
  { x: 724, y: 18 },
  { x: 956, y: 18 },
  { x: 1188, y: 18 },
];

export function buildClassicPlant(model: FactoryFloorModel): ClassicPlantScene {
  const stationPoints = new Map<string, ClassicPoint>();
  const stations: ClassicStationPlacement[] = [];

  model.lanes.forEach((lane, laneIndex) => {
    const shift = laneShift(laneIndex);
    lane.stations.forEach((station, stageIndex) => {
      const anchorIndex = spreadIndex(stageIndex, lane.stations.length, MACHINE_ANCHORS.length);
      const calloutIndex = spreadIndex(stageIndex, lane.stations.length, CALLOUT_ANCHORS.length);
      const machine = offset(MACHINE_ANCHORS[anchorIndex], shift);
      const callout = offset(CALLOUT_ANCHORS[calloutIndex], {
        x: shift.x * 0.7,
        y: shift.y * 0.7,
      });
      stationPoints.set(station.id, machine);
      stations.push({ station, machine, callout });
    });
  });

  const lanes = model.lanes.map((lane, index) => ({
    lane,
    sign: laneSign(index),
    inbound: offset(MACHINE_ANCHORS[0], laneShift(index)),
  }));

  const carriers = model.carriers
    .filter((carrier) => carrier.rendered)
    .map((carrier) => {
      const base =
        stationPoints.get(carrier.stationId) ??
        lanes.find((lane) => lane.lane.id === carrier.laneId)?.inbound ??
        MACHINE_ANCHORS[0];
      const slot = carrier.renderSlot ?? carrier.queueIndex;
      return {
        carrier,
        point: offset(base, {
          x: -30 + (slot % 3) * 30,
          y: 18 + Math.floor(slot / 3) * 24,
        }),
      };
    });

  const workers = model.workers.flatMap((worker) =>
    worker.placements
      .filter((placement) => placement.rendered)
      .map((placement, index) => {
        const base = placement.stationId
          ? stationPoints.get(placement.stationId) ?? MACHINE_ANCHORS[2]
          : { x: 815, y: 690 };
        return {
          placement,
          workerId: worker.id,
          point: offset(base, {
            x: 42 + (index % 3) * 34,
            y: 8 + Math.floor(index / 3) * 34,
          }),
        };
      }),
  );

  const belts = model.lanes.flatMap((lane) =>
    lane.conveyors.flatMap((conveyor) => {
      const from = stationPoints.get(conveyor.fromStationId);
      const terminalIndex = lane.docks.findIndex((dock) => dock.id === conveyor.toId);
      const to =
        stationPoints.get(conveyor.toId) ??
        (terminalIndex >= 0
          ? {
              x: MACHINE_ANCHORS[MACHINE_ANCHORS.length - 1].x + terminalIndex * 72,
              y: MACHINE_ANCHORS[MACHINE_ANCHORS.length - 1].y + terminalIndex * 42,
            }
          : undefined);
      if (!from || !to) {
        return [];
      }
      return [{
        id: conveyor.id,
        active: conveyor.active,
        kind: conveyor.kind,
        outcome: conveyor.outcome,
        from,
        to,
      }];
    }),
  );

  return { lanes, stations, carriers, workers, belts };
}

function laneShift(index: number): ClassicPoint {
  const base = LANE_SHIFTS[index % LANE_SHIFTS.length];
  const bank = Math.floor(index / LANE_SHIFTS.length);
  return { x: base.x + bank * 16, y: base.y + bank * 18 };
}

function laneSign(index: number): ClassicPoint {
  const base = SIGN_ANCHORS[index % SIGN_ANCHORS.length];
  const row = Math.floor(index / SIGN_ANCHORS.length);
  return { x: base.x, y: base.y + row * 66 };
}

function spreadIndex(index: number, count: number, anchorCount: number): number {
  if (count <= 1) {
    return Math.floor((anchorCount - 1) / 2);
  }
  return Math.round((index * (anchorCount - 1)) / (count - 1));
}

function offset(point: ClassicPoint, delta: ClassicPoint): ClassicPoint {
  return { x: point.x + delta.x, y: point.y + delta.y };
}
