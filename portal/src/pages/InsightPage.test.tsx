import { render, screen, waitFor, within } from "@testing-library/react";
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
    const getTelemetryErrorSignatures = vi.spyOn(client, "getTelemetryErrorSignatures");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(await screen.findByRole("heading", { name: "Insight" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Insight" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(screen.getByRole("heading", { name: "Success and failure" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Failure reasons" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Slowest stages" })).toBeInTheDocument();
    expect(screen.getByText("harness.crash")).toBeInTheDocument();
    expect(screen.getAllByText("unknown").length).toBeGreaterThan(0);
    expect(
      screen.getByRole("link", {
        name: "View 2 matching errors for harness.crash",
      }),
    ).toHaveAttribute(
      "href",
      expect.stringMatching(
        /^#\/errors\?code=harness\.crash&errorClass=unknown&since=.*&until=.*/,
      ),
    );
    expect(
      screen.getByRole("link", {
        name: "View 1 matching error for scheduler.storage",
      }),
    ).toHaveAttribute(
      "href",
      expect.stringMatching(
        /^#\/errors\?code=scheduler\.storage&errorClass=unknown&since=.*&until=.*/,
      ),
    );
    expect(screen.getAllByText("50.0%").length).toBeGreaterThan(0);
    expect(screen.getAllByText("P50").length).toBeGreaterThan(0);
    expect(screen.getAllByText("P95").length).toBeGreaterThan(0);

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      screen.getByRole("option", { name: "Stage · core / implementation / implement" }),
    );
    expect(
      screen.getByRole("link", {
        name: /^View terminal attempts behind core \/ implementation \/ implement for success rate 60.0%/,
      }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("link", {
        name: /^View terminal attempts behind core \/ implementation \/ implement for success rate/,
      }),
    ).toHaveAttribute(
      "href",
      expect.stringMatching(/stage=implement.*outcome=terminal.*population=attempts/),
    );
    expect(
      screen.getByRole("link", {
        name: /^View runs behind core implementation implement:/,
      }),
    ).toHaveAttribute(
      "href",
      expect.stringMatching(/stage=implement.*outcome=finished.*population=measured/),
    );
    await waitFor(() =>
      expect(getTelemetryErrorSignatures).toHaveBeenLastCalledWith(
        expect.objectContaining({
          gaggle: "core",
          workflow: "implementation",
          stage: "implement",
          since: expect.stringMatching(/Z$/),
          until: expect.stringMatching(/Z$/),
        }),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );

    await user.selectOptions(screen.getByLabelText("Time window"), "24h");
    await waitFor(() => {
      const request = getTelemetryStats.mock.calls.at(-1)?.[0];
      expect(request?.since).toMatch(/Z$/);
      expect(request?.until).toMatch(/Z$/);
      const errorRequest = getTelemetryErrorSignatures.mock.calls.at(-1)?.[0];
      expect(errorRequest?.stage).toBe("implement");
      expect(errorRequest?.since).toMatch(/Z$/);
      expect(errorRequest?.until).toMatch(/Z$/);
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
      screen.getByRole("link", { name: "View all runs behind core / implementation: 4" }),
    );

    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    expect(screen.getByText("core / implementation")).toBeInTheDocument();
    await waitFor(() =>
      expect(listRuns).toHaveBeenCalledWith(
        expect.objectContaining({
          gaggle: "core",
          workflow: "implementation",
          stage: undefined,
          outcome: "finished",
          population: undefined,
          since: expect.stringMatching(/Z$/),
          until: expect.stringMatching(/Z$/),
        }),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
  });

  it("shows measured AI usage and keeps missing runner usage unmeasured", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Credits, cost, and tokens" }),
    ).toBeInTheDocument();
    const costLinks = screen.getAllByRole("link", { name: /^View AI cost runs behind/ });
    expect(costLinks).toHaveLength(3);
    expect(costLinks[0]).toHaveAccessibleName(
      /core \/ implementation \/ implement: 4 samples, P50 \$1\.25, P95 \$2\.50/,
    );
    expect(costLinks[1]).toHaveAccessibleName(
      /core \/ implementation \/ review: 4 samples, P50 \$0\.40, P95 \$0\.80/,
    );

    const tokenLink = screen.getByRole("link", {
      name: /View token usage runs behind core \/ implementation \/ implement/,
    });
    const costLink = screen.getByRole("link", {
      name: /View AI cost runs behind core \/ implementation \/ implement/,
    });
    const wasteLink = screen.getByRole("link", {
      name: /View retry-waste runs behind core \/ implementation \/ implement/,
    });
    for (const link of [tokenLink, costLink, wasteLink]) {
      expect(link).toHaveAttribute(
        "href",
        expect.stringMatching(
          /#\/runs\?.*gaggle=core.*workflow=implementation.*stage=implement.*outcome=finished.*population=attempts.*since=.*until=/,
        ),
      );
    }
    expect(screen.getAllByText("24,000 tokens").length).toBeGreaterThan(0);
    expect(screen.getByText("12,000 tokens")).toBeInTheDocument();
    expect(screen.getByText("$0.75")).toBeInTheDocument();

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      screen.getByRole("option", { name: "Workflow · core / implementation" }),
    );
    expect(screen.getAllByRole("link", { name: /^View AI cost runs behind/ })).toHaveLength(2);
    expect(
      screen.queryByRole("link", {
        name: /View AI cost runs behind tools \/ implementation \/ implement/,
      }),
    ).not.toBeInTheDocument();

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      screen.getByRole("option", { name: "Stage · tools / implementation / implement" }),
    );

    const unmeasuredTokens = screen.getByRole("link", {
      name: /View token usage runs behind tools \/ implementation \/ implement: Unmeasured/,
    });
    const unmeasuredCost = screen.getByRole("link", {
      name: /View AI cost runs behind tools \/ implementation \/ implement: Unmeasured/,
    });
    expect(within(unmeasuredTokens).getAllByText("Unmeasured")).toHaveLength(3);
    expect(within(unmeasuredCost).getAllByText("Unmeasured")).toHaveLength(3);
    expect(screen.getByText("No retry waste")).toBeInTheDocument();
    expect(within(unmeasuredCost).queryByText("$0.00")).not.toBeInTheDocument();
    expect(within(unmeasuredTokens).queryByText("0 tokens")).not.toBeInTheDocument();

    await user.click(unmeasuredCost);
    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    await waitFor(() =>
      expect(listRuns).toHaveBeenCalledWith(
        expect.objectContaining({
          gaggle: "tools",
          workflow: "implementation",
          stage: "implement",
          outcome: "finished",
          population: "attempts",
          since: expect.stringMatching(/Z$/),
          until: expect.stringMatching(/Z$/),
        }),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
  });

  it("drills into every matching run error while keeping the selected filters", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listTelemetryErrors = vi.spyOn(client, "listTelemetryErrors");
    const user = userEvent.setup();
    render(<App client={client} />);

    await user.selectOptions(
      await screen.findByLabelText("Scope"),
      screen.getByRole("option", { name: "Stage · core / implementation / implement" }),
    );
    await user.click(
      screen.getByRole("link", { name: "View 2 matching errors for harness.crash" }),
    );

    expect(await screen.findByRole("heading", { name: "Matching errors" })).toBeInTheDocument();
    expect(screen.getByText("Harness exited before producing a result envelope.")).toBeInTheDocument();
    expect(screen.getByText("Harness process exited unexpectedly.")).toBeInTheDocument();
    await waitFor(() =>
      expect(listTelemetryErrors).toHaveBeenCalledWith(
        expect.objectContaining({
          gaggle: "core",
          workflow: "implementation",
          stage: "implement",
          code: "harness.crash",
          errorClass: "unknown",
          since: expect.stringMatching(/Z$/),
          until: expect.stringMatching(/Z$/),
        }),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
  });

  it("provides an inspectable drill-through for instance errors", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const user = userEvent.setup();
    render(<App client={client} />);

    await user.click(
      await screen.findByRole("link", {
        name: "View 1 matching error for scheduler.storage",
      }),
    );

    expect(await screen.findByText("Scheduler journal append failed.")).toBeInTheDocument();
    expect(screen.getByText("Instance scheduler")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /Open run .*scheduler.storage/ })).not.toBeInTheDocument();
  });

  it("gives each outcome number its exact run population", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await user.selectOptions(
      await screen.findByLabelText("Scope"),
      screen.getByRole("option", { name: "Workflow · core / implementation" }),
    );

    const terminal = screen.getByRole("link", {
      name: "View terminal runs behind core / implementation for success rate 50.0%",
    });
    const succeeded = screen.getByRole("link", {
      name: "View successful runs behind core / implementation: 1",
    });
    const failed = screen.getByRole("link", {
      name: "View failed runs behind core / implementation: 1",
    });
    const other = screen.getByRole("link", {
      name: "View other runs behind core / implementation: 2",
    });
    const total = screen.getByRole("link", {
      name: "View all runs behind core / implementation: 4",
    });

    expect(terminal).toHaveAttribute("href", expect.stringContaining("outcome=terminal"));
    expect(succeeded).toHaveAttribute("href", expect.stringContaining("outcome=success"));
    expect(failed).toHaveAttribute("href", expect.stringContaining("outcome=failure"));
    expect(other).toHaveAttribute("href", expect.stringContaining("outcome=other"));
    expect(total).toHaveAttribute("href", expect.stringContaining("outcome=finished"));
  });

  it("keeps a selected scope when a narrower window has no rows", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const getTelemetryStats = vi.spyOn(client, "getTelemetryStats");
    const getTelemetryErrorSignatures = vi.spyOn(client, "getTelemetryErrorSignatures");
    const user = userEvent.setup();
    render(<App client={client} />);

    await user.selectOptions(
      await screen.findByLabelText("Scope"),
      screen.getByRole("option", { name: "Workflow · core / implementation" }),
    );
    getTelemetryStats.mockResolvedValueOnce({ gaggles: [], runs: [], stages: [], models: [] });
    getTelemetryErrorSignatures.mockResolvedValueOnce({ items: [] });

    await user.selectOptions(screen.getByLabelText("Time window"), "24h");

    expect(
      await screen.findByRole("heading", { name: "No telemetry in this window" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Scope")).toHaveDisplayValue(
      "Workflow · core / implementation",
    );
    expect(screen.queryByText("Gaggle: Instance")).not.toBeInTheDocument();
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
