import { describe, expect, it, vi } from "vitest";
import type {
  AttemptList,
  RequestOptions,
  RunSummary,
  WorkflowDetail,
  WorkflowSummary,
} from "./api/types";
import { FixtureDaemonClient, fixtureKey, type DaemonFixtures } from "./api/fixtureClient";
import {
  FACTORY_RUN_DETAIL_CONCURRENCY,
  factoryWorkflowKeys,
  loadFactoryDetail,
} from "./factoryData";
import { buildFactoryFloorModel, laneKey } from "./factoryModel";
import type { GaggleInventory } from "./operationalData";
import { factoryFloorFixtures } from "./test/daemonFixtures";

describe("factory detail reads", () => {
  it("reads a signal for every visible active run with bounded concurrency", async () => {
    const fixtures = manyRunFixtures(32);
    const client = new ConcurrencyClient(fixtures);

    const detail = await loadFactoryDetail(
      client,
      {},
      ["core/implementation"],
    );

    expect(detail.activeRuns).toHaveLength(32);
    expect(detail.runSignals).toHaveLength(32);
    expect(client.started).toBe(32);
    expect(client.maximumActive).toBeLessThanOrEqual(
      FACTORY_RUN_DETAIL_CONCURRENCY,
    );
    expect(client.maximumActive).toBeGreaterThan(1);
    expect(
      [...detail.runSignals.values()].every(
        (signal) => signal.confirmed && signal.state === "running",
      ),
    ).toBe(true);
  });

  it("marks a failed signal read unknown instead of running", async () => {
    const fixtures = manyRunFixtures(30);
    const unreadRun = fixtures.runs.runs[27].id;
    const client = new ConcurrencyClient(fixtures, unreadRun);

    const detail = await loadFactoryDetail(
      client,
      {},
      ["core/implementation"],
    );

    expect(detail.runSignals.get(unreadRun)).toEqual({
      state: "unknown",
      confirmed: false,
    });
    expect(detail.runSignals).toHaveLength(30);
  });

  it("states when the active population exceeds the 50-run floor bound", async () => {
    const client = new ConcurrencyClient(manyRunFixtures(55));

    const detail = await loadFactoryDetail(
      client,
      {},
      ["core/implementation"],
    );

    expect(detail.activeRuns).toHaveLength(50);
    expect(detail.runSignals).toHaveLength(50);
    expect(detail.runsTruncated).toBe(true);
    expect(client.started).toBe(50);
  });

  it("cancels the bounded signal queue without starting the remaining reads", async () => {
    const fixtures = manyRunFixtures(30);
    const client = new ConcurrencyClient(fixtures, undefined, 50);
    const controller = new AbortController();

    const pending = loadFactoryDetail(
      client,
      {},
      ["core/implementation"],
      controller.signal,
    );
    await waitUntil(() => client.started > 0);
    controller.abort();

    await expect(pending).rejects.toThrow();
    expect(client.started).toBeLessThanOrEqual(FACTORY_RUN_DETAIL_CONCURRENCY);
  });
});

