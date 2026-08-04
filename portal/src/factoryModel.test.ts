import { describe, expect, it } from "vitest";
import type {
  Gaggle,
  Goober,
  RunEvent,
  RunSummary,
  StageAttempt,
  WorkflowDetail,
  WorkflowSummary,
} from "./api/types";
import {
  FACTORY_RENDERED_CARRIERS_PER_STATION,
  FACTORY_RENDERED_CARRIERS_PER_YARD,
  FACTORY_RENDERED_COMMONS_WORKERS,
  buildFactoryFloorModel,
  deriveRunSignal,
  laneKey,
  stationKey,
  validateFactoryScope,
  type FactoryFloorModel,
  type FactoryRunSignal,
} from "./factoryModel";
import type { GaggleInventory } from "./operationalData";

/**
 * The Factory Floor is only defensible if the plant is the daemon's own facts.
 * These tests pin the semantics that make it truthful: real inventories become
 * real buildings, a run stands where the daemon says it stands, motion happens
 * only on a genuine stage change, "blocked" means blocked, capacity is the
 * workflow's own limit or nothing at all, and no free-form or sensitive field
 * ever reaches the view model.
 */

const SENSITIVE = {
  triggerRef: "trigger-ref-should-not-render",
  attemptMessage: "free-form attempt message",
  journalMessage: "free-form journal message",
  purpose: "Implement approved backlog items for org/private-repo.",
  goal: "Claim the next approved backlog item from https://example.invalid/queue",
  digest: "sha256:deadbeefcafefeed",
  owner: "Very-Private-Org",
  repository: "private-repository",
  skill: "internal-tooling-skill",
  capability: "repo:push:private",
  artifact: "verdict.json",
};

function gaggle(name: string, displayName: string): Gaggle {
  return {
    name,
    displayName,
    status: "configured",
    project: { provider: "github", owner: SENSITIVE.owner, name: SENSITIVE.repository },
    backlog: { provider: "github", project: `${SENSITIVE.owner}/${SENSITIVE.repository}` },
    gooberCount: 1,
    workflowCount: 1,
    activeRunCount: 0,
    warnings: [],
  };
}

function workflowSummary(
  gaggleName: string,
  name: string,
  maxConcurrentRuns: number | undefined,
): WorkflowSummary {
  return {
    identity: { gaggle: gaggleName, name },
    displayName: name === "implementation" ? "Implementation" : "Review queue",
    purpose: SENSITIVE.purpose,
    triggers: [{ type: "backlog-item", selector: { label: "ready" } }],
    readiness: maxConcurrentRuns === undefined ? {} : { maxConcurrentRuns },
    concurrency: { activeRuns: 0, maxConcurrentRuns: maxConcurrentRuns ?? 0 },
    owners: [{ gaggle: gaggleName, name: "implementer" }],
    stageCount: 3,
    definition: { version: 7, digest: SENSITIVE.digest },
    warnings: [],
  };
}

function goober(gaggleName: string, name: string, stages: string[]): Goober {
  return {
    name,
    displayName: `${gaggleName} ${name}`,
    role: "Implements claimed backlog items end to end.",
    status: "configured",
    harness: "copilot",
    skills: [SENSITIVE.skill],
    capabilities: [SENSITIVE.capability],
    workflows: [{ gaggle: gaggleName, name: "implementation" }],
    stages: stages.map((stage) => ({
      workflow: { gaggle: gaggleName, name: "implementation" },
      stage,
      kind: stage === "review" ? "gate" : "agentic",
    })),
    warnings: [],
  };
}

function workflowDetail(
  gaggleName: string,
  name = "implementation",
  maxConcurrentRuns: number | undefined = 2,
): WorkflowDetail {
  return {
    ...workflowSummary(gaggleName, name, maxConcurrentRuns),
    graph: {
      name,
      version: 7,
      digest: SENSITIVE.digest,
      start: "query",
      nodes: [
        { id: "query", kind: "deterministic" },
        { id: "implement", kind: "agentic", owner: `${gaggleName}/implementer` },
        { id: "review", kind: "gate", evaluator: "agentic" },
      ],
      edges: [
        { source: "query", target: "implement" },
        { source: "implement", target: "review" },
        { source: "review", target: "", outcome: "approve", terminal: "complete" },
        { source: "review", target: "implement", outcome: "needs-changes" },
        { source: "review", target: "@escalate", outcome: "fail", terminal: "escalate" },
      ],
    },
    stages: [
      {
        name: "query",
        kind: "deterministic",
        goal: SENSITIVE.goal,
        owner: null,
        evaluator: "",
        capabilities: [SENSITIVE.capability],
        rawYaml: "",
      },
      {
        name: "implement",
        kind: "agentic",
        goal: SENSITIVE.goal,
        owner: { gaggle: gaggleName, name: "implementer" },
        evaluator: "",
        capabilities: [SENSITIVE.capability],
        rawYaml: "",
      },
      {
        name: "review",
        kind: "gate",
        goal: SENSITIVE.goal,
        owner: { gaggle: gaggleName, name: "implementer" },
        evaluator: "agentic",
        capabilities: [SENSITIVE.capability],
        rawYaml: "",
      },
    ],
  };
}

