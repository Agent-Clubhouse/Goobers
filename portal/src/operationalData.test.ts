import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type { RequestOptions, RunList, RunListOptions } from "./api/types";
import { DATA_CACHE_TTL_MS, SessionDataCache } from "./dataCache";
import {
  INVENTORY_CACHE_TTL_MS,
  loadOperationalOverview,
  loadOperationalSnapshot,
} from "./operationalData";
import { largeJournalFixtures, populatedDaemonFixtures } from "./test/daemonFixtures";

class ConcurrencyTrackingClient extends FixtureDaemonClient {
  readonly requests: (RunListOptions | undefined)[] = [];
  maxConcurrentRuns = 0;
  private activeRuns = 0;

  override async listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList> {
    this.requests.push(request);
    this.activeRuns += 1;
    this.maxConcurrentRuns = Math.max(this.maxConcurrentRuns, this.activeRuns);
    await Promise.resolve();
    try {
      return await super.listRuns(request, options);
    } finally {
      this.activeRuns -= 1;
    }
  }
}

describe("loadOperationalSnapshot", () => {
  it("fetches a bounded recent window for each workflow and keeps one terminal outcome (#1664)", async () => {
    const client = new FixtureDaemonClient(
      largeJournalFixtures({
        completed: 80,
        running: 2,
        failed: 0,
        escalated: 0,
        aborted: 0,
      }),
    );
    const listRuns = vi.spyOn(client, "listRuns");

    const snapshot = await loadOperationalSnapshot(client);

    expect(listRuns.mock.calls.map(([request]) => request)).toEqual([
      { gaggle: "core", workflow: "implementation", limit: 5 },
      { gaggle: "tools", workflow: "implementation", limit: 5 },
    ]);
    expect(listRuns.mock.calls.every(([request]) => request?.cursor === undefined)).toBe(true);
    expect(snapshot.runs.map((run) => run.id)).toEqual(["01JZTEST000000079"]);
  });

  it("caps concurrent outcome requests with a large workflow inventory (#1679)", async () => {
    const fixtures = populatedDaemonFixtures();
    const coreGaggle = fixtures.gaggles.items.find((gaggle) => gaggle.name === "core");
    const coreGoobers = fixtures.goobers?.core;
    const coreWorkflows = fixtures.workflows?.core;
    const workflowTemplate = coreWorkflows?.items[0];
    if (!coreGaggle || !coreGoobers || !coreWorkflows || !workflowTemplate) {
      throw new Error("Populated fixtures must include the core gaggle inventory.");
    }

    const workflowCount = 20;
    const workflows = Array.from({ length: workflowCount }, (_, index) => ({
      ...workflowTemplate,
      identity: { gaggle: "core", name: `workflow-${index}` },
    }));
    fixtures.gaggles = {
      items: [{ ...coreGaggle, workflowCount }],
      page: { ...fixtures.gaggles.page, total: 1 },
    };
    fixtures.goobers = { core: coreGoobers };
    fixtures.workflows = {
      core: {
        items: workflows,
        page: { ...coreWorkflows.page, total: workflowCount },
      },
    };
    const client = new ConcurrencyTrackingClient(fixtures);

    await loadOperationalSnapshot(client);

    expect(client.requests).toHaveLength(workflowCount);
    expect(client.maxConcurrentRuns).toBe(4);
    expect(
      client.requests.every(
        (request) => request?.limit === 5 && request.cursor === undefined,
      ),
    ).toBe(true);
  });
});

