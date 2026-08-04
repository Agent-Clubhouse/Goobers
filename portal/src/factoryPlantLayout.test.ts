import { describe, expect, it } from "vitest";

import {
  buildFactoryPlantLayout,
  FACTORY_PLANT_BAY_GRID,
  FACTORY_PLANT_LOD_THRESHOLDS,
  fitFactoryPlantCamera,
  type FactoryPlantLayout,
} from "./factoryPlantLayout";
import type { FactoryFloorModel } from "./factoryModel";
import {
  scalablePlantFixture,
} from "./test/plantFixtures";
import factoryPlantLayoutSource from "./factoryPlantLayout.ts?raw";
import plantEntitySource from "./components/plant/factoryPlantEntities.ts?raw";
import plantSceneGraphSource from "./components/plant/factoryPlantSceneGraph.ts?raw";

function expectCollisionFree(layout: FactoryPlantLayout): void {
  expect(layout.metrics.collisions).toEqual({
    bayCells: 0,
    machines: 0,
    duplicateStationCoordinates: 0,
  });
  expect(layout.metrics.unresolvedTrackIds).toEqual([]);
  expect(layout.metrics.boundsFinite).toBe(true);

  const cells = layout.bays.flatMap((bay) =>
    bay.allocation.cells.map((cell) => `${cell.x},${cell.z}`),
  );
  expect(new Set(cells).size).toBe(cells.length);

  const coordinates = layout.machines.map(
    (machine) =>
      `${machine.transform.position.x},${machine.transform.position.z}`,
  );
  expect(new Set(coordinates).size).toBe(coordinates.length);
}

function expectEveryStationOnce(
  model: FactoryFloorModel,
  layout: FactoryPlantLayout,
): void {
  expect(layout.machines.map((machine) => machine.id).sort()).toEqual(
    model.stations.map((station) => station.id).sort(),
  );
  expect(layout.tracks.map((track) => track.id).sort()).toEqual(
    model.lanes.flatMap((lane) => lane.conveyors.map((track) => track.id)).sort(),
  );
}

function reversedModel(model: FactoryFloorModel): FactoryFloorModel {
  return {
    ...model,
    lanes: [...model.lanes]
      .reverse()
      .map((lane) => ({
        ...lane,
        stations: [...lane.stations].reverse(),
        conveyors: [...lane.conveyors].reverse(),
        docks: [...lane.docks].reverse(),
      })),
    stations: [...model.stations].reverse(),
    carriers: [...model.carriers].reverse(),
    workers: [...model.workers]
      .reverse()
      .map((worker) => ({
        ...worker,
        placements: [...worker.placements].reverse(),
      })),
  };
}

function replaceWorkflow(
  base: FactoryFloorModel,
  replacement: FactoryFloorModel,
  workflowId: string,
): FactoryFloorModel {
  const lane = replacement.lanes.find((candidate) => candidate.id === workflowId);
  if (!lane) {
    throw new Error(`Replacement workflow ${workflowId} is missing`);
  }
  return {
    ...base,
    lanes: base.lanes.map((candidate) =>
      candidate.id === workflowId ? lane : candidate,
    ),
    stations: [
      ...base.stations.filter((station) => station.laneId !== workflowId),
      ...replacement.stations,
    ],
  };
}