function inventory(
  name: string,
  displayName: string,
  options: { workflows?: WorkflowSummary[]; goobers?: Goober[] } = {},
): GaggleInventory {
  return {
    gaggle: gaggle(name, displayName),
    goobers: options.goobers ?? [goober(name, "implementer", ["implement", "review"])],
    workflows: options.workflows ?? [workflowSummary(name, "implementation", 2)],
    connections: [],
  };
}

function activeRun(
  id: string,
  gaggleName: string,
  stage: string | undefined,
  startedAt = "2026-07-18T06:00:00Z",
  workflow = "implementation",
): RunSummary {
  return {
    id,
    workflow,
    workflowVersion: 7,
    workflowDigest: SENSITIVE.digest,
    gaggle: gaggleName,
    trigger: { kind: "item", ref: SENSITIVE.triggerRef },
    phase: "running",
    terminal: false,
    noWork: false,
    currentStage: stage,
    startedAt,
    durationMillis: 120_000,
    lastActivityAt: "2026-07-18T06:02:00Z",
    lastSeq: 6,
    repassCount: 0,
    retryCount: 0,
    policyRetryCount: 0,
    infraRetryCount: 0,
  };
}

function terminalRun(id: string, gaggleName: string, phase: RunSummary["phase"]): RunSummary {
  return {
    ...activeRun(id, gaggleName, undefined),
    phase,
    terminal: true,
    finishedAt: "2026-07-18T05:00:00Z",
  };
}

function details(...list: WorkflowDetail[]): Map<string, WorkflowDetail> {
  return new Map(
    list.map((item) => [laneKey(item.identity.gaggle, item.identity.name), item]),
  );
}

function signals(
  entries: Record<string, FactoryRunSignal>,
): Map<string, FactoryRunSignal> {
  return new Map(Object.entries(entries));
}

function rectanglesOverlap(
  left: { x: number; y: number; width: number; height: number },
  right: { x: number; y: number; width: number; height: number },
): boolean {
  return (
    left.x < right.x + right.width &&
    left.x + left.width > right.x &&
    left.y < right.y + right.height &&
    left.y + left.height > right.y
  );
}

function twoGaggleFloor(
  overrides: Partial<Parameters<typeof buildFactoryFloorModel>[0]> = {},
): FactoryFloorModel {
  return buildFactoryFloorModel({
    inventories: [inventory("core", "Core product"), inventory("tools", "Developer tools")],
    workflowDetails: details(workflowDetail("core"), workflowDetail("tools")),
    activeRuns: [activeRun("01RUNCORE", "core", "implement")],
    runSignals: signals({ "01RUNCORE": { state: "running", confirmed: true } }),
    ...overrides,
  });
}