describe("loadOperationalOverview", () => {
  it("issues only bounded, phase-filtered run requests regardless of journal size (DASH-12)", async () => {
    const client = new FixtureDaemonClient(largeJournalFixtures({ completed: 80 }));
    const listRuns = vi.spyOn(client, "listRuns");

    const overview = await loadOperationalOverview(client);

    expect(overview.groups.recent.length).toBeLessThanOrEqual(20);
    expect(overview.groups.active.every((run) => run.phase === "running")).toBe(true);
    const phases = listRuns.mock.calls.map(([request]) => request?.phase);
    expect(phases).toEqual(
      expect.arrayContaining(["running", "escalated", "failed", "completed", "aborted"]),
    );
    // No request paginates the journal.
    expect(listRuns.mock.calls.every(([request]) => request?.cursor === undefined)).toBe(true);
  });

  it("reuses cached inventory when only the run model is invalidated (DASH-13)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listWorkflows = vi.spyOn(client, "listWorkflows");

    const previous = await loadOperationalOverview(client);
    expect(listGaggles).toHaveBeenCalled();
    listGaggles.mockClear();
    listWorkflows.mockClear();
    const listRuns = vi.spyOn(client, "listRuns");

    const refreshed = await loadOperationalOverview(client, undefined, {
      previous,
      models: new Set(["run"]),
    });

    // Inventory is not re-paged for a run-only change...
    expect(listGaggles).not.toHaveBeenCalled();
    expect(listWorkflows).not.toHaveBeenCalled();
    expect(refreshed.workflowNames).toBe(previous.workflowNames);
    // ...but the bounded run groups are still refetched without pagination.
    expect(listRuns).toHaveBeenCalled();
    expect(listRuns.mock.calls.every(([request]) => request?.cursor === undefined)).toBe(true);
  });

  // The #1708 incident shape: /api/v1/runs hung while health, instance and the
  // inventory all returned 200 — and the operator saw a blank "Daemon
  // unavailable" page instead of the health data telling them what was wrong.
  it("still renders health, instance and inventory when the run queries fail (#1709)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(client, "listRuns").mockRejectedValue(
      new Error("The daemon request timed out after 10000ms."),
    );

    const overview = await loadOperationalOverview(client);

    expect(overview.health).toBeDefined();
    expect(overview.instance).toBeDefined();
    expect(overview.gaggleCount).toBeGreaterThan(0);
    expect(overview.workflowNames.size).toBeGreaterThan(0);
    // The run groups are reported as failed rather than silently shown empty.
    expect(overview.sectionErrors?.runs).toBeInstanceOf(Error);
    expect(overview.groups).toEqual({ active: [], attention: [], recent: [] });
  });

  it("keeps gaggle and workflow inventory available when a shared goober load fails", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const cache = new SessionDataCache();
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listGoobers = vi
      .spyOn(client, "listGoobers")
      .mockRejectedValue(new Error("goober inventory unavailable"));
    const listWorkflows = vi.spyOn(client, "listWorkflows");

    try {
      const [snapshot, overview] = await Promise.allSettled([
        loadOperationalSnapshot(client, undefined, cache),
        loadOperationalOverview(client, undefined, { cache }),
      ]);

      expect(snapshot.status).toBe("rejected");
      expect(overview.status).toBe("fulfilled");
      if (overview.status !== "fulfilled") {
        throw overview.reason;
      }
      expect(listGaggles).toHaveBeenCalledOnce();
      expect(listGoobers).toHaveBeenCalledTimes(2);
      expect(listWorkflows).toHaveBeenCalledTimes(2);
      expect(overview.value.gaggleCount).toBe(2);
      expect(overview.value.workflowNames.size).toBe(2);
      expect(overview.value.sectionErrors?.inventory).toBeUndefined();
    } finally {
      cache.dispose();
    }
  });

  it("keeps the other four phase groups when a single phase query fails (#1709)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const real = client.listRuns.bind(client);
    vi.spyOn(client, "listRuns").mockImplementation(async (request, options) => {
      if (request?.phase === "completed") {
        throw new Error("The daemon request timed out after 10000ms.");
      }
      return real(request, options);
    });

    const overview = await loadOperationalOverview(client);

    // A failing "completed" page must not void "running"/"escalated"/"failed".
    expect(overview.sectionErrors?.runs).toBeUndefined();
    expect(overview.groups.active.every((run) => run.phase === "running")).toBe(true);
    expect(overview.groups.active.length).toBeGreaterThan(0);
  });

  it("preserves the previous run groups when a refresh's run queries fail (#1709)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const previous = await loadOperationalOverview(client);
    expect(previous.groups.active.length).toBeGreaterThan(0);
    vi.spyOn(client, "listRuns").mockRejectedValue(new Error("daemon unavailable"));

    const refreshed = await loadOperationalOverview(client, undefined, {
      previous,
      models: new Set(["run"]),
    });

    expect(refreshed.groups).toEqual(previous.groups);
    expect(refreshed.sectionErrors?.runs).toBeInstanceOf(Error);
  });

  it("still fails the whole load when nothing at all could be read (#1709)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const boom = new Error("daemon unavailable");
    vi.spyOn(client, "getHealth").mockRejectedValue(boom);
    vi.spyOn(client, "getInstance").mockRejectedValue(boom);
    vi.spyOn(client, "listRuns").mockRejectedValue(boom);

    await expect(loadOperationalOverview(client)).rejects.toThrow("daemon unavailable");
  });

  it("refetches inventory when the workflow model changes (DASH-13)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const previous = await loadOperationalOverview(client);
    const listWorkflows = vi.spyOn(client, "listWorkflows");

    await loadOperationalOverview(client, undefined, {
      previous,
      models: new Set(["workflow"]),
    });

    expect(listWorkflows).toHaveBeenCalled();
  });
});

describe("operational inventory cache", () => {
  it("shares inventory across page loaders until the longer inventory TTL expires", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(1_000);
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const cache = new SessionDataCache();
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listGoobers = vi.spyOn(client, "listGoobers");
    const listWorkflows = vi.spyOn(client, "listWorkflows");

    try {
      await loadOperationalOverview(client, undefined, { cache });
      expect(INVENTORY_CACHE_TTL_MS).toBeGreaterThan(DATA_CACHE_TTL_MS);
      listGaggles.mockClear();
      listGoobers.mockClear();
      listWorkflows.mockClear();

      vi.setSystemTime(1_000 + DATA_CACHE_TTL_MS + 1);
      await loadOperationalSnapshot(client, undefined, cache);

      expect(listGaggles).not.toHaveBeenCalled();
      expect(listGoobers).toHaveBeenCalledTimes(2);
      expect(listWorkflows).not.toHaveBeenCalled();

      vi.setSystemTime(1_000 + INVENTORY_CACHE_TTL_MS);
      await loadOperationalOverview(client, undefined, { cache });

      expect(listGaggles).toHaveBeenCalledOnce();
      expect(listGoobers).toHaveBeenCalledTimes(2);
      expect(listWorkflows).toHaveBeenCalledTimes(2);
    } finally {
      cache.dispose();
      vi.useRealTimers();
    }
  });
});
