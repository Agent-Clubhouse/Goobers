import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { DaemonUnavailableError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { RunSummary } from "../api/types";
import {
  emptyDaemonFixtures,
  largeJournalFixtures,
  populatedDaemonFixtures,
} from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/runs";
});

describe("runs history page", () => {
  it("reads live daemon runs and paginates with server-side cursors", async () => {
    const client = new FixtureDaemonClient(
      largeJournalFixtures({ completed: 68, running: 0, failed: 0, escalated: 0, aborted: 0 }),
    );
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    const history = await screen.findByRole("region", { name: "Run history" });
    // The initial load is one bounded page, not the whole 68-run journal.
    expect(within(history).getAllByRole("link")).toHaveLength(50);
    const callsBeforeLoadMore = listRuns.mock.calls.length;

    await user.click(screen.getByRole("button", { name: "Load more runs" }));

    await waitFor(() =>
      expect(
        within(screen.getByRole("region", { name: "Run history" })).getAllByRole("link"),
      ).toHaveLength(68),
    );
    // Load more advanced a server-side cursor instead of refetching from the start.
    const lastCall = listRuns.mock.calls.at(-1);
    expect(lastCall?.[0]?.cursor).toBeTruthy();
    expect(listRuns.mock.calls.length).toBeGreaterThan(callsBeforeLoadMore);

    await user.click(screen.getByRole("button", { name: "Insight" }));
    expect(await screen.findByRole("heading", { name: "Insight" })).toBeInTheDocument();
    const callsBeforeRevisit = listRuns.mock.calls.length;
    listRuns.mockImplementation(() => new Promise(() => {}));

    await user.click(screen.getByRole("button", { name: "Runs" }));

    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    expect(
      within(screen.getByRole("region", { name: "Run history" })).getAllByRole("link"),
    ).toHaveLength(68);
    expect(listRuns).toHaveBeenCalledTimes(callsBeforeRevisit);
  });

  it("maps filter chips onto server-side phase requests", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "active" }));

    expect(
      await screen.findByRole("link", { name: "Open run 01JZ441DAEMONAPI" }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Open run 01JZ455ESCALATE" }),
    ).not.toBeInTheDocument();
    expect(listRuns).toHaveBeenCalledWith(
      expect.objectContaining({ phase: "running" }),
      expect.anything(),
    );
  });

  it("hides no-work runs by default and reveals them via the toggle (#2188)", async () => {
    const noWorkRun: RunSummary = {
      id: "01JZ000NOWORK",
      workflow: "backlog-curation",
      workflowVersion: 1,
      gaggle: "core",
      trigger: { kind: "schedule", ref: "0 * * * *" },
      phase: "completed",
      terminal: true,
      startedAt: "2026-07-18T00:00:00Z",
      finishedAt: "2026-07-18T00:00:20Z",
      durationMillis: 20_000,
      lastActivityAt: "2026-07-18T00:00:20Z",
      lastSeq: 2,
      repassCount: 0,
      retryCount: 0,
      policyRetryCount: 0,
      infraRetryCount: 0,
      noWork: true,
    };
    const producedRun: RunSummary = {
      ...noWorkRun,
      id: "01JZ000PRODUCED",
      noWork: false,
      startedAt: "2026-07-18T00:05:00Z",
      finishedAt: "2026-07-18T00:06:00Z",
      lastActivityAt: "2026-07-18T00:06:00Z",
    };
    const fixtures = emptyDaemonFixtures();
    fixtures.runs = { runs: [noWorkRun, producedRun] };
    const client = new FixtureDaemonClient(fixtures);
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(await screen.findByRole("link", { name: /Open run 01JZ000PRODUCED/ })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Open run 01JZ000NOWORK/ })).not.toBeInTheDocument();

    await user.click(screen.getByRole("checkbox", { name: "Show no-work runs" }));

    expect(await screen.findByRole("link", { name: /Open run 01JZ000NOWORK/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Open run 01JZ000PRODUCED/ })).toBeInTheDocument();
  });

  it("shows an empty state without inventing runs", async () => {
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    expect(await screen.findByText("No runs match this filter.")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Runs" })).toBeInTheDocument();
  });

  it("surfaces a daemon error with an explicit reconnect affordance", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(client, "listRuns").mockRejectedValue(new DaemonUnavailableError());
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Daemon unavailable" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reconnect" })).toBeInTheDocument();
  });
});