describe("factory floor entities", () => {
  it("turns the real inventory into distinct gaggles, lines, machines and staff", () => {
    const model = twoGaggleFloor();

    expect(model.gaggles.map((entity) => entity.name)).toEqual(["core", "tools"]);
    expect(model.lanes.map((lane) => lane.id)).toEqual([
      "core/implementation",
      "tools/implementation",
    ]);
    expect(model.lanes[0].displayName).toBe("Implementation");
    // Every declared stage becomes its own machine, on the real topology order.
    expect(model.lanes[0].stations.map((station) => station.stageId)).toEqual([
      "query",
      "implement",
      "review",
    ]);
    expect(model.lanes[0].stations.map((station) => station.column)).toEqual([0, 1, 2]);
    expect(model.lanes[0].stations[2].kind).toBe("gate");
    expect(model.stations).toHaveLength(6);
    expect(model.workers.map((worker) => worker.id)).toEqual([
      "core/implementer",
      "tools/implementer",
    ]);
    expect(model.lanes[0].source).toBe("declared");
    // Real conveyors from real edges, including the review -> implement repass.
    expect(model.lanes[0].conveyors.map((conveyor) => conveyor.kind)).toEqual([
      "forward",
      "forward",
      "terminal",
      "repass",
      "terminal",
    ]);
    expect(model.lanes[0].docks.map((dock) => dock.terminal)).toEqual([
      "complete",
      "escalate",
    ]);
  });

  it("preserves declared parallel branch names on line conveyors", () => {
    const core = workflowDetail("core");
    core.graph.nodes[0] = { id: "query", kind: "parallel" };
    core.graph.edges = [
      { source: "query", target: "implement", branch: "linux" },
      { source: "query", target: "review", branch: "windows" },
      { source: "implement", target: "", outcome: "done", terminal: "complete" },
      { source: "review", target: "@escalate", outcome: "fail", terminal: "escalate" },
    ];

    const model = twoGaggleFloor({
      workflowDetails: details(core, workflowDetail("tools")),
    });

    const branches = model.lanes[0].conveyors.filter((conveyor) => conveyor.branch);
    expect(branches.map((conveyor) => conveyor.branch)).toEqual(["linux", "windows"]);
    expect(new Set(branches.map((conveyor) => conveyor.labelY)).size).toBe(2);
  });

  it("places a goober at an owned stage only while that stage holds work", () => {
    const model = twoGaggleFloor();
    const core = model.workers.find((worker) => worker.id === "core/implementer");
    const tools = model.workers.find((worker) => worker.id === "tools/implementer");

    expect(core?.activeStationIds).toEqual(["core/implementation/implement"]);
    expect(core?.idle).toBe(false);
    expect(core?.placements[0].stationId).toBe("core/implementation/implement");
    // Configured but with nothing on its owned stages: it waits in the commons,
    // never inferred to be busy merely because it exists.
    expect(tools?.idle).toBe(true);
    expect(tools?.activeStationIds).toEqual([]);
    expect(tools?.placements[0].stationId).toBeUndefined();
    expect(model.commons.workerIds).toEqual(["tools/implementer"]);
    expect(model.counts.idleGoobers).toBe(1);
  });

  it("keeps a lane for a workflow whose definition could not be read, using observed stages", () => {
    const model = buildFactoryFloorModel({
      inventories: [inventory("core", "Core product")],
      workflowDetails: new Map(),
      activeRuns: [activeRun("01RUNCORE", "core", "implement")],
    });

    expect(model.lanes[0].source).toBe("observed");
    expect(model.lanes[0].stations.map((station) => station.stageId)).toEqual(["implement"]);
    expect(model.lanes[0].conveyors).toEqual([]);
  });

  it("is deterministic: rebuilding the same snapshot places everything identically", () => {
    const first = twoGaggleFloor();
    const second = twoGaggleFloor();

    expect(second.stations.map((station) => [station.x, station.y])).toEqual(
      first.stations.map((station) => [station.x, station.y]),
    );
    expect(second.carriers.map((carrier) => [carrier.x, carrier.y])).toEqual(
      first.carriers.map((carrier) => [carrier.x, carrier.y]),
    );
    expect([second.width, second.height]).toEqual([first.width, first.height]);
  });
});

