import { describe, expect, it } from "vitest";
import type { GaggleInventory } from "./operationalData";
import { buildFactoryFloorModel, type FactoryModelInput } from "./factoryModel";
import {
  buildFactoryPlant,
  FACTORY_PLANT_TILE,
  type PlantPoint,
} from "./factoryPlant";
import type { RunSummary, WorkflowDetail } from "./api/types";
import factoryPlantSource from "./factoryPlant.ts?raw";

/**
 * The plant projection is the only new geometry in the second layout, so it is
 * held to the same standard as the model it draws: pure, deterministic, and
 * derived entirely from daemon facts. These tests pin that contract.
 */

function inventory(): GaggleInventory {
  return {
    gaggle: {
      name: "core",
      displayName: "Core product",
      status: "configured",
      project: { provider: "github", owner: "example", name: "example" },
      backlog: { provider: "github", project: "example/example" },
      gooberCount: 2,
      workflowCount: 1,
      activeRunCount: 2,
      warnings: [],
    },
    goobers: [
      {
        name: "implementer",
        displayName: "Core implementer",
        role: "Implements claimed backlog items.",
        status: "configured",
        harness: "copilot",
        skills: [],
        capabilities: [],
        workflows: [{ gaggle: "core", name: "implementation" }],
        stages: [
          {
            workflow: { gaggle: "core", name: "implementation" },
            stage: "implement",
            kind: "agentic",
          },
        ],
        warnings: [],
      },
      {
        name: "analyst",
        displayName: "Core analyst",
        role: "Reviews claimed backlog items.",
        status: "configured",
        harness: "copilot",
        skills: [],
        capabilities: [],
        workflows: [{ gaggle: "core", name: "implementation" }],
        stages: [],
        warnings: [],
      },
    ],
    workflows: [
      {
        identity: { gaggle: "core", name: "implementation" },
        displayName: "Implementation",
        purpose: "Implement approved backlog items.",
        triggers: [],
        readiness: { maxConcurrentRuns: 2 },
        concurrency: { activeRuns: 2, maxConcurrentRuns: 2 },
        owners: [{ gaggle: "core", name: "implementer" }],
        stageCount: 3,
        definition: { version: 7, digest: "sha256:core" },
        warnings: [],
      },
    ],
    connections: undefined,
  } as unknown as GaggleInventory;
}

function detail(): WorkflowDetail {
  return {
    identity: { gaggle: "core", name: "implementation" },
    displayName: "Implementation",
    purpose: "Implement approved backlog items.",
    triggers: [],
    readiness: { maxConcurrentRuns: 2 },
    concurrency: { activeRuns: 2, maxConcurrentRuns: 2 },
    owners: [{ gaggle: "core", name: "implementer" }],
    stageCount: 3,
    definition: { version: 7, digest: "sha256:core" },
    warnings: [],
    graph: {
      name: "implementation",
      version: 7,
      digest: "sha256:core",
      start: "query",
      nodes: [
        { id: "query", kind: "deterministic" },
        { id: "implement", kind: "agentic", owner: "core/implementer" },
        { id: "review", kind: "gate", evaluator: "agentic" },
      ],
      edges: [
        { source: "query", target: "implement" },
        { source: "implement", target: "review" },
        { source: "review", target: "", outcome: "approve", terminal: "complete" },
        { source: "review", target: "implement", outcome: "needs-changes" },
      ],
    },
    stages: [
      {
        name: "query",
        kind: "deterministic",
        goal: "Claim work.",
        owner: null,
        evaluator: "",
        capabilities: [],
      },
      {
        name: "implement",
        kind: "agentic",
        goal: "Do the work.",
        owner: { gaggle: "core", name: "implementer" },
        evaluator: "",
        capabilities: [],
      },
      {
        name: "review",
        kind: "gate",
        goal: "Approve the work.",
        owner: null,
        evaluator: "agentic",
        capabilities: [],
      },
    ],
  } as unknown as WorkflowDetail;
}

function run(id: string, stage: string | undefined, minute: number): RunSummary {
  const startedAt = new Date(Date.parse("2026-07-18T06:00:00Z") + minute * 60_000)
    .toISOString();
  return {
    id,
    workflow: "implementation",
    workflowVersion: 7,
    workflowDigest: "sha256:core",
    gaggle: "core",
    trigger: { kind: "item", ref: "ref" },
    phase: "running",
    terminal: false,
    currentStage: stage,
    startedAt,
    durationMillis: 60_000,
    lastActivityAt: startedAt,
    lastSeq: 4,
    repassCount: 0,
    retryCount: 0,
    policyRetryCount: 0,
    infraRetryCount: 0,
  };
}