describe("renderer-neutral Factory Plant layout", () => {
  it.each([
    [1, 1],
    [6, 6],
    [12, 12],
    [50, 20],
  ])(
    "lays out %i workflows x %i stages without collisions",
    (workflowCount, stagesPerWorkflow) => {
      const model = scalablePlantFixture({
        workflowCount,
        stagesPerWorkflow,
      });
      const layout = buildFactoryPlantLayout(model);

      expect(layout.bays).toHaveLength(workflowCount);
      expect(layout.machines).toHaveLength(
        workflowCount * stagesPerWorkflow,
      );
      expect(layout.tracks).toHaveLength(
        workflowCount * stagesPerWorkflow,
      );
      expectEveryStationOnce(model, layout);
      expectCollisionFree(layout);

      for (const aspect of [16 / 9, 1, 9 / 16]) {
        const fit = fitFactoryPlantCamera(layout, aspect, { x: 2, y: 1.5 });
        expect(Number.isFinite(fit.viewWidth)).toBe(true);
        expect(Number.isFinite(fit.viewHeight)).toBe(true);
        expect(fit.viewWidth).toBeGreaterThanOrEqual(
          layout.projectedBounds.width + 4,
        );
        expect(fit.viewHeight).toBeGreaterThanOrEqual(
          layout.projectedBounds.height + 3,
        );
      }
    },
  );

  it("uses stabilized render slots and keeps crate aprons clear of adjacent machines", () => {
    const fixture = scalablePlantFixture({
      workflowCount: 1,
      stagesPerWorkflow: 2,
      carriersPerWorkflow: 7,
    });
    const [firstStation, secondStation] = fixture.stations;
    if (!firstStation || !secondStation) {
      throw new Error("Expected two fixture stations.");
    }
    const stations = fixture.stations.map((station, index) => ({
      ...station,
      column: 0,
      row: index,
    }));
    const carriers = fixture.carriers.map((carrier, index) => ({
      ...carrier,
      rendered: index > 0,
      renderSlot: index > 0 ? index - 1 : undefined,
    }));
    const model: FactoryFloorModel = {
      ...fixture,
      carriers,
      lanes: fixture.lanes.map((lane) => ({
        ...lane,
        stations,
      })),
      stations,
    };

    const layout = buildFactoryPlantLayout(model);
    const anchors = new Map(layout.carriers.map((carrier) => [carrier.id, carrier]));
    const adjacentMachine = layout.machines.find(
      (machine) => machine.id === secondStation.id,
    );
    expect(adjacentMachine).toBeDefined();
    expect(anchors).toHaveLength(6);
    expect(anchors.has(carriers[0]!.runId)).toBe(false);

    for (const carrier of carriers.slice(1)) {
      const anchor = anchors.get(carrier.runId);
      expect(anchor).toBeDefined();
      const slot = carrier.renderSlot!;
      expect(anchor?.position.x).toBeCloseTo(
        layout.machines[0]!.transform.position.x +
          0.85 +
          (slot % 2) * 0.42,
      );
      const overlapsAdjacentMachine =
        Math.abs(
          anchor!.position.x - adjacentMachine!.transform.position.x,
        ) <= 0.54 &&
        Math.abs(
          anchor!.position.z - adjacentMachine!.transform.position.z,
        ) <= 0.54;
      expect(overlapsAdjacentMachine).toBe(false);
    }
  });

  it("fits a twenty-stage workflow in one standard bay cell", () => {
    const layout = buildFactoryPlantLayout(
      scalablePlantFixture({ workflowCount: 1, stagesPerWorkflow: 20 }),
    );

    expect(
      FACTORY_PLANT_BAY_GRID.stationSlotsX *
        FACTORY_PLANT_BAY_GRID.stationSlotsZ,
    ).toBeGreaterThanOrEqual(20);
    expect(layout.bays[0]?.allocation.span).toEqual({ x: 1, z: 1 });
    expect(layout.bays[0]?.allocation.cells).toHaveLength(1);
    expectCollisionFree(layout);
  });

  it("is byte-deterministic when model arrays are shuffled", () => {
    const model = scalablePlantFixture({
      workflowCount: 12,
      stagesPerWorkflow: 12,
      carriersPerWorkflow: 2,
      workersPerWorkflow: 2,
    });

    const ordered = buildFactoryPlantLayout(model);
    const shuffled = buildFactoryPlantLayout(reversedModel(model));

    expect(JSON.stringify(shuffled)).toBe(JSON.stringify(ordered));
  });

  it("preserves all prior workflow and station positions when a workflow is inserted", () => {
    const workflowIds = Array.from(
      { length: 24 },
      (_, index) => `gaggle/workflow-${String(index + 1).padStart(3, "0")}`,
    );
    const beforeModel = scalablePlantFixture({
      workflowIds,
      stagesPerWorkflow: 8,
    });
    const before = buildFactoryPlantLayout(beforeModel);
    const insertedModel = scalablePlantFixture({
      workflowIds: ["gaggle/workflow-000", ...workflowIds],
      stagesPerWorkflow: 8,
    });
    const after = buildFactoryPlantLayout(insertedModel, { previous: before });
    const beforeMachines = new Map(
      before.machines.map((machine) => [machine.id, machine.transform.position]),
    );

    for (const workflowId of workflowIds) {
      expect(after.allocation.bays[workflowId]?.origin).toEqual(
        before.allocation.bays[workflowId]?.origin,
      );
      expect(after.allocation.bays[workflowId]?.stationSlots).toEqual(
        before.allocation.bays[workflowId]?.stationSlots,
      );
    }
    for (const machine of after.machines) {
      const previous = beforeMachines.get(machine.id);
      if (previous) {
        expect(machine.transform.position).toEqual(previous);
      }
    }
    expectCollisionFree(after);
  });

  it("preserves stable station slots when topology inserts an earlier column", () => {
    const beforeModel = scalablePlantFixture({
      workflowCount: 1,
      stagesPerWorkflow: 5,
    });
    const before = buildFactoryPlantLayout(beforeModel);
    const lane = beforeModel.lanes[0]!;
    const shifted = lane.stations.map((station) => ({
      ...station,
      isStart: false,
      column: station.column + 1,
    }));
    const inserted = {
      ...shifted[0]!,
      id: `${lane.id}/stage-inserted`,
      stageId: "stage-inserted",
      column: 0,
      row: 0,
      isStart: true,
      wip: 0,
      runIds: [],
      renderedRunIds: [],
    };
    const nextLane = {
      ...lane,
      stageCount: lane.stageCount + 1,
      stations: [inserted, ...shifted],
      conveyors: [
        {
          ...lane.conveyors[0]!,
          id: `${lane.id}#edge:inserted`,
          fromStationId: inserted.id,
          toId: shifted[0]!.id,
        },
        ...lane.conveyors,
      ],
    };
    const afterModel = {
      ...beforeModel,
      lanes: [nextLane],
      stations: nextLane.stations,
    };
    const after = buildFactoryPlantLayout(afterModel, { previous: before });
    const beforePositions = new Map(
      before.machines.map((machine) => [machine.id, machine.transform.position]),
    );

    for (const station of lane.stations) {
      const beforeSlot =
        before.allocation.bays[lane.id]?.stationSlots[station.id];
      const afterSlot = after.allocation.bays[lane.id]?.stationSlots[station.id];
      expect(
        afterSlot && { column: afterSlot.column, row: afterSlot.row },
      ).toEqual(
        beforeSlot && { column: beforeSlot.column, row: beforeSlot.row },
      );
      expect(afterSlot?.modelColumn).toBe(station.column + 1);
      expect(
        after.machines.find((machine) => machine.id === station.id)?.transform
          .position,
      ).toEqual(beforePositions.get(station.id));
    }
    expectCollisionFree(after);
  });

  it("relocates only a workflow that cannot grow in place", () => {
    const workflowIds = [
      "gaggle/a",
      "gaggle/b",
      "gaggle/c",
      "gaggle/d",
    ];
    const beforeModel = scalablePlantFixture({
      workflowIds,
      stagesPerWorkflow: 20,
    });
    const before = buildFactoryPlantLayout(beforeModel);
    const grown = scalablePlantFixture({
      workflowIds: ["gaggle/a"],
      stagesPerWorkflow: 21,
    });
    const afterModel = replaceWorkflow(beforeModel, grown, "gaggle/a");
    const after = buildFactoryPlantLayout(afterModel, { previous: before });

    expect(after.allocation.bays["gaggle/a"]?.origin).not.toEqual(
      before.allocation.bays["gaggle/a"]?.origin,
    );
    for (const workflowId of workflowIds.slice(1)) {
      expect(after.allocation.bays[workflowId]?.origin).toEqual(
        before.allocation.bays[workflowId]?.origin,
      );
      const beforePositions = before.machines
        .filter((machine) => machine.workflowId === workflowId)
        .map((machine) => machine.transform.position);
      const afterPositions = after.machines
        .filter((machine) => machine.workflowId === workflowId)
        .map((machine) => machine.transform.position);
      expect(afterPositions).toEqual(beforePositions);
    }
    expectCollisionFree(after);
  });

  it("uses blocked > held > unknown > running > idle aggregate precedence", () => {
    const statuses = [
      "idle",
      "running",
      "unknown",
      "held",
      "blocked",
    ] as const;
    const model = scalablePlantFixture({
      workflowCount: 1,
      stagesPerWorkflow: statuses.length,
      carriersPerWorkflow: 2,
      statusAt: (_workflow, stage) => statuses[stage]!,
      wipAt: (_workflow, stage) => stage + 1,
    });
    const layout = buildFactoryPlantLayout(model);

    expect(layout.aggregate).toMatchObject({
      status: "blocked",
      workflows: 1,
      stages: 5,
      wip: 15,
      carriers: 2,
      statusCounts: {
        blocked: 1,
        held: 1,
        idle: 1,
        impeded: 0,
        running: 1,
        unknown: 1,
      },
    });
    expect(layout.bays[0]?.summary.status).toBe("blocked");
  });

  it("keeps stress metadata and instance batches bounded", () => {
    const statuses = [
      "idle",
      "running",
      "unknown",
      "held",
      "blocked",
      "impeded",
    ] as const;
    const layout = buildFactoryPlantLayout(
      scalablePlantFixture({
        workflowCount: 50,
        stagesPerWorkflow: 20,
        statusAt: (workflow, stage) =>
          statuses[(workflow + stage) % statuses.length]!,
      }),
    );

    expect(layout.aggregatePlan).toMatchObject({
      workflows: 50,
      stations: 1_000,
      tracks: 1_000,
      carriers: 0,
      workers: 0,
    });
    expect(layout.instanceBatches.length).toBeLessThanOrEqual(48);
    expect(layout.aggregatePlan.drawCalls.instancedPlan).toBeLessThanOrEqual(64);
    expect(layout.lod.maxDomItems).toBeLessThanOrEqual(
      FACTORY_PLANT_LOD_THRESHOLDS.maxDetailDomItems,
    );
    expect(layout.lod.levels.detail.totalCandidates).toBe(1_000);
    expect(layout.lod.levels.detail.truncated).toBe(true);
    expectCollisionFree(layout);
  });

  it("uses no randomness or wall clock", () => {
    expect(factoryPlantLayoutSource).not.toMatch(/Math\.random\(/);
    expect(factoryPlantLayoutSource).not.toMatch(/Date\.now\(/);
    expect(factoryPlantLayoutSource).not.toMatch(/new Date\(/);
    expect(factoryPlantLayoutSource).not.toMatch(/performance\.now\(/);
  });

  it("keeps fixed bitmap anchors and decorative districts out of WebGL", () => {
    expect(plantEntitySource).not.toMatch(/factoryClassicPlant/);
    expect(plantEntitySource).not.toMatch(/MACHINE_ANCHORS/);
    expect(plantSceneGraphSource).not.toMatch(/const districts\s*=/);
    expect(plantSceneGraphSource).not.toMatch(/local conveyor/i);
  });
});
