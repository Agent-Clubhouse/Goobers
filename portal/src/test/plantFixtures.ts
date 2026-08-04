import { buildClassicPlant, type ClassicPlantScene } from "../factoryClassicPlant";
import {
  buildFactoryFloorModel,
  type FactoryCarrier,
  type FactoryConveyor,
  type FactoryFloorModel,
  type FactoryLane,
  type FactoryModelInput,
  type FactoryStation,
  type FactoryStationStatus,
  type FactoryWorker,
} from "../factoryModel";
import type { GaggleInventory } from "../operationalData";
import type { RunSummary, WorkflowDetail } from "../api/types";
import {
  buildFactoryPlantLayout,
  type FactoryPlantAllocation,
  type FactoryPlantLayout,
} from "../factoryPlantLayout";

/**
 * Confirmed-daemon fixtures for the WebGL Plant runtime tests.
 *
 * The runtime is judged on identity — station ids, run ids, placement ids — so
 * the tests build the real model and the real classic plant instead of hand
 * writing scene shapes that could drift from what the portal actually renders.
 */

function inventory(): GaggleInventory {
  return {
    gaggle: {
      name: "core",
      displayName: "Core product",
      status: "configured",
      project: { provider: "github", owner: "example", name: "example" },
      backlog: { provider: "github", project: "example/example" },
      gooberCount: 1,
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
        rawYaml: "",
      },
      {
        name: "implement",
        kind: "agentic",
        goal: "Do the work.",
        owner: { gaggle: "core", name: "implementer" },
        evaluator: "",
        capabilities: [],
        rawYaml: "",
      },
      {
        name: "review",
        kind: "gate",
        goal: "Approve the work.",
        owner: null,
        evaluator: "agentic",
        capabilities: [],
        rawYaml: "",
      },
    ],
  } as unknown as WorkflowDetail;
}

function run(id: string, stage: string | undefined, minute: number): RunSummary {
  const startedAt = new Date(
    Date.parse("2026-07-18T06:00:00Z") + minute * 60_000,
  ).toISOString();
  return {
    id,
    workflow: "implementation",
    workflowVersion: 7,
    workflowDigest: "sha256:core",
    gaggle: "core",
    trigger: { kind: "item", ref: "ref" },
    phase: "running",
    terminal: false,
    noWork: false,
    currentStage: stage,
    startedAt,
    durationMillis: 60_000,
    lastActivityAt: startedAt,
    lastSeq: 4,
    repassCount: 0,
    retryCount: 0,
    policyRetryCount: 0,
    infraRetryCount: 0,
  } as unknown as RunSummary;
}

export interface PlantFixtureOptions {
  /** Stage each run currently occupies, keyed by run id. */
  stages?: Record<string, string | undefined>;
  /** Runs to include; defaults to two confirmed running runs. */
  runIds?: readonly string[];
  /** Confirmed run signals, keyed by run id. */
  signals?: Record<string, { state: string; reason?: string; confirmed: boolean }>;
  /** Prior model, which is what turns a stage move into a confirmed transition. */
  previous?: FactoryFloorModel;
}

export function plantFixtureModel(options: PlantFixtureOptions = {}): FactoryFloorModel {
  const runIds = options.runIds ?? ["01RUNIMPLEMENT1", "01RUNREVIEW0001"];
  const stages = options.stages ?? {
    "01RUNIMPLEMENT1": "implement",
    "01RUNREVIEW0001": "review",
  };
  const signals =
    options.signals ??
    Object.fromEntries(
      runIds.map((id) => [id, { state: "running", confirmed: true }]),
    );
  const input: FactoryModelInput = {
    inventories: [inventory()],
    workflowDetails: new Map([["core/implementation", detail()]]),
    activeRuns: runIds.map((id, index) => run(id, stages[id], index + 1)),
    runSignals: new Map(
      Object.entries(signals),
    ) as unknown as FactoryModelInput["runSignals"],
    ...(options.previous ? { previous: options.previous } : {}),
  };
  return buildFactoryFloorModel(input);
}

