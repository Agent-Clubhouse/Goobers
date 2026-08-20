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
    expect(history.querySelectorAll("a")).toHaveLength(50);
    const callsBeforeLoadMore = listRuns.mock.calls.length;

    await user.click(screen.getByRole("button", { name: "Load more runs" }));

    await waitFor(() => expect(history.querySelectorAll("a")).toHaveLength(68));
    // Load more advanced a server-side cursor instead of refetching from the start.
    expect(listRuns.mock.calls.some(([request]) => Boolean(request?.cursor))).toBe(true);
    expect(listRuns.mock.calls.length).toBeGreaterThan(callsBeforeLoadMore);

    await user.click(screen.getByRole("button", { name: "Insight" }));
    expect(await screen.findByRole("heading", { name: "Insight" })).toBeInTheDocument();
    listRuns.mockClear();
    listRuns.mockImplementation(() => new Promise(() => {}));

    await user.click(screen.getByRole("button", { name: "Runs" }));

    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    const revisitedHistory = screen.getByRole("region", { name: "Run history" });
    expect(revisitedHistory.querySelectorAll("a")).toHaveLength(68);
    expect(listRuns).not.toHaveBeenCalled();
  }, 10_000);

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
      stale: false,
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

  it("shows how to start the first run when no runs exist", async () => {
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    expect(await screen.findByText("No runs recorded")).toBeInTheDocument();
    expect(screen.getByText("goobers run <workflow> <instance>")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Runs" })).toBeInTheDocument();
  });

  it("distinguishes a stale unmonitored run from a live running run", async () => {
    const fixtures = populatedDaemonFixtures();
    const running = fixtures.runs.runs.find((run) => run.phase === "running");
    if (!running) {
      throw new Error("Expected a running run fixture.");
    }
    running.stale = true;
    render(<App client={new FixtureDaemonClient(fixtures)} />);

    const row = await screen.findByRole("link", { name: `Open run ${running.id}` });
    expect(within(row).getByText("Stale / unmonitored")).toBeInTheDocument();
    expect(within(row).queryByText("Running")).not.toBeInTheDocument();
  });

  it("distinguishes filters that exclude existing runs and offers recovery", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.runs.runs = fixtures.runs.runs.filter((run) => run.phase === "completed");
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(fixtures)} />);

    await screen.findByRole("region", { name: "Run history" });
    await user.click(screen.getByRole("button", { name: "active" }));

    expect(await screen.findByText("Filters exclude existing runs")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Clear all filters" })).toHaveAttribute(
      "href",
      "#/runs",
    );
    expect(screen.getByText("goobers status <instance>")).toBeInTheDocument();

    await user.click(screen.getByRole("link", { name: "Clear all filters" }));
    expect(
      await screen.findByRole("link", { name: "Open run 01JZ455ESCALATE" }),
    ).toBeInTheDocument();
  });

  it("keeps a gaggle/workflow scope when navigating to Insight via the primary nav (#2528)", async () => {
    window.location.hash = "#/runs?gaggle=core&workflow=implementation";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByText("core / implementation")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Insight" }));

    expect(await screen.findByLabelText("Scope")).toHaveDisplayValue(
      "Workflow · core / implementation",
    );

    await user.click(screen.getByRole("button", { name: "Runs" }));

    expect(await screen.findByText("core / implementation")).toBeInTheDocument();
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
