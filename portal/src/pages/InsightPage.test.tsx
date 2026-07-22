import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { emptyDaemonFixtures, populatedDaemonFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/insight";
});

describe("Insight page", () => {
  it("shows scoped outcomes and full stage duration distributions", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const getTelemetryStats = vi.spyOn(client, "getTelemetryStats");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(await screen.findByRole("heading", { name: "Insight" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Insight" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByRole("heading", { name: "Success and failure" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Slowest stages" })).toBeInTheDocument();
    expect(screen.getAllByText("50.0%").length).toBeGreaterThan(0);
    expect(screen.getAllByText("P50").length).toBeGreaterThan(0);
    expect(screen.getAllByText("P95").length).toBeGreaterThan(0);

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      screen.getByRole("option", { name: "Stage · core / implementation / implement" }),
    );
    expect(screen.getByText("60.0%")).toBeInTheDocument();
    expect(
      screen.getByRole("link", {
        name: /^View runs behind core \/ implementation \/ implement:/,
      }),
    ).toHaveAttribute("href", expect.stringContaining("stage=implement"));

    await user.selectOptions(screen.getByLabelText("Time window"), "24h");
    await waitFor(() => {
      const request = getTelemetryStats.mock.calls.at(-1)?.[0];
      expect(request?.since).toMatch(/Z$/);
      expect(request?.until).toMatch(/Z$/);
    });
  });

  it("drills into run history with the selected scope and time window", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    await user.selectOptions(
      await screen.findByLabelText("Scope"),
      screen.getByRole("option", { name: "Workflow · core / implementation" }),
    );
    await user.click(
      screen.getByRole("link", { name: /^View runs behind core \/ implementation:/ }),
    );

    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    expect(screen.getByText("core / implementation")).toBeInTheDocument();
    await waitFor(() =>
      expect(listRuns).toHaveBeenCalledWith(
        expect.objectContaining({
          gaggle: "core",
          workflow: "implementation",
          stage: undefined,
          since: expect.stringMatching(/Z$/),
          until: expect.stringMatching(/Z$/),
        }),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
  });

  it("shows an honest empty state when no telemetry was measured", async () => {
    render(<App client={new FixtureDaemonClient(emptyDaemonFixtures())} />);

    expect(
      await screen.findByRole("heading", { name: "No telemetry in this window" }),
    ).toBeInTheDocument();
    expect(screen.queryByText("0%")).not.toBeInTheDocument();
  });

  it("does not relabel an old snapshot when a new time window fails", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const user = userEvent.setup();
    render(<App client={client} />);
    await screen.findByRole("heading", { name: "Insight" });
    vi.spyOn(client, "getTelemetryStats").mockRejectedValueOnce(new Error("window failed"));

    await user.selectOptions(screen.getByLabelText("Time window"), "24h");

    expect(await screen.findByRole("heading", { name: "Daemon unavailable" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Success and failure" })).not.toBeInTheDocument();
  });
});
