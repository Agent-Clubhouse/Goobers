import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { emptyDaemonFixtures, populatedDaemonFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/insight";
  // populatedDaemonFixtures() is anchored to 2026-07-18, but the Insight page
  // filters telemetry by a window relative to the current time. Pin the clock to
  // the fixtures' "now" (their observedAt) so those windows include the fixture
  // data deterministically. Faking only Date leaves setTimeout/microtasks real,
  // so userEvent and findBy* still resolve. Without this the suite is a time
  // bomb: it passes at authoring time, then fails once wall-clock drifts past
  // the window (it began failing ~24h after landing).
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-07-18T20:00:00Z"));
});

afterEach(() => {
  vi.useRealTimers();
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
    expect(screen.getByRole("heading", { name: "Highest-contributing nodes" })).toBeInTheDocument();
    expect(
      screen.getByRole("link", {
        name: "View runs behind core implementation review: 1 failures, 1 escalations, 2 wasted attempts",
      }),
    ).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Slowest stages" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Ready-pool health" })).toBeInTheDocument();
    expect(screen.getByText("Throughput / demand")).toBeInTheDocument();
    expect(screen.getByText("8 / 6")).toBeInTheDocument();
    expect(screen.getByText("In flight now")).toBeInTheDocument();
    expect(screen.getByText("1h 30m 0s average · 2 claimed")).toBeInTheDocument();
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

  it("shows exact cost and token rollups with contributor-specific drill-downs", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listRuns = vi.spyOn(client, "listRuns");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Cost and tokens" }),
    ).toBeInTheDocument();
    expect(screen.queryByText("AI credits")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: /^View AI credit runs behind/ }),
    ).not.toBeInTheDocument();
    const costLinks = screen.getAllByRole("link", { name: /^View AI cost runs behind/ });
    expect(costLinks).toHaveLength(1);
    expect(costLinks[0]).toHaveAccessibleName(
      /Instance: 8 samples, P50 \$0\.80, P95 \$2\.50/,
    );

    const tokenLink = screen.getByRole("link", {
      name: /View token usage runs behind Instance/,
    });
    const costLink = screen.getByRole("link", {
      name: /View AI cost runs behind Instance/,
    });
    const wasteLink = screen.getByRole("link", {
      name: /View retry-waste runs behind Instance/,
    });
    expect(tokenLink).toHaveAttribute("href", expect.stringContaining("population=token-measured"));
    expect(costLink).toHaveAttribute("href", expect.stringContaining("population=cost-measured"));
    expect(wasteLink).toHaveAttribute("href", expect.stringContaining("population=retry-waste"));
    for (const link of [tokenLink, costLink, wasteLink]) {
      expect(link).toHaveAttribute("href", expect.not.stringContaining("outcome=finished"));
      expect(link).toHaveAttribute("href", expect.stringMatching(/since=.*until=/));
    }
    expect(screen.getAllByText("15,000 tokens").length).toBeGreaterThan(0);
    expect(screen.getByText("12,000 tokens")).toBeInTheDocument();
    expect(screen.getByText("$0.75")).toBeInTheDocument();

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      screen.getByRole("option", { name: "Workflow · core / implementation" }),
    );
    expect(screen.getAllByRole("link", { name: /^View AI cost runs behind/ })).toHaveLength(1);
    expect(
      screen.getByRole("link", {
        name: /View AI cost runs behind core \/ implementation: 8 samples, P50 \$0\.80, P95 \$2\.50/,
      }),
    ).toBeInTheDocument();
    expect(costLink).toHaveAttribute("href", expect.stringContaining("population=cost-measured"));

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      screen.getByRole("option", { name: "Gaggle · core" }),
    );
    expect(
      screen.getByRole("link", {
        name: /View AI cost runs behind core: 8 samples, P50 \$0\.80, P95 \$2\.50/,
      }),
    ).toBeInTheDocument();

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      screen.getByRole("option", { name: "Stage · core / implementation / implement" }),
    );
    expect(
      screen.getByRole("link", {
        name: /View AI cost runs behind core \/ implementation \/ implement: 4 samples, P50 \$1\.25, P95 \$2\.50/,
      }),
    ).toHaveAttribute("href", expect.stringContaining("population=cost-measured"));

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
    expect(unmeasuredCost).toHaveAttribute(
      "href",
      expect.stringMatching(
        /gaggle=tools.*workflow=implementation.*stage=implement.*population=cost-measured/,
      ),
    );

    await user.click(unmeasuredCost);
    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    await waitFor(() =>
      expect(listRuns).toHaveBeenCalledWith(
        expect.objectContaining({
          gaggle: "tools",
          workflow: "implementation",
          stage: "implement",
          outcome: undefined,
          population: "cost-measured",
          since: expect.stringMatching(/Z$/),
          until: expect.stringMatching(/Z$/),
        }),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      ),
    );
  });

  it("shows a cost trend and a same-length prior-period comparison for the selected scope", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const getTelemetryStats = vi.spyOn(client, "getTelemetryStats");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Cost over time" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("img", { name: /AI cost trend by bucket/ })).toBeInTheDocument();
    expect(screen.getAllByText(/vs\. previous 7 days/)).toHaveLength(2);

    await waitFor(() => {
      const ranges = getTelemetryStats.mock.calls.map(([request]) => ({
        since: request?.since,
        until: request?.until,
      }));
      // One call per trend bucket (7 for the default 7d window), plus one for
      // the immediately preceding 7-day period, plus the page's own snapshot
      // fetch for the selected window.
      expect(ranges.length).toBeGreaterThanOrEqual(9);
      const uniqueRanges = new Set(ranges.map((range) => `${range.since}:${range.until}`));
      expect(uniqueRanges.size).toBeGreaterThanOrEqual(8);
    });

    await user.selectOptions(screen.getByLabelText("Time window"), "all");
    expect(
      await screen.findByText(
        "Trend and period comparison need a bounded time window — choose 24h, 7d, or 30d.",
      ),
    ).toBeInTheDocument();
  });

  it("shows an instance-wide cost rollup broken down by gaggle, unaffected by the selected scope", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const getTelemetryStats = vi.spyOn(client, "getTelemetryStats");
    getTelemetryStats.mockResolvedValue({
      creditAssignment: [],
      causalCredit: null,
      gaggles: [
        {
          gaggle: "core",
          totalRuns: 4,
          completedRuns: 1,
          failedRuns: 1,
          infraFailedRuns: 0,
          otherRuns: 2,
        },
        {
          gaggle: "tools",
          totalRuns: 1,
          completedRuns: 0,
          failedRuns: 0,
          infraFailedRuns: 0,
          otherRuns: 1,
        },
      ],
      runs: [],
      stages: [],
      usage: [
        {
          scope: "gaggle",
          gaggle: "core",
          totalAttempts: 9,
          tokenSamples: 8,
          premiumRequestSamples: 0,
          costSamples: 8,
          costUSD: 4,
          p50CostUSD: 0.8,
          p95CostUSD: 2.5,
          retryWasteAttempts: 0,
        },
        {
          scope: "gaggle",
          gaggle: "tools",
          totalAttempts: 1,
          tokenSamples: 0,
          premiumRequestSamples: 0,
          costSamples: 3,
          costUSD: 6,
          p50CostUSD: 0.1,
          p95CostUSD: 5.8,
          retryWasteAttempts: 0,
        },
      ],
      models: [
        { model: "claude", usageSamples: 8, inputTokenSamples: 8, outputTokenSamples: 8, premiumRequestSamples: 0, costSamples: 6, costUSD: 6 },
        { model: "gpt", usageSamples: 2, inputTokenSamples: 2, outputTokenSamples: 2, premiumRequestSamples: 0, costSamples: 2, costUSD: 4 },
      ],
      curation: {
        everRecorded: false,
        runs: 0,
        reportedRuns: 0,
        ready: 0,
        needsHuman: 0,
        closed: 0,
        deduped: 0,
        split: 0,
        stale: 0,
        reconciled: 0,
        milestoned: 0,
        bounced: 0,
      },
      readyPool: {
        sampleEverRecorded: false,
        bounceEverRecorded: false,
        claimAgeSamples: 0,
        inFlightClaimSamples: 0,
        averageInFlightClaimAgeSeconds: 0,
        oldestInFlightClaimAgeSeconds: 0,
        forwardCurationThroughput: 0,
        implementationDemand: 0,
      },
    });
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(await screen.findByRole("heading", { name: "Instance spend" })).toBeInTheDocument();
    expect(screen.getByText("Total AI cost · all gaggles")).toBeInTheDocument();
    expect(screen.getByText("$10.00")).toBeInTheDocument();
    const coreLink = screen.getByRole("link", {
      name: /View instance spend for gaggle core: 8 samples, P50 \$0\.80, P95 \$2\.50/,
    });
    expect(coreLink).toBeInTheDocument();
    const toolsLink = screen.getByRole("link", {
      name: /View instance spend for gaggle tools: 3 samples, P50 \$0\.10, P95 \$5\.80/,
    });
    expect(toolsLink.compareDocumentPosition(coreLink) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);

    // Selecting a narrower scope must not change the instance-wide rollup —
    // it always reports across all gaggles regardless of the Scope dropdown.
    await user.selectOptions(
      screen.getByLabelText("Scope"),
      screen.getByRole("option", { name: "Gaggle · core" }),
    );
    expect(screen.getByText("$10.00")).toBeInTheDocument();
    expect(coreLink).toBeInTheDocument();
  });

  it("flags spend against a configured soft budget threshold", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(client, "getTelemetryStats").mockResolvedValue({
      creditAssignment: [],
      causalCredit: null,
      gaggles: [],
      runs: [],
      stages: [],
      usage: [],
      models: [
        { model: "claude", usageSamples: 1, inputTokenSamples: 1, outputTokenSamples: 1, premiumRequestSamples: 0, costSamples: 1, costUSD: 10 },
      ],
      curation: {
        everRecorded: false,
        runs: 0,
        reportedRuns: 0,
        ready: 0,
        needsHuman: 0,
        closed: 0,
        deduped: 0,
        split: 0,
        stale: 0,
        reconciled: 0,
        milestoned: 0,
        bounced: 0,
      },
      readyPool: {
        sampleEverRecorded: false,
        bounceEverRecorded: false,
        claimAgeSamples: 0,
        inFlightClaimSamples: 0,
        averageInFlightClaimAgeSeconds: 0,
        oldestInFlightClaimAgeSeconds: 0,
        forwardCurationThroughput: 0,
        implementationDemand: 0,
      },
    });
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(await screen.findByText("$10.00")).toBeInTheDocument();
    const budgetInput = screen.getByLabelText("Soft budget (USD)");

    await user.type(budgetInput, "5");
    await user.tab();
    expect(await screen.findByText(/over by \$5\.00/)).toBeInTheDocument();

    await user.clear(budgetInput);
    await user.type(budgetInput, "50");
    await user.tab();
    expect(await screen.findByText("20% of budget")).toBeInTheDocument();
    expect(screen.queryByText(/over by/)).not.toBeInTheDocument();
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

    const errorsHash = window.location.hash;
    await user.click(screen.getByRole("link", { name: "Back to Insight" }));
    expect(await screen.findByRole("heading", { name: "Insight" })).toBeInTheDocument();
    const callsBeforeRevisit = listTelemetryErrors.mock.calls.length;
    listTelemetryErrors.mockImplementation(() => new Promise(() => {}));

    await act(async () => {
      window.location.hash = errorsHash;
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });

    expect(await screen.findByRole("heading", { name: "Matching errors" })).toBeInTheDocument();
    expect(screen.getByText("Harness process exited unexpectedly.")).toBeInTheDocument();
    expect(listTelemetryErrors).toHaveBeenCalledTimes(callsBeforeRevisit);
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
    getTelemetryStats.mockResolvedValueOnce({
      creditAssignment: [],
      causalCredit: null,
      gaggles: [],
      runs: [],
      stages: [],
      usage: [],
      models: [],
      curation: {
        everRecorded: false,
        runs: 0,
        reportedRuns: 0,
        ready: 0,
        needsHuman: 0,
        closed: 0,
        deduped: 0,
        split: 0,
        stale: 0,
        reconciled: 0,
        milestoned: 0,
        bounced: 0,
      },
      readyPool: {
        sampleEverRecorded: false,
        bounceEverRecorded: false,
        claimAgeSamples: 0,
        inFlightClaimSamples: 0,
        averageInFlightClaimAgeSeconds: 0,
        oldestInFlightClaimAgeSeconds: 0,
        forwardCurationThroughput: 0,
        implementationDemand: 0,
      },
    });
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

  it("pre-selects the scope and time window from a deep link (#2528)", async () => {
    window.location.hash = "#/insight?gaggle=core&workflow=implementation&window=24h";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByLabelText("Scope")).toHaveDisplayValue(
      "Workflow · core / implementation",
    );
    expect(screen.getByLabelText("Time window")).toHaveDisplayValue("Last 24 hours");
    expect(screen.getByRole("link", { name: "Clear filters" })).toHaveAttribute(
      "href",
      "#/insight?window=24h",
    );
  });

  it("keeps a gaggle/workflow scope when navigating to Runs and back via the primary nav (#2528)", async () => {
    window.location.hash = "#/insight?gaggle=core&workflow=implementation";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await screen.findByLabelText("Scope");
    await user.click(screen.getByRole("button", { name: "Runs" }));

    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    expect(screen.getByText("core / implementation")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Insight" }));

    expect(await screen.findByLabelText("Scope")).toHaveDisplayValue(
      "Workflow · core / implementation",
    );
  });

  it("distinguishes a never-recorded writer from an empty window and from measured data", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const getTelemetryStats = vi.spyOn(client, "getTelemetryStats");
    const user = userEvent.setup();
    render(<App client={client} />);
    // The initial populated-fixture render (asserted in the first test above)
    // covers the fully measured state; this test isolates the two "no value"
    // states that otherwise look identical to an operator.
    await screen.findByRole("heading", { name: "Ready-pool health" });

    // Curation ran and reported real outputs, but the ready-pool-sample and
    // bounce-cohort writers never once fired for this scope — the exact
    // #2277 bug shape (one writer dead, a sibling writer fine).
    getTelemetryStats.mockResolvedValueOnce({
      creditAssignment: [],
      causalCredit: null,
      gaggles: [],
      runs: [],
      stages: [],
      usage: [],
      models: [],
      curation: {
        everRecorded: true,
        runs: 2,
        reportedRuns: 2,
        ready: 3,
        needsHuman: 1,
        closed: 0,
        deduped: 0,
        split: 0,
        stale: 0,
        reconciled: 0,
        milestoned: 0,
        bounced: 0,
      },
      readyPool: {
        sampleEverRecorded: false,
        bounceEverRecorded: false,
        claimAgeSamples: 0,
        inFlightClaimSamples: 0,
        averageInFlightClaimAgeSeconds: 0,
        oldestInFlightClaimAgeSeconds: 0,
        forwardCurationThroughput: 3,
        implementationDemand: 0,
      },
    });
    await user.selectOptions(screen.getByLabelText("Time window"), "24h");

    expect(await screen.findByText("3 ready · 1 needs human · 0 closed")).toBeInTheDocument();
    expect(screen.getByText("3 / 0")).toBeInTheDocument();
    expect(screen.getAllByText("Never recorded")).toHaveLength(3); // ready depth, oldest ready, bounce rate

    // Same writers HAVE fired historically, but this window has no rows —
    // must read differently from "never recorded" above.
    getTelemetryStats.mockResolvedValueOnce({
      creditAssignment: [],
      causalCredit: null,
      gaggles: [],
      runs: [],
      stages: [],
      usage: [],
      models: [],
      curation: {
        everRecorded: true,
        runs: 0,
        reportedRuns: 0,
        ready: 0,
        needsHuman: 0,
        closed: 0,
        deduped: 0,
        split: 0,
        stale: 0,
        reconciled: 0,
        milestoned: 0,
        bounced: 0,
      },
      readyPool: {
        sampleEverRecorded: true,
        bounceEverRecorded: true,
        claimAgeSamples: 0,
        inFlightClaimSamples: 0,
        averageInFlightClaimAgeSeconds: 0,
        oldestInFlightClaimAgeSeconds: 0,
        forwardCurationThroughput: 0,
        implementationDemand: 0,
      },
    });
    await user.selectOptions(screen.getByLabelText("Time window"), "30d");

    // ready depth, oldest ready, age before claim (unscoped by #2278, always
    // reads "No data in window" when absent), and bounce rate.
    expect(await screen.findAllByText("No data in window")).toHaveLength(4);
    expect(screen.queryByText("Never recorded")).not.toBeInTheDocument();
  });
});
