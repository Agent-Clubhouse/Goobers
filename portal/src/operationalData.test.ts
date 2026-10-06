import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import type { RunSummary, UpdateModel } from "./api/types";
import { DATA_CACHE_TTL_MS, SessionDataCache } from "./dataCache";
import {
  INVENTORY_CACHE_TTL_MS,
  attentionSeverity,
  incompleteRunPhasesMessage,
  loadOperationalOverview,
  loadOperationalSnapshot,
} from "./operationalData";
import {
  emptyDaemonFixtures,
  largeJournalFixtures,
  populatedDaemonFixtures,
} from "./test/daemonFixtures";

describe("loadOperationalSnapshot", () => {
  it("fetches latest workflow outcomes in one request regardless of workflow count", async () => {
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

    expect(listRuns).toHaveBeenCalledOnce();
    expect(listRuns).toHaveBeenCalledWith(
      { gaggle: undefined, workflow: undefined, latestPerWorkflow: true },
      { signal: undefined },
    );
    expect(snapshot.runs.map((run) => run.id)).toEqual(["01JZTEST000000079"]);
  });

  it("does not grow run request count with a large workflow inventory", async () => {
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
    const client = new FixtureDaemonClient(fixtures);
    const listRuns = vi.spyOn(client, "listRuns");

    await loadOperationalSnapshot(client);

    expect(listRuns).toHaveBeenCalledOnce();
  });

  it("loads only the requested gaggle's subordinate inventory and outcomes", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listGoobers = vi.spyOn(client, "listGoobers");
    const listWorkflows = vi.spyOn(client, "listWorkflows");
    const listRuns = vi.spyOn(client, "listRuns");

    const snapshot = await loadOperationalSnapshot(client, undefined, {
      scope: { gaggle: "core" },
    });

    expect(snapshot.inventories.map(({ gaggle }) => gaggle.name)).toEqual(["core"]);
    expect(listGoobers.mock.calls.map(([gaggle]) => gaggle)).toEqual(["core"]);
    expect(listWorkflows.mock.calls.map(([gaggle]) => gaggle)).toEqual(["core"]);
    expect(listRuns).toHaveBeenCalledWith(
      { gaggle: "core", workflow: undefined, latestPerWorkflow: true },
      { signal: undefined },
    );
    expect(snapshot.runs.every((run) => run.gaggle === "core")).toBe(true);
  });

  it("keeps gaggle activity authoritative when an active workflow definition is absent", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listRuns = client.listRuns.bind(client);
    vi.spyOn(client, "listRuns").mockImplementation(async (request, options) => {
      const response = await listRuns(request, options);
      return {
        ...response,
        workflowActivity: [
          ...(response.workflowActivity ?? []),
          { gaggle: "core", workflow: "removed-workflow", activeRuns: 1 },
        ],
      };
    });

    const snapshot = await loadOperationalSnapshot(client);
    const core = snapshot.inventories.find(({ gaggle }) => gaggle.name === "core");

    expect(core?.gaggle.activeRunCount).toBe(2);
    expect(core?.workflows[0].concurrency.activeRuns).toBe(1);
  });

  it.each([
    {
      model: "run" as UpdateModel,
      expected: { gaggles: 0, goobers: 0, workflows: 0, runs: 1 },
    },
    {
      model: "workflow" as UpdateModel,
      expected: { gaggles: 1, goobers: 2, workflows: 2, runs: 0 },
    },
    {
      model: "instance" as UpdateModel,
      expected: { gaggles: 1, goobers: 2, workflows: 2, runs: 0 },
    },
  ])("issues only the requests required by a $model invalidation", async ({ model, expected }) => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const previous = await loadOperationalSnapshot(client);
    const getHealth = vi.spyOn(client, "getHealth");
    const getInstance = vi.spyOn(client, "getInstance");
    const listGaggles = vi.spyOn(client, "listGaggles");
    const listGoobers = vi.spyOn(client, "listGoobers");
    const listWorkflows = vi.spyOn(client, "listWorkflows");
    const listRuns = vi.spyOn(client, "listRuns");

    await loadOperationalSnapshot(client, undefined, {
      previous,
      models: new Set([model]),
    });

    expect(getHealth).toHaveBeenCalledOnce();
    expect(getInstance).toHaveBeenCalledOnce();
    expect(listGaggles).toHaveBeenCalledTimes(expected.gaggles);
    expect(listGoobers).toHaveBeenCalledTimes(expected.goobers);
    expect(listWorkflows).toHaveBeenCalledTimes(expected.workflows);
    expect(listRuns).toHaveBeenCalledTimes(expected.runs);
  });

  it("does not publish loading sections over a retained snapshot during refresh", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const previous = await loadOperationalSnapshot(client);
    const partials: typeof previous[] = [];

    await loadOperationalSnapshot(client, undefined, {
      previous,
      models: new Set(["run"]),
      onPartial: (partial) => partials.push(partial),
    });

    expect(partials).toHaveLength(1);
    expect(partials[0]?.loadingSections).toBeUndefined();
    expect(partials[0]?.runs).toEqual(previous.runs);
  });
});

