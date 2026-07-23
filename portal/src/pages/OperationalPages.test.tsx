import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { DaemonUnavailableError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { Health, RequestOptions } from "../api/types";
import {
  emptyDaemonFixtures,
  largeJournalFixtures,
  populatedDaemonFixtures,
} from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/overview";
});

describe("operational overview", () => {
  it("shows a ready first boot without inventing configured resources", async () => {
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    expect(
      await screen.findByRole("heading", { name: "Daemon is ready." }),
    ).toBeInTheDocument();
    expect(screen.getByText(/No gaggles are configured/)).toBeInTheDocument();
    expect(screen.getByText("Daemon ready")).toBeInTheDocument();
    expect(screen.queryByText("Static fixture data")).not.toBeInTheDocument();
  });

  it("reports a stale scheduler heartbeat as unhealthy", async () => {
    const fixtures = emptyDaemonFixtures();
    fixtures.health = {
      ...fixtures.health,
      healthy: false,
      freshness: {
        ...fixtures.health.freshness,
        lastSchedulerTickAt: "2026-07-18T19:57:00Z",
        lastTickAgeMillis: 180_000,
      },
    };
    render(<App client={new FixtureDaemonClient(fixtures)} />);

    expect(
      await screen.findByRole("heading", { name: "Daemon is unhealthy." }),
    ).toBeInTheDocument();
    expect(screen.getByText("Daemon unhealthy")).toBeInTheDocument();
    expect(screen.getByText(/last scheduler tick 3m 0s ago/)).toBeInTheDocument();
  });

  it("refreshes health while events stay connected and reports a newly stale heartbeat", async () => {
    vi.useFakeTimers();
    const client = new StallingSchedulerClient();
    const rendered = render(<App client={client} />);

    try {
      await act(async () => vi.advanceTimersByTimeAsync(0));
      expect(screen.getByRole("heading", { name: "Daemon is ready." })).toBeInTheDocument();

      await act(async () => vi.advanceTimersByTimeAsync(5_000));

      expect(screen.getByRole("heading", { name: "Daemon is unhealthy." })).toBeInTheDocument();
      expect(screen.getByText("Daemon unhealthy")).toBeInTheDocument();
      expect(screen.getByText(/last scheduler tick 3m 0s ago/)).toBeInTheDocument();
      expect(client.healthRequests).toBe(2);
    } finally {
      rendered.unmount();
      vi.useRealTimers();
    }
  });

  it("groups canonical phases and places attention rows before aggregate counts", async () => {
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const attentionHeading = await screen.findByRole("heading", { name: "Needs attention" });
    const attentionSection = attentionHeading.closest("section");
    const counts = screen.getByRole("region", {
      name: "Daemon connection and instance counts",
    });
    if (!attentionSection) {
      throw new Error("Attention section was not rendered.");
    }

    expect(attentionSection.compareDocumentPosition(counts) & Node.DOCUMENT_POSITION_FOLLOWING).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
    expect(
      within(attentionSection).getByRole("link", { name: "Open run 01JZ402DASHBOARD" }),
    ).toBeInTheDocument();
    expect(
      within(attentionSection).getByRole("link", { name: "Open run 01JZ400FAILED" }),
    ).toBeInTheDocument();
    expect(
      within(screen.getByRole("region", { name: "Active runs" })).getByRole("link", {
        name: "Open run 01JZ441DAEMONAPI",
      }),
    ).toBeInTheDocument();

    const recent = screen.getByRole("region", { name: "Recent outcomes" });
    expect(
      within(recent).getByRole("link", { name: "Open run 01JZ455ESCALATE" }),
    ).toBeInTheDocument();
    expect(
      within(recent).getByRole("link", { name: "Open run 01JZ300ABORTED" }),
    ).toBeInTheDocument();
    expect(within(recent).queryByText("Failed")).not.toBeInTheDocument();
    expect(within(counts).getAllByText("2", { selector: "dd" })).toHaveLength(2);
    expect(within(counts).getByText("1", { selector: "dd" })).toBeInTheDocument();
  });

  it("bounds recent outcomes and sources active runs server-side on a large journal", async () => {
    const client = new FixtureDaemonClient(largeJournalFixtures({ completed: 60 }));
    const listRuns = vi.spyOn(client, "listRuns");
    render(<App client={client} />);

    const recent = await screen.findByRole("region", { name: "Recent outcomes" });
    // "Recent outcomes" is capped regardless of the 60+ terminal runs in the journal.
    expect(within(recent).getAllByRole("link").length).toBeLessThanOrEqual(20);

    // Active runs come from the server-side phase=running filter, not a client sweep.
    expect(listRuns).toHaveBeenCalledWith(
      expect.objectContaining({ phase: "running" }),
      expect.anything(),
    );
    // The Overview never paginates the full history: no request carries a cursor.
    expect(listRuns.mock.calls.every(([request]) => request?.cursor === undefined)).toBe(true);
  });

  it("shows loading and recovers explicitly when the daemon reconnects", async () => {
    const client = new RecoveringClient();
    render(<App client={client} />);

    expect(screen.getByRole("heading", { name: "Connecting to daemon" })).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", { name: "Daemon unavailable" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/No fixture data has been substituted/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Reconnect" }));
    expect(
      await screen.findByRole("heading", { name: "2 runs need attention." }),
    ).toBeInTheDocument();
  });
});