describe("work placement and movement", () => {
  it("stands an active run on the stage the daemon reports", () => {
    const model = twoGaggleFloor();
    const carrier = model.carriers[0];
    const station = model.stations.find(
      (candidate) => candidate.id === stationKey("core", "implementation", "implement"),
    );

    expect(model.carriers).toHaveLength(1);
    expect(carrier.runId).toBe("01RUNCORE");
    expect(carrier.stageId).toBe("implement");
    expect(carrier.stationId).toBe("core/implementation/implement");
    expect(station?.runIds).toEqual(["01RUNCORE"]);
    expect(station?.wip).toBe(1);
    // The crate parks on that machine's apron, not somewhere decorative.
    expect(carrier.x).toBeGreaterThanOrEqual(station!.x);
    expect(carrier.y).toBeGreaterThan(station!.y + station!.height);
  });

  it("queues a run with no current stage in the line's inbound yard", () => {
    const model = twoGaggleFloor({
      activeRuns: [activeRun("01RUNCORE", "core", undefined)],
      runSignals: signals({
        "01RUNCORE": { state: "starting", reason: "awaiting-stage", confirmed: true },
      }),
    });

    expect(model.carriers[0].stationId).toBe("core/implementation#yard");
    expect(model.lanes[0].yard.runIds).toEqual(["01RUNCORE"]);
    expect(model.counts.queuedRuns).toBe(1);
  });

  it("marks a real stage transition and moves only the run that moved", () => {
    const before = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNCORE", "core", "implement"),
        activeRun("01RUNHELD", "core", "query", "2026-07-18T05:00:00Z"),
      ],
    });
    const after = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNCORE", "core", "review"),
        activeRun("01RUNHELD", "core", "query", "2026-07-18T05:00:00Z"),
      ],
      previous: before,
    });

    const movedBefore = before.carriers.find((carrier) => carrier.runId === "01RUNCORE")!;
    const movedAfter = after.carriers.find((carrier) => carrier.runId === "01RUNCORE")!;
    const stillBefore = before.carriers.find((carrier) => carrier.runId === "01RUNHELD")!;
    const stillAfter = after.carriers.find((carrier) => carrier.runId === "01RUNHELD")!;

    expect(movedAfter.transition).toEqual({
      kind: "stage-change",
      fromStageId: "implement",
      fromStationId: "core/implementation/implement",
    });
    expect(movedAfter.stationId).toBe("core/implementation/review");
    expect([movedAfter.x, movedAfter.y]).not.toEqual([movedBefore.x, movedBefore.y]);

    // The run that did not move keeps its exact coordinate and gains no
    // transition: a refresh must never make the floor twitch.
    expect(stillAfter.transition).toBeUndefined();
    expect([stillAfter.x, stillAfter.y]).toEqual([stillBefore.x, stillBefore.y]);
    // ... and the conveyor that carried the move is the only one marked active.
    const active = after.lanes[0].conveyors.filter((conveyor) => conveyor.active);
    expect(active).toHaveLength(1);
    expect(active[0].fromStationId).toBe("core/implementation/implement");
    expect(active[0].toId).toBe("core/implementation/review");
  });

  it("treats a newly started run as an arrival rather than a transition", () => {
    const before = twoGaggleFloor({ activeRuns: [] });
    const after = twoGaggleFloor({ previous: before });

    expect(after.carriers[0].transition).toEqual({ kind: "arrival" });
    expect(after.lanes[0].conveyors.some((conveyor) => conveyor.active)).toBe(false);
  });

  it("keeps a survivor in its stable slot when a sibling leaves", () => {
    const before = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNFIRST", "core", "implement", "2026-07-18T05:00:00Z"),
        activeRun("01RUNSURVIVOR", "core", "implement", "2026-07-18T06:00:00Z"),
      ],
    });
    const survivorBefore = before.carriers.find(
      (carrier) => carrier.runId === "01RUNSURVIVOR",
    )!;
    const after = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNSURVIVOR", "core", "implement", "2026-07-18T06:00:00Z"),
      ],
      previous: before,
    });
    const survivorAfter = after.carriers[0];

    expect(survivorAfter.queueIndex).toBe(survivorBefore.queueIndex);
    expect([survivorAfter.x, survivorAfter.y]).toEqual([
      survivorBefore.x,
      survivorBefore.y,
    ]);
    expect(survivorAfter.transition).toBeUndefined();
  });
});