describe("loadOperationalOverview", () => {
  it("uses recovery-safe readiness when scheduler-backed instance data is gated", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.health.ready = false;
    fixtures.health.healthy = true;
    fixtures.health.startup = {
      phase: "crash-resume",
      target: "candidates=6",
      since: "2026-10-02T16:00:00Z",
      elapsedSeconds: 120,
      budgetSeconds: 60,
      budgetState: "exceeded",
      blockingCandidate: {
        progress: { total: 6, examined: 6, resumed: 5, reattached: 0, terminal: 0, skipped: 0 },
        runId: "c423d482",
        gaggle: "goobers",
        workflow: "implement",
        disposition: "resolving-generation",
        operation: "resolve execution generation",
        lastProgressAt: "2026-10-02T16:01:50Z",
      },
    };
    fixtures.readiness = {
      apiVersion: fixtures.instance.apiVersion,
      schemaVersion: fixtures.instance.schemaVersion,
      computerName: fixtures.instance.computerName,
      instanceRoot: fixtures.instance.instanceRoot,
      rootIdentity: fixtures.instance.rootIdentity,
      ready: false,
      recovery: fixtures.health.startup,
    };
    class RecoveryClient extends FixtureDaemonClient {
      getInstance(): Promise<never> {
        return Promise.reject(new Error("scheduler not ready"));
      }
    }

    const overview = await loadOperationalOverview(new RecoveryClient(fixtures));

    expect(overview.instance.status).toBe("starting");
    expect(overview.health.startup?.blockingCandidate?.runId).toBe("c423d482");
    expect(overview.sectionErrors?.instance).toBeUndefined();
  });

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

  it.each(["scheduler", "runner"])("moves %s-stale running work into needs attention", async (source) => {
    const fixtures = populatedDaemonFixtures();
    const running = fixtures.runs.runs.find((run) => run.phase === "running");
    if (!running) {
      throw new Error("Populated fixtures must include a running run.");
    }
    running.stale = source === "scheduler";
    if (source === "runner") {
      if (!running.operator) {
        throw new Error("Populated running fixture must include operator status.");
      }
      running.operator.liveness = "stale";
    }

    const overview = await loadOperationalOverview(new FixtureDaemonClient(fixtures));

    expect(overview.groups.active.map((run) => run.id)).not.toContain(running.id);
    expect(overview.groups.attention.map((run) => run.id)).toContain(running.id);
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

  it("does not publish loading sections over a retained overview during refresh", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const previous = await loadOperationalOverview(client);
    const partials: typeof previous[] = [];

    await loadOperationalOverview(client, undefined, {
      previous,
      models: new Set(["run"]),
      onPartial: (partial) => partials.push(partial),
    });

    expect(partials).toHaveLength(1);
    expect(partials[0]?.loadingSections).toBeUndefined();
    expect(partials[0]?.groups).toEqual(previous.groups);
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
        loadOperationalSnapshot(client, undefined, { cache }),
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

  // #3658: the surviving phases are real data, but the phase that failed was
  // silently rendered as an empty group — indistinguishable from a gaggle with
  // nothing in that phase.
  it("names the phases it could not read instead of reporting them empty (#3658)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const real = client.listRuns.bind(client);
    vi.spyOn(client, "listRuns").mockImplementation(async (request, options) => {
      if (request?.phase === "completed") {
        throw new Error("The daemon request timed out after 10000ms.");
      }
      return real(request, options);
    });

    const overview = await loadOperationalOverview(client);

    expect(overview.sectionErrors?.runs).toBeUndefined();
    expect(overview.groups.incomplete?.phases).toEqual(["completed"]);
    expect(overview.groups.incomplete?.error.message).toContain("timed out");
    expect(incompleteRunPhasesMessage(overview.groups.incomplete!)).toContain("completed");
  });

  it("reports no incomplete phases when every phase query succeeds (#3658)", async () => {
    const overview = await loadOperationalOverview(
      new FixtureDaemonClient(populatedDaemonFixtures()),
    );

    expect(overview.groups.incomplete).toBeUndefined();
  });

  it("keeps the previous runs of a phase that fails on refresh (#3658)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const previous = await loadOperationalOverview(client);
    const previousCompleted = previous.groups.recent.filter((run) => run.phase === "completed");
    expect(previousCompleted.length).toBeGreaterThan(0);
    const real = client.listRuns.bind(client);
    vi.spyOn(client, "listRuns").mockImplementation(async (request, options) => {
      if (request?.phase === "completed") {
        throw new Error("daemon unavailable");
      }
      return real(request, options);
    });

    const refreshed = await loadOperationalOverview(client, undefined, {
      previous,
      models: new Set(["run"]),
    });

    expect(refreshed.groups.recent.filter((run) => run.phase === "completed")).toEqual(
      previousCompleted,
    );
    expect(refreshed.groups.incomplete?.phases).toEqual(["completed"]);
    // The phases that did read successfully are still refreshed, not frozen.
    expect(refreshed.groups.active.length).toBe(previous.groups.active.length);
  });

  it("still fails the whole load when nothing at all could be read (#1709)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const boom = new Error("daemon unavailable");
    vi.spyOn(client, "getHealth").mockRejectedValue(boom);
    vi.spyOn(client, "getInstance").mockRejectedValue(boom);
    vi.spyOn(client, "listRuns").mockRejectedValue(boom);

    await expect(loadOperationalOverview(client)).rejects.toThrow("daemon unavailable");
  });

  // #3659: a failed health/instance refresh fell back to the previous data
  // with no error or stale-state indicator, so a degraded daemon kept
  // reading as healthy on the Overview page.
  it("reports a section error when a health refresh falls back to previous data (#3659)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const previous = await loadOperationalOverview(client);
    const boom = new Error("The daemon request timed out after 10000ms.");
    vi.spyOn(client, "getHealth").mockRejectedValue(boom);

    const refreshed = await loadOperationalOverview(client, undefined, { previous });

    expect(refreshed.health).toEqual(previous.health);
    expect(refreshed.sectionErrors?.health).toBe(boom);
    expect(refreshed.sectionErrors?.instance).toBeUndefined();
  });

  it("reports a section error when an instance refresh falls back to previous data (#3659)", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const previous = await loadOperationalOverview(client);
    const boom = new Error("The daemon request timed out after 10000ms.");
    vi.spyOn(client, "getInstance").mockRejectedValue(boom);

    const refreshed = await loadOperationalOverview(client, undefined, { previous });

    expect(refreshed.instance).toEqual(previous.instance);
    expect(refreshed.sectionErrors?.instance).toBe(boom);
    expect(refreshed.sectionErrors?.health).toBeUndefined();
  });

  it("does not report a health/instance section error on a clean refresh", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const overview = await loadOperationalOverview(client);

    expect(overview.sectionErrors?.health).toBeUndefined();
    expect(overview.sectionErrors?.instance).toBeUndefined();
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
      await loadOperationalSnapshot(client, undefined, { cache });

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

// #1199: escalation is a permanent terminal phase with no time filter, so a
// single old, still-unresolved escalation used to sit on the attention list
// forever until 20 newer failures/escalations pushed it off. The fix filters
// and orders escalated/failed candidates by last journal activity (not
// start) within a 24h window, before the existing count cap applies.
describe("loadOperationalOverview attention recency window (#1199)", () => {
  const NOW = Date.parse("2026-08-01T12:00:00Z");
  const STALE_STARTED_AT = "2026-01-01T00:00:00Z";

  function attentionRun(
    id: string,
    phase: "escalated" | "failed",
    lastActivityAt: string,
  ): RunSummary {
    return {
      id,
      workflow: "implementation",
      workflowVersion: 7,
      gaggle: "core",
      trigger: { kind: "item", ref: id.slice(-3) },
      phase,
      terminal: true,
      startedAt: STALE_STARTED_AT,
      finishedAt: lastActivityAt,
      durationMillis: Date.parse(lastActivityAt) - Date.parse(STALE_STARTED_AT),
      lastActivityAt,
      stale: false,
      lastSeq: 1,
      repassCount: 0,
      retryCount: 0,
      policyRetryCount: 0,
      infraRetryCount: 0,
      noWork: false,
    };
  }

  it.each(["running", "completed"] as const)(
    "classifies a failed item as FYI when a newer attempt is %s",
    (phase) => {
      const failed = attentionRun("failed-attempt", "failed", "2026-08-01T10:00:00Z");
      failed.trigger.ref = "6487";
      failed.startedAt = "2026-08-01T09:00:00Z";
      failed.terminalReason = "ISSUE_OVER_SCOPE";
      const later: RunSummary = {
        ...attentionRun(
          "later-attempt",
          phase === "running" ? "failed" : "escalated",
          "2026-08-01T11:00:00Z",
        ),
        phase,
        terminal: phase === "completed",
        trigger: { kind: "item", ref: "6487" },
        startedAt: "2026-08-01T10:30:00Z",
        finishedAt: phase === "completed" ? "2026-08-01T11:00:00Z" : undefined,
      };

      expect(attentionSeverity(failed, [failed, later])).toBe("warning");
    },
  );

  it("keeps an unrecovered non-retryable failure action-required", () => {
    const failed = attentionRun("unrecovered", "failed", "2026-08-01T10:00:00Z");
    failed.trigger.ref = "6487";
    failed.terminalReason = "ISSUE_OVER_SCOPE";

    expect(
      attentionSeverity(failed, [failed], {
        code: "ISSUE_OVER_SCOPE",
        errorClass: "item-judgment",
        message: "The item needs decomposition.",
      }),
    ).toBe("blocked");
  });

  it.each(["non-retryable", "not retryable"])(
    "keeps an unrecovered failure described as %s action-required",
    (description) => {
      const failed = attentionRun("unrecovered", "failed", "2026-08-01T10:00:00Z");

      expect(
        attentionSeverity(failed, [failed], {
          code: "provider.failure",
          errorClass: "infrastructure",
          message: `This failure is ${description}.`,
        }),
      ).toBe("blocked");
    },
  );

  it("classifies an explicitly self-healing failure reason as FYI", () => {
    const failed = attentionRun("retryable", "failed", "2026-08-01T10:00:00Z");

    expect(
      attentionSeverity(failed, [failed], {
        code: "provider.rate_limit",
        errorClass: "retryable-infrastructure",
        message: "A retry is scheduled after the provider quota resets.",
      }),
    ).toBe("warning");
  });

  it("keeps a non-retryable failure blocked when successor history is truncated", async () => {
    const fixtures = emptyDaemonFixtures();
    const failed = attentionRun(
      "failed-before-busy-completions",
      "failed",
      new Date(NOW - 30 * 60_000).toISOString(),
    );
    failed.trigger.ref = "6487";
    failed.startedAt = new Date(NOW - 60 * 60_000).toISOString();
    failed.terminalReason = "ISSUE_OVER_SCOPE";
    const recovered: RunSummary = {
      ...attentionRun(
        "successful-successor",
        "failed",
        new Date(NOW - 20 * 60_000).toISOString(),
      ),
      phase: "completed",
      trigger: { kind: "item", ref: "6487" },
      startedAt: new Date(NOW - 25 * 60_000).toISOString(),
    };
    const unrelated = Array.from({ length: 21 }, (_, index): RunSummary => {
      const finishedAt = new Date(NOW - index * 30_000).toISOString();
      return {
        ...attentionRun(`unrelated-completion-${index}`, "failed", finishedAt),
        phase: "completed",
        startedAt: new Date(Date.parse(finishedAt) - 10_000).toISOString(),
      };
    });
    fixtures.runs = { runs: [failed, recovered, ...unrelated] };

    const overview = await loadOperationalOverview(new FixtureDaemonClient(fixtures));
    const candidate = overview.groups.attention.find((run) => run.id === failed.id);

    expect(candidate).toBeDefined();
    if (!candidate) {
      throw new Error("Expected the failed run to remain in the attention group.");
    }
    expect(
      attentionSeverity(
        candidate,
        [...overview.groups.active, ...overview.groups.attention, ...overview.groups.recent],
      ),
    ).toBe("blocked");
  });

  it.each(["running", "completed"] as const)(
    "keeps a non-retryable failure blocked when the %s query fails",
    async (unavailablePhase) => {
      const fixtures = emptyDaemonFixtures();
      const failed = attentionRun(
        `failed-without-${unavailablePhase}`,
        "failed",
        new Date(NOW - 30 * 60_000).toISOString(),
      );
      failed.trigger.ref = "6487";
      failed.startedAt = new Date(NOW - 60 * 60_000).toISOString();
      failed.terminalReason = "ISSUE_OVER_SCOPE";
      fixtures.runs = { runs: [failed] };
      const client = new FixtureDaemonClient(fixtures);
      const listRuns = client.listRuns.bind(client);
      vi.spyOn(client, "listRuns").mockImplementation(async (request, options) => {
        if (request?.phase === unavailablePhase) {
          throw new Error(`${unavailablePhase} history unavailable`);
        }
        return listRuns(request, options);
      });

      const overview = await loadOperationalOverview(client);
      const candidate = overview.groups.attention.find((run) => run.id === failed.id);

      expect(candidate).toBeDefined();
      if (!candidate) {
        throw new Error("Expected the failed run to remain in the attention group.");
      }
      expect(overview.groups.incomplete?.phases).toContain(unavailablePhase);
      expect(
        attentionSeverity(candidate, [
          ...overview.groups.active,
          ...overview.groups.attention,
          ...overview.groups.recent,
        ]),
      ).toBe("blocked");
    },
  );

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("keeps blocked and stalled runs ahead of newer warnings when attention is capped", async () => {
    const fixtures = emptyDaemonFixtures();
    const baseTime = NOW - 60_000;
    const failures = Array.from({ length: 20 }, (_, index) =>
      ({
        ...attentionRun(
          `01JZWARNING${String(index).padStart(2, "0")}`,
          "failed",
          new Date(baseTime - index * 1_000).toISOString(),
        ),
        terminalReason: "retryable provider.rate_limit",
      }),
    );
    const escalation = attentionRun(
      "01JZBLOCKED",
      "escalated",
      new Date(baseTime - 30_000).toISOString(),
    );
    const stalled: RunSummary = {
      ...attentionRun("01JZSTALLED", "failed", new Date(baseTime - 40_000).toISOString()),
      phase: "running",
      terminal: false,
      stale: true,
      finishedAt: undefined,
    };
    fixtures.runs = { runs: [...failures, escalation, stalled] };

    const overview = await loadOperationalOverview(new FixtureDaemonClient(fixtures));

    expect(overview.groups.attention).toHaveLength(20);
    expect(overview.groups.attention.slice(0, 2).map((run) => run.id)).toEqual([
      "01JZBLOCKED",
      "01JZSTALLED",
    ]);
    expect(overview.groups.attention.map((run) => run.id)).not.toContain("01JZWARNING18");
  });

  it("keeps a run whose last activity is just under the 24h boundary", async () => {
    const fixtures = emptyDaemonFixtures();
    const justUnder = new Date(NOW - (24 * 60 * 60 * 1000 - 1)).toISOString();
    fixtures.runs = { runs: [attentionRun("01JZ000JUSTUNDER", "escalated", justUnder)] };

    const overview = await loadOperationalOverview(new FixtureDaemonClient(fixtures));

    expect(overview.groups.attention.map((run) => run.id)).toEqual(["01JZ000JUSTUNDER"]);
  });

  it.each(["goobers:needs-human", "goobers:needs-remediation", "goobers:blocked-on-sibling"])(
    "keeps an older %s failure ahead of warnings and newer stalled work",
    async (label) => {
      const fixtures = emptyDaemonFixtures();
      const warnings = Array.from({ length: 25 }, (_, index) =>
        ({
          ...attentionRun(
            `warning-${index}`,
            "failed",
            new Date(NOW - 1_000 - index).toISOString(),
          ),
          terminalReason: "retryable provider.rate_limit",
        }),
      );
      const blocked = attentionRun("blocked-failure", "failed", new Date(NOW - 60_000).toISOString());
      blocked.operator = {
        issue: { number: "6487", labels: [label] },
        liveness: "terminal",
        trajectory: "terminal",
        claim: { leaseStatus: "none", providerMarker: "recorded" },
        potentialBlockers: [],
      };
      const stalled = Array.from({ length: 20 }, (_, index): RunSummary => ({
        ...attentionRun(`stalled-${index}`, "failed", new Date(NOW - 2_000 - index).toISOString()),
        phase: "running",
        terminal: false,
        stale: true,
      }));
      fixtures.runs = { runs: [...warnings, blocked, ...stalled] };

      const overview = await loadOperationalOverview(new FixtureDaemonClient(fixtures));

      expect(overview.groups.attention).toHaveLength(20);
      expect(overview.groups.attention[0].id).toBe(blocked.id);
      expect(overview.groups.attention.slice(1).every((run) => run.stale)).toBe(true);
    },
  );

  it("reports a truncated candidate window without paginating or discarding available runs", async () => {
    const fixtures = emptyDaemonFixtures();
    fixtures.runs = {
      runs: Array.from({ length: 101 }, (_, index) =>
        attentionRun(`failure-${index}`, "failed", new Date(NOW - 1_000 - index).toISOString()),
      ),
    };
    const client = new FixtureDaemonClient(fixtures);
    const listRuns = vi.spyOn(client, "listRuns");

    const overview = await loadOperationalOverview(client);

    expect(overview.groups.attention).toHaveLength(20);
    expect(overview.groups.attentionCandidatesTruncated).toBe(true);
    expect(listRuns).toHaveBeenCalledTimes(5);
    expect(listRuns.mock.calls.every(([request]) => request?.cursor === undefined)).toBe(true);
  });

  it("ages out a run whose last activity is just over the 24h boundary", async () => {
    const fixtures = emptyDaemonFixtures();
    const justOver = new Date(NOW - (24 * 60 * 60 * 1000 + 1)).toISOString();
    fixtures.runs = { runs: [attentionRun("01JZ000JUSTOVER", "failed", justOver)] };

    const overview = await loadOperationalOverview(new FixtureDaemonClient(fixtures));

    expect(overview.groups.attention).toEqual([]);
  });

  it("keeps a run active regardless of how long ago it started, as long as it was touched recently", async () => {
    const fixtures = emptyDaemonFixtures();
    const recentActivity = new Date(NOW - 60_000).toISOString();
    fixtures.runs = {
      // startedAt (STALE_STARTED_AT) is months old; only lastActivityAt is recent.
      runs: [attentionRun("01JZ000STILLACTIVE", "escalated", recentActivity)],
    };

    const overview = await loadOperationalOverview(new FixtureDaemonClient(fixtures));

    expect(overview.groups.attention.map((run) => run.id)).toEqual(["01JZ000STILLACTIVE"]);
  });

  it("reads as empty, not an error, when every escalation/failure has aged out", async () => {
    const fixtures = emptyDaemonFixtures();
    const longAgo = new Date(NOW - 7 * 24 * 60 * 60 * 1000).toISOString();
    fixtures.runs = {
      runs: [
        attentionRun("01JZ000OLDESCALATE", "escalated", longAgo),
        attentionRun("01JZ000OLDFAILED", "failed", longAgo),
      ],
    };

    const overview = await loadOperationalOverview(new FixtureDaemonClient(fixtures));

    expect(overview.groups.attention).toEqual([]);
    expect(overview.sectionErrors?.runs).toBeUndefined();
  });

  it("reports the runs section as failed, not silently empty, when the read model refuses orderByActivity", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const real = client.listRuns.bind(client);
    const refusal = new Error("orderByActivity requires the bounded read model");
    vi.spyOn(client, "listRuns").mockImplementation(async (request, options) => {
      if (request?.phase === "escalated" || request?.phase === "failed") {
        throw refusal;
      }
      return real(request, options);
    });

    const overview = await loadOperationalOverview(client);

    // The other three phases succeeding must not mask the attention group's
    // refusal — that would render an attention list empty for the same
    // reason as a genuinely idle instance, hiding real escalations.
    expect(overview.sectionErrors?.runs).toBe(refusal);
    expect(overview.groups).toEqual({ active: [], attention: [], recent: [] });
  });

  it("filters and orders escalated/failed queries by last activity within the window", async () => {
    const client = new FixtureDaemonClient(emptyDaemonFixtures());
    const listRuns = vi.spyOn(client, "listRuns");

    await loadOperationalOverview(client);

    const escalatedCall = listRuns.mock.calls.find(([request]) => request?.phase === "escalated");
    const failedCall = listRuns.mock.calls.find(([request]) => request?.phase === "failed");
    const expectedSince = new Date(NOW - 24 * 60 * 60 * 1000).toISOString();
    expect(escalatedCall?.[0]).toMatchObject({ orderByActivity: true, since: expectedSince });
    expect(failedCall?.[0]).toMatchObject({ orderByActivity: true, since: expectedSince });
  });
});