describe("workflow and gaggle inventory", () => {
  it("renders multiple gaggles, duplicate names, roster contracts, and unique deep links", async () => {
    window.location.hash = "#/workflows";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "Core product" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Developer tools" })).toBeInTheDocument();
    expect(screen.getByText("Core implementer")).toBeInTheDocument();
    expect(screen.getByText("Tools implementer")).toBeInTheDocument();
    expect(screen.getAllByText("go, react")).toHaveLength(2);
    expect(screen.getAllByText("repo:push")).toHaveLength(2);
    expect(screen.getAllByText(/implementation \/ implement \(agentic\)/)).toHaveLength(2);

    const coreLink = screen.getByRole("link", {
      name: "Open workflow Implementation for gaggle Core product",
    });
    const toolsLink = screen.getByRole("link", {
      name: "Open workflow Implementation for gaggle Developer tools",
    });
    expect(coreLink).toHaveAttribute("href", "#/workflow/core/implementation");
    expect(toolsLink).toHaveAttribute("href", "#/workflow/tools/implementation");
    expect(screen.getByText("Escalated")).toBeInTheDocument();
    expect(screen.getByText("Aborted")).toBeInTheDocument();

    await userEvent.click(toolsLink);
    await waitFor(() =>
      expect(window.location.hash).toBe("#/workflow/tools/implementation"),
    );
  });

  it("renders the ready-empty workflow state", async () => {
    window.location.hash = "#/workflows";
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    expect(
      await screen.findByRole("heading", { name: "No gaggles configured" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/The daemon is ready/)).toBeInTheDocument();
  });
});

class RecoveringClient extends FixtureDaemonClient {
  private healthRequests = 0;

  constructor() {
    super(populatedDaemonFixtures());
  }

  override getHealth(options?: RequestOptions): Promise<Health> {
    this.healthRequests += 1;
    if (this.healthRequests === 1) {
      return Promise.reject(new DaemonUnavailableError());
    }
    return super.getHealth(options);
  }
}

class StallingSchedulerClient extends FixtureDaemonClient {
  healthRequests = 0;

  constructor() {
    super(emptyDaemonFixtures());
  }

  override async getHealth(options?: RequestOptions): Promise<Health> {
    const request = ++this.healthRequests;
    const health = await super.getHealth(options);
    if (request === 1) {
      return health;
    }
    return {
      ...health,
      healthy: false,
      freshness: {
        ...health.freshness,
        observedAt: "2026-07-18T20:00:00Z",
        lastSchedulerTickAt: "2026-07-18T19:57:00Z",
        lastTickAgeMillis: 180_000,
      },
    };
  }
}