function modelInput(overrides: Partial<FactoryModelInput> = {}): FactoryModelInput {
  return {
    inventories: [inventory()],
    workflowDetails: new Map([["core/implementation", detail()]]),
    activeRuns: [
      run("01RUNIMPLEMENT1", "implement", 1),
      run("01RUNREVIEW0001", "review", 2),
      run("01RUNQUEUED0001", undefined, 3),
    ],
    runSignals: new Map([
      ["01RUNIMPLEMENT1", { state: "running", confirmed: true }],
      ["01RUNREVIEW0001", { state: "paused", reason: "human-gate", confirmed: true }],
      ["01RUNQUEUED0001", { state: "starting", reason: "awaiting-stage", confirmed: true }],
    ]),
    ...overrides,
  };
}

function distance(left: PlantPoint, right: PlantPoint): number {
  return Math.hypot(right.x - left.x, right.y - left.y);
}

function polygonPoints(value: string): PlantPoint[] {
  return value.split(" ").map((pair) => {
    const [x, y] = pair.split(",").map(Number);
    return { x, y };
  });
}

function pointInPolygon(point: PlantPoint, polygon: readonly PlantPoint[]): boolean {
  let inside = false;
  for (let index = 0, previous = polygon.length - 1; index < polygon.length; previous = index++) {
    const currentPoint = polygon[index];
    const previousPoint = polygon[previous];
    const crosses =
      currentPoint.y > point.y !== previousPoint.y > point.y &&
      point.x <
        ((previousPoint.x - currentPoint.x) * (point.y - currentPoint.y)) /
          (previousPoint.y - currentPoint.y) +
          currentPoint.x;
    if (crosses) {
      inside = !inside;
    }
  }
  return inside;
}