export function plantFixtureScene(model: FactoryFloorModel): ClassicPlantScene {
  return buildClassicPlant(model);
}

export function plantFixtureLayout(
  model: FactoryFloorModel,
  previous?: FactoryPlantAllocation | FactoryPlantLayout,
): FactoryPlantLayout {
  return buildFactoryPlantLayout(model, { previous });
}

export function plantFixture(options: PlantFixtureOptions = {}): {
  model: FactoryFloorModel;
  scene: ClassicPlantScene;
  layout: FactoryPlantLayout;
} {
  const model = plantFixtureModel(options);
  return {
    layout: plantFixtureLayout(model),
    model,
    scene: plantFixtureScene(model),
  };
}

/**
 * A confirmed stage change: the same run observed at two stages, with the
 * second model built against the first so the carrier carries a transition.
 */
export function plantStageChangeFixture(
  runId = "01RUNIMPLEMENT1",
): {
  before: {
    model: FactoryFloorModel;
    scene: ClassicPlantScene;
    layout: FactoryPlantLayout;
  };
  after: {
    model: FactoryFloorModel;
    scene: ClassicPlantScene;
    layout: FactoryPlantLayout;
  };
} {
  const before = plantFixture({
    stages: { [runId]: "query", "01RUNREVIEW0001": "review" },
  });
  const model = plantFixtureModel({
    previous: before.model,
    stages: { [runId]: "implement", "01RUNREVIEW0001": "review" },
  });
  return {
    after: {
      layout: plantFixtureLayout(model, before.layout),
      model,
      scene: plantFixtureScene(model),
    },
    before,
  };
}

export interface ScalablePlantFixtureOptions {
  workflowCount?: number;
  stagesPerWorkflow?: number;
  workflowIds?: readonly string[];
  carriersPerWorkflow?: number;
  workersPerWorkflow?: number;
  statusAt?: (workflowIndex: number, stageIndex: number) => FactoryStationStatus;
  wipAt?: (workflowIndex: number, stageIndex: number) => number;
}

/**
 * Large renderer-neutral fixture without daemon/API overhead.
 *
 * It intentionally uses the model's station column/row and exact chain edges,
 * making 50x20 layout stress tests cheap while preserving the same identities
 * and truth fields the production planner reads.
 */