describe("high-density placement", () => {
  it("caps a 50-run stage inside its lane and keeps staff clear of carriers", () => {
    const runs = Array.from({ length: 50 }, (_, index) =>
      activeRun(
        `01DENSE${String(index).padStart(3, "0")}`,
        "core",
        "implement",
        new Date(Date.parse("2026-07-18T05:00:00Z") + index * 1_000).toISOString(),
      ),
    );
    const coreGoobers = Array.from({ length: 8 }, (_, index) =>
      goober("core", `implementer-${index}`, ["implement"]),
    );
    const model = buildFactoryFloorModel({
      inventories: [
        inventory("core", "Core product", { goobers: coreGoobers }),
        inventory("tools", "Developer tools"),
      ],
      workflowDetails: details(workflowDetail("core"), workflowDetail("tools")),
      activeRuns: runs,
      runSignals: new Map(
        runs.map((run) => [run.id, { state: "running" as const, confirmed: true }]),
      ),
    });
    const station = model.stations.find(
      (candidate) => candidate.id === "core/implementation/implement",
    )!;
    const lane = model.lanes.find((candidate) => candidate.id === "core/implementation")!;
    const renderedCarriers = model.carriers.filter((carrier) => carrier.rendered);
    const renderedWorkers = model.workers.flatMap((worker) =>
      worker.placements.filter(
        (placement) => placement.stationId === station.id && placement.rendered,
      ),
    );

    expect(station.wip).toBe(50);
    expect(station.runIds).toHaveLength(50);
    expect(station.renderedRunIds).toHaveLength(
      FACTORY_RENDERED_CARRIERS_PER_STATION,
    );
    expect(station.overflowRunCount).toBe(
      50 - FACTORY_RENDERED_CARRIERS_PER_STATION,
    );
    expect(renderedCarriers).toHaveLength(FACTORY_RENDERED_CARRIERS_PER_STATION);
    expect(station.workerIds).toHaveLength(8);
    expect(station.renderedWorkerIds).toHaveLength(1);
    expect(station.workerOverflowCount).toBe(7);

    for (const carrier of renderedCarriers) {
      expect(carrier.x).toBeGreaterThanOrEqual(station.x);
      expect(carrier.x + 28).toBeLessThanOrEqual(station.x + station.width);
      expect(carrier.y).toBeGreaterThan(station.y + station.height);
      expect(carrier.y + 22).toBeLessThanOrEqual(lane.y + lane.height);
      for (const worker of renderedWorkers) {
        expect(rectanglesOverlap(
          { x: carrier.x, y: carrier.y, width: 28, height: 22 },
          { x: worker.x, y: worker.y, width: 30, height: 34 },
        )).toBe(false);
      }
    }
  });

  it("caps inbound work and idle commons with truthful aggregate counts", () => {
    const inbound = Array.from({ length: 50 }, (_, index) =>
      activeRun(`01QUEUE${String(index).padStart(3, "0")}`, "core", undefined),
    );
    const queued = buildFactoryFloorModel({
      inventories: [inventory("core", "Core product")],
      workflowDetails: details(workflowDetail("core")),
      activeRuns: inbound,
      runSignals: new Map(
        inbound.map((run) => [
          run.id,
          { state: "starting" as const, reason: "awaiting-stage" as const, confirmed: true },
        ]),
      ),
    });
    const yard = queued.lanes[0].yard;
    const renderedInbound = queued.carriers.filter((carrier) => carrier.rendered);

    expect(yard.runIds).toHaveLength(50);
    expect(yard.renderedRunIds).toHaveLength(FACTORY_RENDERED_CARRIERS_PER_YARD);
    expect(yard.overflowRunCount).toBe(50 - FACTORY_RENDERED_CARRIERS_PER_YARD);
    for (const carrier of renderedInbound) {
      expect(carrier.x).toBeGreaterThanOrEqual(yard.x);
      expect(carrier.x + 28).toBeLessThanOrEqual(yard.x + yard.width);
      expect(carrier.y).toBeGreaterThanOrEqual(yard.y);
      expect(carrier.y + 22).toBeLessThanOrEqual(yard.y + yard.height);
    }

    const idleGoobers = Array.from({ length: 30 }, (_, index) =>
      goober("core", `ready-${index}`, ["implement"]),
    );
    const idle = buildFactoryFloorModel({
      inventories: [
        inventory("core", "Core product", {
          goobers: idleGoobers,
        }),
      ],
      workflowDetails: details(workflowDetail("core")),
      activeRuns: [],
    });
    const renderedReady = idle.workers.flatMap((worker) =>
      worker.placements.filter((placement) => placement.rendered),
    );

    expect(idle.commons.workerIds).toHaveLength(30);
    expect(idle.commons.renderedWorkerIds).toHaveLength(
      FACTORY_RENDERED_COMMONS_WORKERS,
    );
    expect(idle.commons.overflowWorkerCount).toBe(
      30 - FACTORY_RENDERED_COMMONS_WORKERS,
    );
    for (const worker of renderedReady) {
      expect(worker.x).toBeGreaterThanOrEqual(idle.commons.x);
      expect(worker.x + 30).toBeLessThanOrEqual(
        idle.commons.x + idle.commons.width,
      );
      expect(worker.y).toBeGreaterThanOrEqual(idle.commons.y);
      expect(worker.y + 34).toBeLessThanOrEqual(
        idle.commons.y + idle.commons.height,
      );
    }
  });
});