describe("factory workflow detail selection", () => {
  it("is stable across live counter changes and prioritizes explicit scope", () => {
    const { inventory } = manyWorkflowFixtures(14);
    const first = factoryWorkflowKeys([inventory]);
    const changed: GaggleInventory = {
      ...inventory,
      workflows: inventory.workflows.map((workflow, index) => ({
        ...workflow,
        concurrency: {
          ...workflow.concurrency,
          activeRuns: index === 13 ? 50 : 0,
        },
      })),
    };

    expect(factoryWorkflowKeys([changed])).toEqual(first);
    expect(first).toHaveLength(12);
    expect(first).not.toContain("core/flow-13");
    expect(
      factoryWorkflowKeys(
        [changed],
        { gaggle: "core", workflow: "flow-13" },
      ),
    ).toEqual(["core/flow-13"]);
  });

  it("retains a previously read topology when it is outside a later batch", async () => {
    const { fixtures, inventory } = manyWorkflowFixtures(14);
    const client = new FixtureDaemonClient(fixtures);
    const selected = await loadFactoryDetail(
      client,
      { gaggle: "core", workflow: "flow-13" },
      ["core/flow-13"],
    );
    const later = await loadFactoryDetail(
      client,
      {},
      factoryWorkflowKeys([inventory]),
      undefined,
      selected.workflowDetails,
    );
    const run = syntheticRun("01FLOW13", "flow-13", "implement");
    const model = buildFactoryFloorModel({
      inventories: [inventory],
      workflowDetails: later.workflowDetails,
      activeRuns: [run],
      runSignals: new Map([
        [run.id, { state: "running", confirmed: true }],
      ]),
      scope: { workflow: "flow-13" },
    });

    expect(later.workflowDetails.has("core/flow-13")).toBe(true);
    expect(model.lanes[0].source).toBe("declared");
    expect(model.lanes[0].stations.map((station) => station.stageId)).toEqual([
      "query",
      "implement",
      "review",
    ]);
  });

  it("reserves the 12-detail batch for active workflows in stable identity order", async () => {
    const { fixtures, inventory } = manyWorkflowFixtures(14);
    const runs = [
      syntheticRun("01FLOW13", "flow-13", "implement"),
      syntheticRun("01FLOW05", "flow-05", "implement"),
    ];
    fixtures.runs = { runs };
    const attemptTemplate =
      factoryFloorFixtures().stageAttempts?.[
        fixtureKey("01JZ500BLOCKED", "implement")
      ]!;
    fixtures.stageAttempts = Object.fromEntries(
      runs.map((run) => [
        fixtureKey(run.id, "implement"),
        {
          ...structuredClone(attemptTemplate),
          runId: run.id,
          attempts: attemptTemplate.attempts.map((attempt) => ({
            ...attempt,
            id: `${run.id}-attempt`,
            status: "running" as const,
            error: undefined,
          })),
        },
      ]),
    );
    const client = new FixtureDaemonClient(fixtures);
    const getWorkflow = vi.spyOn(client, "getWorkflow");

    const detail = await loadFactoryDetail(
      client,
      {},
      factoryWorkflowKeys([inventory], {}, 100),
    );
    const requested = getWorkflow.mock.calls.map(([gaggle, workflow]) =>
      laneKey(gaggle, workflow),
    );

    expect(requested).toHaveLength(12);
    expect(requested.slice(0, 2)).toEqual(["core/flow-05", "core/flow-13"]);
    expect(requested.slice(2)).toEqual([
      "core/flow-00",
      "core/flow-01",
      "core/flow-02",
      "core/flow-03",
      "core/flow-04",
      "core/flow-06",
      "core/flow-07",
      "core/flow-08",
      "core/flow-09",
      "core/flow-10",
    ]);
    expect(detail.workflowDetails.has("core/flow-13")).toBe(true);
    expect(detail.workflowDetails.has("core/flow-11")).toBe(false);
  });
});

class ConcurrencyClient extends FixtureDaemonClient {
  active = 0;
  maximumActive = 0;
  started = 0;

  constructor(
    fixtures: DaemonFixtures,
    private readonly failRun?: string,
    private readonly delayMillis = 3,
  ) {
    super(fixtures);
  }

  override async listStageAttempts(
    runId: string,
    stage: string,
    options?: RequestOptions,
  ): Promise<AttemptList> {
    this.started += 1;
    this.active += 1;
    this.maximumActive = Math.max(this.maximumActive, this.active);
    try {
      await abortableDelay(this.delayMillis, options?.signal);
      if (runId === this.failRun) {
        throw new Error("synthetic signal read failure");
      }
      return await super.listStageAttempts(runId, stage, options);
    } finally {
      this.active -= 1;
    }
  }
}

