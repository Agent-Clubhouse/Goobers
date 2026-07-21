import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { DaemonUnavailableError } from "../api/errors";
import { FixtureDaemonClient } from "../api/fixtureClient";
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