export function scalablePlantFixture(
  options: ScalablePlantFixtureOptions = {},
): FactoryFloorModel {
  const workflowIds =
    options.workflowIds ??
    Array.from(
      { length: options.workflowCount ?? 1 },
      (_, index) => `gaggle/workflow-${String(index + 1).padStart(3, "0")}`,
    );
  const stagesPerWorkflow = options.stagesPerWorkflow ?? 1;
  const carriersPerWorkflow = options.carriersPerWorkflow ?? 0;
  const workersPerWorkflow = options.workersPerWorkflow ?? 0;
  const lanes: FactoryLane[] = [];
  const stations: FactoryStation[] = [];
  const carriers: FactoryCarrier[] = [];
  const workers: FactoryWorker[] = [];

  workflowIds.forEach((laneId, workflowIndex) => {
    const slash = laneId.indexOf("/");
    const gaggle = slash >= 0 ? laneId.slice(0, slash) : "gaggle";
    const workflow = slash >= 0 ? laneId.slice(slash + 1) : laneId;
    const laneStations: FactoryStation[] = [];
    const runIds = Array.from(
      { length: carriersPerWorkflow },
      (_, index) =>
        `01LAYOUT${String(workflowIndex).padStart(3, "0")}${String(index).padStart(3, "0")}`,
    );
    for (let stageIndex = 0; stageIndex < stagesPerWorkflow; stageIndex += 1) {
      const status = options.statusAt?.(workflowIndex, stageIndex) ?? "idle";
      const wip =
        options.wipAt?.(workflowIndex, stageIndex) ??
        (stageIndex === 0 ? carriersPerWorkflow : 0);
      const stationId = `${laneId}/stage-${String(stageIndex + 1).padStart(3, "0")}`;
      const stationRunIds = stageIndex === 0 ? runIds : [];
      const station: FactoryStation = {
        id: stationId,
        laneId,
        stageId: `stage-${String(stageIndex + 1).padStart(3, "0")}`,
        gaggle,
        workflow,
        workflowDisplayName: workflow,
        kind:
          stageIndex % 4 === 0
            ? "deterministic"
            : stageIndex % 4 === 1
              ? "agentic"
              : stageIndex % 4 === 2
                ? "gate"
                : "parallel",
        source: "declared",
        isStart: stageIndex === 0,
        column: stageIndex,
        row: 0,
        x: stageIndex * 100,
        y: workflowIndex * 100,
        width: 80,
        height: 70,
        wip,
        limit: Math.max(1, carriersPerWorkflow),
        saturation:
          carriersPerWorkflow > 0 ? wip / carriersPerWorkflow : undefined,
        blockedCount:
          status === "blocked" || status === "impeded" ? Math.max(1, wip) : 0,
        hardBlockedCount: status === "blocked" ? Math.max(1, wip) : 0,
        pausedCount: status === "held" ? Math.max(1, wip) : 0,
        unknownCount: status === "unknown" ? Math.max(1, wip) : 0,
        status,
        ...(status === "blocked"
          ? { alarm: "blocked" as const }
          : status === "held"
            ? { alarm: "hold" as const }
            : {}),
        runIds: stationRunIds,
        renderedRunIds: stationRunIds,
        overflowRunCount: 0,
        workerIds: [],
        renderedWorkerIds: [],
        workerOverflowCount: 0,
      };
      laneStations.push(station);
      stations.push(station);
    }

    const dockId = `${laneId}#dock:complete`;
    const conveyors: FactoryConveyor[] = laneStations
      .slice(0, -1)
      .map((station, stageIndex) => ({
      id: `${laneId}#edge:${stageIndex}`,
      laneId,
      kind: "forward" as const,
      fromStationId: station.id,
      toId: laneStations[stageIndex + 1]!.id,
      path: "",
      labelX: 0,
      labelY: 0,
      active: false,
      }));
    if (laneStations.length > 0) {
      conveyors.push({
        id: `${laneId}#edge:terminal`,
        laneId,
        kind: "terminal",
        fromStationId: laneStations.at(-1)!.id,
        toId: dockId,
        terminal: "complete",
        path: "",
        labelX: 0,
        labelY: 0,
        active: false,
      });
    }
    const yardId = `${laneId}#yard`;
    const lane: FactoryLane = {
      id: laneId,
      gaggle,
      gaggleDisplayName: gaggle,
      workflow,
      displayName: workflow,
      source: "declared",
      stageCount: stagesPerWorkflow,
      stations: laneStations,
      docks: [
        {
          id: dockId,
          laneId,
          terminal: "complete",
          x: stagesPerWorkflow * 100,
          y: workflowIndex * 100,
          width: 70,
          height: 70,
        },
      ],
      conveyors,
      yard: {
        id: yardId,
        laneId,
        x: 0,
        y: workflowIndex * 100,
        width: 80,
        height: 70,
        runIds: [],
        renderedRunIds: [],
        overflowRunCount: 0,
      },
      activeRuns: carriersPerWorkflow,
      blockedRuns: 0,
      unreadRuns: 0,
      limit: Math.max(1, carriersPerWorkflow),
      saturation: carriersPerWorkflow > 0 ? 1 : 0,
      x: 0,
      y: workflowIndex * 100,
      width: Math.max(1, stagesPerWorkflow) * 100,
      height: 100,
    };
    lanes.push(lane);

    runIds.forEach((runId, index) => {
      const station = laneStations[0];
      if (!station) {
        return;
      }
      carriers.push({
        runId,
        gaggle,
        workflow,
        workflowDisplayName: workflow,
        laneId,
        stageId: station.stageId,
        stationId: station.id,
        phase: "running",
        state: "running",
        confirmed: true,
        triggerKind: "manual",
        startedAt: "2026-08-03T00:00:00Z",
        lastActivityAt: "2026-08-03T00:01:00Z",
        durationMillis: 60_000,
        retryCount: 0,
        policyRetryCount: 0,
        infraRetryCount: 0,
        repassCount: 0,
        queueIndex: index,
        rendered: true,
        renderSlot: index,
        x: 0,
        y: 0,
      });
    });

    for (let workerIndex = 0; workerIndex < workersPerWorkflow; workerIndex += 1) {
      const station = laneStations[workerIndex % Math.max(1, laneStations.length)];
      if (!station) {
        continue;
      }
      const workerId = `${gaggle}/worker-${workflowIndex}-${workerIndex}`;
      const placementId = `${workerId}@${station.id}`;
      station.workerIds.push(workerId);
      station.renderedWorkerIds.push(workerId);
      workers.push({
        id: workerId,
        gaggle,
        gaggleDisplayName: gaggle,
        name: `worker-${workflowIndex}-${workerIndex}`,
        displayName: `Worker ${workflowIndex}-${workerIndex}`,
        harness: "copilot",
        status: "configured",
        stages: [
          {
            gaggle,
            workflow,
            stage: station.stageId,
            kind: station.kind,
            stationId: station.id,
            inScope: true,
          },
        ],
        activeRunCount: 1,
        activeStationIds: [station.id],
        placements: [
          {
            id: placementId,
            workerId,
            stationId: station.id,
            x: 0,
            y: 0,
            active: true,
            rendered: true,
          },
        ],
        idle: false,
      });
    }
  });

  const statusCounts = stations.reduce(
    (counts, station) => {
      counts[station.status] += 1;
      return counts;
    },
    {
      idle: 0,
      running: 0,
      impeded: 0,
      held: 0,
      blocked: 0,
      unknown: 0,
    } satisfies Record<FactoryStationStatus, number>,
  );
  return {
    scope: {},
    gaggles: [
      {
        name: "gaggle",
        displayName: "Gaggle",
        status: "configured",
        workflowCount: lanes.length,
        gooberCount: workers.length,
        activeRuns: carriers.length,
        unreadRuns: 0,
        heldStages: statusCounts.held,
        blockedStages: statusCounts.blocked,
      },
    ],
    workflows: lanes.map((lane) => ({
      gaggle: lane.gaggle,
      name: lane.workflow,
      displayName: lane.displayName,
      laneId: lane.id,
    })),
    lanes,
    stations,
    carriers,
    workers,
    commons: {
      x: 0,
      y: 0,
      width: 0,
      height: 0,
      workerIds: [],
      renderedWorkerIds: [],
      overflowWorkerCount: 0,
    },
    attention: [],
    counts: {
      gaggles: 1,
      workflows: lanes.length,
      goobers: workers.length,
      idleGoobers: 0,
      activeRuns: carriers.length,
      blockedRuns: 0,
      unreadRuns: 0,
      heldStages: statusCounts.held,
      blockedStages: statusCounts.blocked,
      queuedRuns: 0,
    },
    capacity: {
      wip: carriers.length,
      limit: Math.max(1, carriers.length),
      unknownLimits: 0,
      saturation: carriers.length > 0 ? 1 : 0,
    },
    emptyReason: lanes.length === 0 ? "no-workflows" : undefined,
    runsTruncated: false,
    width: Math.max(1, stagesPerWorkflow) * 100,
    height: Math.max(1, lanes.length) * 100,
  };
}