describe("blocked semantics", () => {
  it("raises a stage alarm when every run on the stage is blocked or paused", () => {
    const model = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNBLOCK", "core", "implement"),
        activeRun("01RUNGATE", "tools", "review"),
      ],
      runSignals: signals({
        "01RUNBLOCK": { state: "blocked", reason: "stage-blocked", confirmed: true },
        "01RUNGATE": { state: "paused", reason: "human-gate", confirmed: true },
      }),
    });

    const blockedStation = model.stations.find(
      (station) => station.id === "core/implementation/implement",
    );
    const pausedStation = model.stations.find(
      (station) => station.id === "tools/implementation/review",
    );

    expect(blockedStation?.status).toBe("blocked");
    expect(blockedStation?.alarm).toBe("blocked");
    expect(pausedStation?.status).toBe("held");
    expect(pausedStation?.alarm).toBe("hold");
    expect(model.counts.blockedStages).toBe(1);
    expect(model.counts.heldStages).toBe(1);
    expect(model.counts.blockedRuns).toBe(2);
    expect(model.attention.map((item) => item.kind)).toEqual(["blocked-run", "blocked-run"]);
  });

  it("does not raise the alarm when only some work on the stage is held", () => {
    const model = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNBLOCK", "core", "implement"),
        activeRun("01RUNFINE", "core", "implement", "2026-07-18T06:10:00Z"),
      ],
      runSignals: signals({
        "01RUNBLOCK": { state: "blocked", reason: "stage-blocked", confirmed: true },
        "01RUNFINE": { state: "running", confirmed: true },
      }),
    });

    const station = model.stations.find(
      (candidate) => candidate.id === "core/implementation/implement",
    );
    expect(station?.status).toBe("impeded");
    expect(station?.alarm).toBeUndefined();
    expect(station?.blockedCount).toBe(1);
    expect(model.counts.blockedStages).toBe(0);
  });

  it("does not claim every run is held when a signal is unread", () => {
    const model = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNBLOCK", "core", "implement"),
        activeRun("01RUNUNKNOWN", "core", "implement", "2026-07-18T06:10:00Z"),
      ],
      runSignals: signals({
        "01RUNBLOCK": { state: "blocked", reason: "stage-blocked", confirmed: true },
        "01RUNUNKNOWN": { state: "unknown", confirmed: false },
      }),
    });
    const station = model.stations.find(
      (candidate) => candidate.id === "core/implementation/implement",
    );

    expect(station?.status).toBe("unknown");
    expect(station?.blockedCount).toBe(1);
    expect(station?.unknownCount).toBe(1);
    expect(station?.alarm).toBeUndefined();
    expect(model.counts.blockedRuns).toBe(1);
    expect(model.counts.unreadRuns).toBe(1);
    expect(model.counts.blockedStages).toBe(0);
  });

  it("derives blocked from a blocked attempt and paused from gate.paused", () => {
    const blockedAttempt: StageAttempt = {
      id: "a1",
      visit: 1,
      number: 1,
      class: "initial",
      status: "blocked",
      durationMillis: 10,
      artifacts: [],
      error: { code: "blocked_by_agent", message: SENSITIVE.attemptMessage },
    };
    expect(
      deriveRunSignal({
        run: activeRun("01RUNBLOCK", "core", "implement"),
        attempts: [blockedAttempt],
      }),
    ).toEqual({ state: "blocked", reason: "stage-blocked", confirmed: true });

    const pausedEvents: RunEvent[] = [
      event(6, "gate.started", { gate: "review" }),
      event(7, "gate.paused", { gate: "review", reason: SENSITIVE.journalMessage }),
    ];
    expect(
      deriveRunSignal({ run: activeRun("01RUNGATE", "core", "review"), events: pausedEvents }),
    ).toEqual({ state: "paused", reason: "human-gate", confirmed: true });
  });

  it("does not treat a finished failure as a blocked stage", () => {
    const failedAttempt: StageAttempt = {
      id: "a1",
      visit: 1,
      number: 1,
      class: "initial",
      status: "failure",
      durationMillis: 10,
      artifacts: [],
      error: { code: "executor_error", message: SENSITIVE.attemptMessage },
    };
    const signal = deriveRunSignal({
      run: activeRun("01RUNRETRY", "core", "implement"),
      attempts: [failedAttempt],
    });
    expect(signal).toEqual({ state: "running", reason: "attempt-retry", confirmed: true });

    const model = twoGaggleFloor({
      activeRuns: [activeRun("01RUNRETRY", "core", "implement")],
      runSignals: signals({ "01RUNRETRY": signal }),
    });
    const station = model.stations.find(
      (candidate) => candidate.id === "core/implementation/implement",
    );
    expect(station?.status).toBe("running");
    expect(station?.alarm).toBeUndefined();
    expect(model.counts.blockedStages).toBe(0);
  });

  it("resolves a gate that has been evaluated after pausing back to running", () => {
    expect(
      deriveRunSignal({
        run: activeRun("01RUNGATE", "core", "review"),
        events: [
          event(7, "gate.paused", { gate: "review" }),
          event(8, "gate.evaluated", { gate: "review", verdict: "approve" }),
        ],
      }),
    ).toEqual({ state: "running", confirmed: true });
  });

  it("reports an unread stage as unknown rather than guessing", () => {
    expect(deriveRunSignal({ run: activeRun("01RUN", "core", "implement") })).toEqual({
      state: "unknown",
      confirmed: false,
    });
  });

  it("lists terminal failures as recent attention, never as work in progress", () => {
    const model = twoGaggleFloor({
      recentOutcomes: [
        terminalRun("01RUNFAILED", "core", "failed"),
        terminalRun("01RUNESCALATED", "core", "escalated"),
        terminalRun("01RUNDONE", "core", "completed"),
      ],
    });

    expect(model.carriers.map((carrier) => carrier.runId)).toEqual(["01RUNCORE"]);
    // Newest finish first, run id as the deterministic tie-break.
    expect(model.attention.map((item) => item.runId)).toEqual([
      "01RUNESCALATED",
      "01RUNFAILED",
    ]);
    expect(model.attention.every((item) => item.kind === "recent-failure")).toBe(true);
  });
});

