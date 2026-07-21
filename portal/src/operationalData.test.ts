import { describe, expect, it, vi } from "vitest";
import { FixtureDaemonClient } from "./api/fixtureClient";
import { loadOperationalOverview } from "./operationalData";
import { largeJournalFixtures, populatedDaemonFixtures } from "./test/daemonFixtures";

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