describe("isometric plant projection", () => {
  it("is deterministic: the same model always projects to the same geometry", () => {
    const model = buildFactoryFloorModel(modelInput());

    const first = buildFactoryPlant(model);
    const second = buildFactoryPlant(model);

    expect(JSON.stringify(second)).toBe(JSON.stringify(first));
    expect(JSON.stringify(buildFactoryPlant(buildFactoryFloorModel(modelInput())))).toBe(
      JSON.stringify(first),
    );
  });

  it("uses no randomness and no wall clock", () => {
    expect(factoryPlantSource).not.toMatch(/Math\.random\(/);
    expect(factoryPlantSource).not.toMatch(/Date\.now\(/);
    expect(factoryPlantSource).not.toMatch(/new Date\(/);
    expect(factoryPlantSource).not.toMatch(/performance\.now\(/);
  });

  it("draws one machine per model stage, keeping the model's own IDs", () => {
    const model = buildFactoryFloorModel(modelInput());
    const scene = buildFactoryPlant(model);

    expect(scene.machines.map((machine) => machine.id).sort()).toEqual(
      model.stations.map((station) => station.id).sort(),
    );
    expect(scene.machines.map((machine) => machine.stageId).sort()).toEqual([
      "implement",
      "query",
      "review",
    ]);
    for (const machine of scene.machines) {
      const station = model.stations.find((candidate) => candidate.id === machine.id)!;
      expect(machine.kind).toBe(station.kind);
      expect(machine.status).toBe(station.status);
      expect(machine.wip).toBe(station.wip);
      expect(machine.districtId).toBe(station.laneId);
    }
  });

  it("draws one belt per readable graph edge and no invented routes", () => {
    const model = buildFactoryFloorModel(modelInput());
    const scene = buildFactoryPlant(model);

    const conveyors = model.lanes.flatMap((lane) => lane.conveyors);
    expect(scene.tracks.map((track) => track.id).sort()).toEqual(
      conveyors.map((conveyor) => conveyor.id).sort(),
    );
    expect(scene.tracks.map((track) => track.kind).sort()).toEqual([
      "forward",
      "forward",
      "repass",
      "terminal",
    ]);
    for (const track of scene.tracks) {
      expect(track.bed.startsWith("M ")).toBe(true);
      expect(track.rails).toHaveLength(2);
    }
  });

  it("places every outcome label by arc length near its target with separate fork offsets", () => {
    const forked = detail();
    forked.graph.edges = [
      { source: "query", target: "implement" },
      { source: "implement", target: "review", outcome: "ready" },
      { source: "review", target: "", outcome: "approve", terminal: "complete" },
      { source: "review", target: "", outcome: "fail", terminal: "escalate" },
      { source: "review", target: "implement", outcome: "needs-changes" },
    ];
    const scene = buildFactoryPlant(
      buildFactoryFloorModel(
        modelInput({
          workflowDetails: new Map([["core/implementation", forked]]),
        }),
      ),
    );
    const outcomes = scene.tracks.filter((track) => track.outcome);

    expect(outcomes.map((track) => track.outcome).sort()).toEqual([
      "approve",
      "fail",
      "needs-changes",
      "ready",
    ]);
    for (const track of outcomes) {
      expect(track.label).toBeDefined();
      expect(track.label!.width).toBeGreaterThan(40);
      expect(distance(track.label!, track.points.at(-1)!)).toBeLessThan(
        distance(track.label!, track.points[0]),
      );
    }
    const approve = outcomes.find((track) => track.outcome === "approve")!.label!;
    const fail = outcomes.find((track) => track.outcome === "fail")!.label!;
    expect({ x: approve.x, y: approve.y }).not.toEqual({ x: fail.x, y: fail.y });
  });

  it("allocates a deterministic return lane and label position per repass edge", () => {
    const repeated = detail();
    repeated.graph.edges = [
      ...repeated.graph.edges,
      { source: "review", target: "query", outcome: "restart" },
    ];
    const model = buildFactoryFloorModel(
      modelInput({
        workflowDetails: new Map([["core/implementation", repeated]]),
      }),
    );

    const first = buildFactoryPlant(model);
    const second = buildFactoryPlant(model);
    const repasses = first.tracks.filter((track) => track.kind === "repass");
    expect(repasses).toHaveLength(2);
    expect(new Set(repasses.map((track) => track.bed)).size).toBe(2);
    expect(
      new Set(repasses.map((track) => `${track.label?.x},${track.label?.y}`)).size,
    ).toBe(2);
    expect(first.districts[0].returnTracks).toHaveLength(2);
    expect(second.tracks.filter((track) => track.kind === "repass")).toEqual(repasses);
  });

  it("places every rendered run and only rendered runs", () => {
    const model = buildFactoryFloorModel(modelInput());
    const scene = buildFactoryPlant(model);

    expect(scene.crates.map((crate) => crate.id).sort()).toEqual(
      model.carriers
        .filter((carrier) => carrier.rendered)
        .map((carrier) => carrier.runId)
        .sort(),
    );
    const queued = scene.crates.find((crate) => crate.id === "01RUNQUEUED0001")!;
    expect(queued.holderId).toBe("core/implementation#yard");
  });

  it("stands a goober beside a stage it owns and parks the rest in the commons", () => {
    const model = buildFactoryFloorModel(modelInput());
    const scene = buildFactoryPlant(model);

    const posted = scene.staff.find((figure) => figure.stationId !== undefined)!;
    expect(posted.workerId).toBe("core/implementer");
    expect(posted.active).toBe(true);
    expect(posted.stationId).toBe("core/implementation/implement");

    const idle = scene.staff.find((figure) => figure.stationId === undefined)!;
    expect(idle.workerId).toBe("core/analyst");
    expect(idle.active).toBe(false);
    expect(scene.commons).toBeDefined();
  });

  it("moves only the run whose stage changed", () => {
    const before = buildFactoryFloorModel(modelInput());
    const after = buildFactoryFloorModel(
      modelInput({
        activeRuns: [
          run("01RUNIMPLEMENT1", "review", 1),
          run("01RUNREVIEW0001", "review", 2),
          run("01RUNQUEUED0001", undefined, 3),
        ],
        previous: before,
      }),
    );

    const first = buildFactoryPlant(before);
    const second = buildFactoryPlant(after);

    const moved = second.crates.find((crate) => crate.id === "01RUNIMPLEMENT1")!;
    expect(moved.moved).toBe(true);
    expect(moved.holderId).toBe("core/implementation/review");

    const sibling = (scene: typeof first, id: string) =>
      scene.crates.find((crate) => crate.id === id)!;
    expect(sibling(second, "01RUNQUEUED0001").x).toBe(
      sibling(first, "01RUNQUEUED0001").x,
    );
    expect(sibling(second, "01RUNQUEUED0001").y).toBe(
      sibling(first, "01RUNQUEUED0001").y,
    );
    expect(sibling(second, "01RUNQUEUED0001").moved).toBe(false);
  });

  it("carries the alarm from the model and gives it hardware to show it", () => {
    const model = buildFactoryFloorModel(
      modelInput({
        activeRuns: [run("01RUNBLOCKED001", "implement", 1)],
        runSignals: new Map([
          ["01RUNBLOCKED001", { state: "blocked", reason: "stage-blocked", confirmed: true }],
        ]),
      }),
    );
    const scene = buildFactoryPlant(model);

    const machine = scene.machines.find((candidate) => candidate.stageId === "implement")!;
    expect(machine.alarm).toBe("blocked");
    expect(machine.beaconSize).toBeGreaterThan(0);
    expect(machine.wash.split(" ")).toHaveLength(4);
    expect(scene.machines.find((candidate) => candidate.stageId === "query")?.alarm)
      .toBeUndefined();
  });

  it("clips every machine and crate to its own silhouette", () => {
    const scene = buildFactoryPlant(buildFactoryFloorModel(modelInput()));

    for (const machine of scene.machines) {
      // Prism, beacon housing, placard.
      expect(machine.clip.match(/Z/g)).toHaveLength(3);
      expect(machine.clip.startsWith("M ")).toBe(true);
    }
    for (const crate of scene.crates) {
      expect(crate.clip.match(/Z/g)).toHaveLength(1);
    }
  });

  it("uses one fixed tile and never moves existing geometry as the plant grows", () => {
    const small = buildFactoryPlant(buildFactoryFloorModel(modelInput()));
    expect(small.tile).toBe(FACTORY_PLANT_TILE);

    const baseModel = buildFactoryFloorModel(modelInput());
    const extraLane = structuredClone(baseModel.lanes[0]);
    extraLane.id = "core/second";
    extraLane.workflow = "second";
    extraLane.displayName = "Second workflow";
    extraLane.stations = [];
    extraLane.docks = [];
    extraLane.conveyors = [];
    extraLane.yard = {
      ...extraLane.yard,
      id: "core/second#yard",
      laneId: extraLane.id,
    };
    const expanded = buildFactoryPlant({
      ...baseModel,
      lanes: [...baseModel.lanes, extraLane],
    });
    expect(expanded.tile).toBe(FACTORY_PLANT_TILE);
    expect(expanded.districts[0]).toEqual(small.districts[0]);
    for (const machine of small.machines) {
      expect(expanded.machines.find((candidate) => candidate.id === machine.id)).toEqual(
        machine,
      );
    }

    const topologyLane = structuredClone(baseModel.lanes[0]);
    topologyLane.stations.push({
      ...structuredClone(topologyLane.stations[0]),
      id: "core/implementation/later-stage",
      stageId: "later-stage",
      column: 5,
      row: 0,
    });
    const withLaterTopology = buildFactoryPlant({
      ...baseModel,
      lanes: [topologyLane],
    });
    for (const machine of small.machines) {
      expect(
        withLaterTopology.machines.find((candidate) => candidate.id === machine.id),
      ).toEqual(machine);
    }
  });

  it("keeps every district identifiable and separated", () => {
    const model = buildFactoryFloorModel(modelInput());
    const scene = buildFactoryPlant(model);

    expect(scene.districts.map((district) => district.id)).toEqual(
      model.lanes.map((lane) => lane.id),
    );
    expect(scene.districts[0].floorLabel.text).toBe(
      "Core product · Implementation",
    );
    expect(scene.districts[0].yard.id).toBe(model.lanes[0].yard.id);

    const earlierLane = structuredClone(model.lanes[0]);
    earlierLane.id = "other/workflow";
    earlierLane.gaggleDisplayName = "Other gaggle";
    earlierLane.displayName = "Other workflow";
    earlierLane.stations = [];
    earlierLane.docks = [];
    earlierLane.conveyors = [];
    earlierLane.yard = {
      ...earlierLane.yard,
      id: "other/workflow#yard",
      laneId: earlierLane.id,
    };
    const reordered = buildFactoryPlant({
      ...model,
      lanes: [earlierLane, ...model.lanes],
    });
    expect(
      reordered.districts.find((district) => district.id === model.lanes[0].id)
        ?.floorLabel.text,
    ).toBe("Core product · Implementation");
  });

  it("keeps stage overflow anchors inside their own district apron", () => {
    const scene = buildFactoryPlant(buildFactoryFloorModel(modelInput()));
    const districtById = new Map(
      scene.districts.map((district) => [
        district.id,
        polygonPoints(district.plot),
      ]),
    );
    for (const machine of scene.machines) {
      expect(
        pointInPolygon(machine.apron, districtById.get(machine.districtId)!),
      ).toBe(true);
    }
  });

  it("shows an unread topology as an empty marked bay rather than invented machines", () => {
    const model = buildFactoryFloorModel(
      modelInput({ workflowDetails: new Map(), activeRuns: [], runSignals: new Map() }),
    );
    const scene = buildFactoryPlant(model);

    expect(scene.machines).toHaveLength(0);
    expect(scene.tracks).toHaveLength(0);
    expect(scene.districts[0].emptyBay).toBeDefined();
    expect(scene.districts[0].source).toBe("observed");
    expect(scene.width).toBeGreaterThan(0);
    expect(scene.height).toBeGreaterThan(0);
  });
});