describe("capacity", () => {
  it("reports WIP against the workflow's own concurrency limit", () => {
    const model = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNA", "core", "implement"),
        activeRun("01RUNB", "core", "implement", "2026-07-18T06:05:00Z"),
      ],
    });
    const station = model.stations.find(
      (candidate) => candidate.id === "core/implementation/implement",
    );

    expect(station?.wip).toBe(2);
    expect(station?.limit).toBe(2);
    expect(station?.saturation).toBe(1);
    expect(model.lanes[0].limit).toBe(2);
    expect(model.capacity).toEqual({ wip: 2, limit: 4, unknownLimits: 0, saturation: 0.5 });
  });

  it("says unknown instead of inventing a limit the daemon did not report", () => {
    const model = buildFactoryFloorModel({
      inventories: [
        inventory("core", "Core product", {
          workflows: [workflowSummary("core", "implementation", undefined)],
        }),
      ],
      workflowDetails: details(workflowDetail("core", "implementation", undefined)),
      activeRuns: [activeRun("01RUNA", "core", "implement")],
    });
    const station = model.stations.find(
      (candidate) => candidate.id === "core/implementation/implement",
    );

    expect(station?.limit).toBeUndefined();
    expect(station?.saturation).toBeUndefined();
    expect(model.capacity).toEqual({
      wip: 1,
      limit: undefined,
      unknownLimits: 1,
      saturation: undefined,
    });
  });

  it("keeps identical stage ids in separate per-workflow lanes", () => {
    const model = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNCORE", "core", "implement"),
        activeRun("01RUNTOOLS", "tools", "implement"),
      ],
    });
    const stations = model.stations.filter((station) => station.stageId === "implement");

    expect(stations.map((station) => station.id)).toEqual([
      "core/implementation/implement",
      "tools/implementation/implement",
    ]);
    expect(stations.every((station) => station.wip === 1)).toBe(true);
  });

  it("keeps the complete bounded model while marking a truncated population partial", () => {
    const runs = Array.from({ length: 50 }, (_, index) =>
      activeRun(`01BOUND${String(index).padStart(3, "0")}`, "core", "implement"),
    );
    const model = buildFactoryFloorModel({
      inventories: [inventory("core", "Core product")],
      workflowDetails: details(workflowDetail("core")),
      activeRuns: runs,
      runSignals: new Map(
        runs.map((run) => [run.id, { state: "running" as const, confirmed: true }]),
      ),
      runsTruncated: true,
    });

    expect(model.runsTruncated).toBe(true);
    expect(model.counts.activeRuns).toBe(50);
    expect(model.carriers.map((carrier) => carrier.runId)).toHaveLength(50);
    expect(model.capacity.wip).toBe(50);
  });
});

describe("scope", () => {
  it("filters the floor to a selected gaggle and workflow", () => {
    const all = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNCORE", "core", "implement"),
        activeRun("01RUNTOOLS", "tools", "implement"),
      ],
    });
    expect(all.lanes).toHaveLength(2);
    expect(all.counts.activeRuns).toBe(2);
    expect(all.counts.gaggles).toBe(2);

    const scoped = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNCORE", "core", "implement"),
        activeRun("01RUNTOOLS", "tools", "implement"),
      ],
      scope: { gaggle: "tools" },
    });
    expect(scoped.lanes.map((lane) => lane.id)).toEqual(["tools/implementation"]);
    expect(scoped.carriers.map((carrier) => carrier.runId)).toEqual(["01RUNTOOLS"]);
    expect(scoped.counts.gaggles).toBe(1);
    expect(scoped.workers.map((worker) => worker.id)).toEqual(["tools/implementer"]);

    const byWorkflow = buildFactoryFloorModel({
      inventories: [
        inventory("core", "Core product", {
          workflows: [
            workflowSummary("core", "implementation", 2),
            workflowSummary("core", "review-queue", 1),
          ],
        }),
      ],
      workflowDetails: details(
        workflowDetail("core"),
        workflowDetail("core", "review-queue", 1),
      ),
      activeRuns: [
        activeRun("01RUNIMPL", "core", "implement"),
        activeRun("01RUNREVQ", "core", "implement", "2026-07-18T06:01:00Z", "review-queue"),
      ],
      scope: { workflow: "review-queue" },
    });
    expect(byWorkflow.lanes.map((lane) => lane.id)).toEqual(["core/review-queue"]);
    expect(byWorkflow.carriers.map((carrier) => carrier.runId)).toEqual(["01RUNREVQ"]);
  });

  it("drops a scope the daemon does not know rather than showing an empty plant", () => {
    const inventories = [inventory("core", "Core product")];

    expect(validateFactoryScope({ gaggle: "core" }, inventories)).toEqual({
      scope: { gaggle: "core", workflow: undefined },
      dropped: {},
    });
    expect(validateFactoryScope({ gaggle: "ghost" }, inventories)).toEqual({
      scope: { gaggle: undefined, workflow: undefined },
      dropped: { gaggle: "ghost" },
    });
    expect(
      validateFactoryScope({ gaggle: "core", workflow: "ghost-flow" }, inventories),
    ).toEqual({
      scope: { gaggle: "core", workflow: undefined },
      dropped: { workflow: "ghost-flow" },
    });
  });

  it("reports an honest empty reason instead of a blank floor", () => {
    expect(buildFactoryFloorModel({ inventories: [], activeRuns: [] }).emptyReason).toBe(
      "no-gaggles",
    );
    expect(
      buildFactoryFloorModel({
        inventories: [inventory("core", "Core product", { workflows: [] })],
        activeRuns: [],
      }).emptyReason,
    ).toBe("no-workflows");
    expect(twoGaggleFloor({ activeRuns: [] }).emptyReason).toBe("no-active-runs");
  });

  it("keeps configured stage counts when topology was not read in the batch", () => {
    const model = buildFactoryFloorModel({
      inventories: [inventory("core", "Core product")],
      workflowDetails: new Map(),
      activeRuns: [],
    });

    expect(model.lanes[0].stageCount).toBe(3);
    expect(model.lanes[0].stations).toHaveLength(0);
    expect(model.lanes[0].source).toBe("observed");
    expect(model.emptyReason).toBe("no-active-runs");
  });
});