function manyRunFixtures(count: number): DaemonFixtures {
  const fixtures = factoryFloorFixtures();
  const template = fixtures.runs.runs.find(
    (run) => run.gaggle === "core" && run.currentStage === "implement",
  )!;
  const attemptTemplate =
    fixtures.stageAttempts?.[fixtureKey("01JZ500BLOCKED", "implement")];
  const runs = Array.from({ length: count }, (_, index) => ({
    ...template,
    id: `01ACTIVE${String(index).padStart(3, "0")}`,
    startedAt: new Date(Date.parse(template.startedAt) + index * 1_000).toISOString(),
    lastActivityAt: new Date(
      Date.parse(template.lastActivityAt) + index * 1_000,
    ).toISOString(),
  }));
  fixtures.runs = { runs };
  fixtures.stageAttempts = Object.fromEntries(
    runs.map((run) => [
      fixtureKey(run.id, "implement"),
      {
        ...attemptTemplate!,
        runId: run.id,
        attempts: attemptTemplate!.attempts.map((attempt) => ({
          ...attempt,
          id: `${run.id}-attempt`,
          status: "running" as const,
          error: undefined,
        })),
      },
    ]),
  );
  return fixtures;
}

function manyWorkflowFixtures(count: number): {
  fixtures: DaemonFixtures;
  inventory: GaggleInventory;
} {
  const fixtures = factoryFloorFixtures();
  const summaryTemplate = fixtures.workflows!.core.items[0];
  const detailTemplate =
    fixtures.workflowDetails![fixtureKey("core", "implementation")];
  const summaries = Array.from({ length: count }, (_, index) =>
    cloneWorkflowSummary(summaryTemplate, index),
  );
  fixtures.workflows!.core = {
    ...fixtures.workflows!.core,
    items: summaries,
  };
  fixtures.workflowDetails = Object.fromEntries(
    summaries.map((summary, index) => {
      const detail = cloneWorkflowDetail(detailTemplate, summary, index);
      return [fixtureKey("core", summary.identity.name), detail];
    }),
  );
  fixtures.runs = { runs: [] };
  const inventory: GaggleInventory = {
    gaggle: fixtures.gaggles.items.find((gaggle) => gaggle.name === "core")!,
    goobers: fixtures.goobers!.core.items,
    workflows: summaries,
    connections: fixtures.connections!.core.repositories,
  };
  return { fixtures, inventory };
}

function cloneWorkflowSummary(
  template: WorkflowSummary,
  index: number,
): WorkflowSummary {
  const name = `flow-${String(index).padStart(2, "0")}`;
  return {
    ...structuredClone(template),
    identity: { gaggle: "core", name },
    displayName: `Flow ${String(index).padStart(2, "0")}`,
    concurrency: {
      ...template.concurrency,
      activeRuns: index % 3,
    },
  };
}

function cloneWorkflowDetail(
  template: WorkflowDetail,
  summary: WorkflowSummary,
  index: number,
): WorkflowDetail {
  return {
    ...structuredClone(template),
    ...summary,
    graph: {
      ...structuredClone(template.graph),
      name: summary.identity.name,
      version: index + 1,
    },
  };
}

function syntheticRun(
  id: string,
  workflow: string,
  stage: string,
): RunSummary {
  const template = factoryFloorFixtures().runs.runs.find(
    (run) => run.gaggle === "core" && run.currentStage === "implement",
  )!;
  return {
    ...template,
    id,
    workflow,
    currentStage: stage,
  };
}

function abortableDelay(
  milliseconds: number,
  signal: AbortSignal | undefined,
): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(resolve, milliseconds);
    const abort = () => {
      clearTimeout(timer);
      reject(new Error("cancelled"));
    };
    if (signal?.aborted) {
      abort();
      return;
    }
    signal?.addEventListener("abort", abort, { once: true });
  });
}

async function waitUntil(predicate: () => boolean): Promise<void> {
  const deadline = Date.now() + 1_000;
  while (!predicate()) {
    if (Date.now() >= deadline) {
      throw new Error("Timed out waiting for factory reads.");
    }
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
}