describe("privacy", () => {
  it("never lets free-form or sensitive definition fields into the view model", () => {
    const model = twoGaggleFloor({
      activeRuns: [
        activeRun("01RUNCORE", "core", "implement"),
        activeRun("01RUNGATE", "tools", "review"),
      ],
      runSignals: signals({
        "01RUNCORE": { state: "blocked", reason: "stage-blocked", confirmed: true },
        "01RUNGATE": { state: "paused", reason: "human-gate", confirmed: true },
      }),
      recentOutcomes: [terminalRun("01RUNFAILED", "core", "failed")],
    });

    const serialized = JSON.stringify(model);
    for (const value of Object.values(SENSITIVE)) {
      expect(serialized).not.toContain(value);
    }
    expect(serialized).not.toMatch(/https?:\/\//);
    expect(serialized).not.toContain("skills");
    expect(serialized).not.toContain("capabilities");
    expect(serialized).not.toContain("artifacts");
    expect(serialized).not.toContain("transcript");
    // What IS present is the safe operational vocabulary.
    expect(serialized).toContain("01RUNCORE");
    expect(serialized).toContain("Core product");
    expect(serialized).toContain("implement");
    expect(serialized).toContain("stage-blocked");
  });

  it("keeps the trigger kind but never the trigger reference", () => {
    const model = twoGaggleFloor();
    expect(model.carriers[0].triggerKind).toBe("item");
    expect(JSON.stringify(model.carriers[0])).not.toContain(SENSITIVE.triggerRef);
  });

  it("keeps the frontend model on an explicit safe field whitelist", () => {
    const model = twoGaggleFloor();

    expect(Object.keys(model).sort()).toEqual(
      [
        "attention",
        "capacity",
        "carriers",
        "commons",
        "counts",
        "emptyReason",
        "gaggles",
        "height",
        "lanes",
        "runsTruncated",
        "scope",
        "stations",
        "topologyReadFailures",
        "width",
        "workers",
        "workflows",
      ].sort(),
    );
    expect(Object.keys(model.carriers[0]).sort()).toEqual(
      [
        "confirmed",
        "durationMillis",
        "gaggle",
        "infraRetryCount",
        "laneId",
        "lastActivityAt",
        "ownerWorkerId",
        "phase",
        "policyRetryCount",
        "queueIndex",
        "rendered",
        "renderSlot",
        "reason",
        "repassCount",
        "retryCount",
        "runId",
        "stageId",
        "startedAt",
        "state",
        "stationId",
        "triggerKind",
        "workflow",
        "workflowDisplayName",
        "x",
        "y",
      ].sort(),
    );
    expect(Object.keys(model.workers[0]).sort()).toEqual(
      [
        "activeRunCount",
        "activeStationIds",
        "displayName",
        "gaggle",
        "gaggleDisplayName",
        "harness",
        "id",
        "idle",
        "name",
        "placements",
        "stages",
        "status",
      ].sort(),
    );
  });
});

function event(seq: number, type: RunEvent["type"], fields: Partial<RunEvent>): RunEvent {
  return {
    schema: "v1",
    seq,
    type,
    branch: 0,
    time: new Date(Date.parse("2026-07-18T06:00:00Z") + seq * 1_000).toISOString(),
    knownSchema: true,
    ...fields,
  };
}
