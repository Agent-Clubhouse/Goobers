import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { DaemonApiError } from "../api/errors";
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
  it("keeps cost reporting out of operational Insight", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const getTelemetryStats = vi.spyOn(client, "getTelemetryStats");
    const getTelemetryErrorSignatures = vi.spyOn(client, "getTelemetryErrorSignatures");
    const getTelemetryCosts = vi.spyOn(client, "getTelemetryCosts");

    render(<App client={client} />);

    await waitFor(() => {
      expect(getTelemetryStats).toHaveBeenCalledTimes(1);
      expect(getTelemetryErrorSignatures).toHaveBeenCalledTimes(1);
      expect(getTelemetryCosts).not.toHaveBeenCalled();
    });
    expect(screen.queryByRole("heading", { name: "Instance spend" })).not.toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Cost by pull request and issue" }),
    ).not.toBeInTheDocument();
  });

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
    expect(await screen.findByText("harness.crash")).toBeInTheDocument();
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
      JSON.stringify(["stage", "core", "implementation", "implement"]),
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
    expect(screen.getByLabelText("Filter by gaggle")).toHaveDisplayValue("Core product");
    expect(screen.getByLabelText("Filter by workflow")).toHaveDisplayValue("Implementation");
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
    window.location.hash = "#/cost";
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Cost summary" }),
    ).toBeInTheDocument();
    expect(screen.queryByText("AI credits")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: /^View AI credit runs behind/ }),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /^View AI cost runs behind/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: /^View retry-waste runs behind/ })).not.toBeInTheDocument();
    expect(screen.getByText("12,000 tokens")).toBeInTheDocument();
    expect(screen.getByText("$0.75")).toBeInTheDocument();

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      JSON.stringify(["workflow", "core", "implementation"]),
    );
    expect(screen.queryByRole("link", { name: /^View AI cost runs behind/ })).not.toBeInTheDocument();

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      JSON.stringify(["gaggle", "core"]),
    );
    expect(screen.queryByRole("link", { name: /^View AI cost runs behind/ })).not.toBeInTheDocument();

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      JSON.stringify(["stage", "core", "implementation", "implement"]),
    );
    expect(screen.queryByRole("link", { name: /^View AI cost runs behind/ })).not.toBeInTheDocument();

    await user.selectOptions(
      screen.getByLabelText("Scope"),
      JSON.stringify(["stage", "tools", "implementation", "implement"]),
    );

    const unmeasuredCost = screen
      .getByText("AI cost", { selector: ".usage-metric-static .usage-metric-heading strong" })
      .closest<HTMLElement>(".usage-metric-static");
    if (!unmeasuredCost) throw new Error("Expected a static AI cost metric.");
    expect(within(unmeasuredCost).getAllByText("Unmeasured")).toHaveLength(3);
    expect(screen.getByText("No retry waste")).toBeInTheDocument();
    expect(within(unmeasuredCost).queryByText("$0.00")).not.toBeInTheDocument();
    expect(unmeasuredCost.tagName).toBe("DIV");
  });

  it("shows provider-native attributed costs, normalized estimates, and coverage", async () => {
    window.location.hash = "#/cost";
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const getTelemetryCosts = vi.spyOn(client, "getTelemetryCosts");
    const user = userEvent.setup();
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Cost by pull request and issue" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/Exact recorded usage · Loaded Jul 18, 2026/)).toBeInTheDocument();
    const table = screen.getByRole("table");
    expect(screen.getByText("PR #4398")).toBeInTheDocument();
    expect(screen.getByText("Issue #4398")).toBeInTheDocument();
    expect(screen.getAllByText("3 AIC").length).toBeGreaterThan(0);
    expect(screen.getAllByText("$0.03").length).toBeGreaterThan(0);
    expect(screen.getAllByText("$0.42").length).toBeGreaterThan(0);
    expect(screen.getAllByText("42 AIC").length).toBeGreaterThan(0);
    expect(
      screen.getByText("Lower bound: 2 of 3 runs and 3 of 4 attempts measured."),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Complete coverage: 2 runs and 2 attempts measured."),
    ).toBeInTheDocument();
    expect(screen.getByText("gpt-5.6-sol: 3 AIC · 3/3 attempts")).toBeInTheDocument();
    expect(screen.getByText("claude-sonnet: $0.42 · 2/2 attempts")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "View 1 run for PR #4398" }));
    expect(screen.getByRole("dialog", { name: "PR #4398 runs" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "01JZ455ESCALATE" })).toHaveAttribute(
      "href",
      "#/run/01JZ455ESCALATE",
    );
    await user.click(screen.getByRole("button", { name: "Close run list" }));

    let rows = within(table).getAllByRole("row").slice(1);
    expect(rows[0]).toHaveTextContent("PR #4398");
    const nativeSort = screen.getByRole("button", { name: /Provider-native/ });
    expect(nativeSort.closest('[role="columnheader"]')).toHaveAttribute(
      "aria-sort",
      "descending",
    );
    await user.click(nativeSort);
    rows = within(table).getAllByRole("row").slice(1);
    expect(rows[0]).toHaveTextContent("Issue #4398");
    expect(nativeSort.closest('[role="columnheader"]')).toHaveAttribute(
      "aria-sort",
      "ascending",
    );

    await user.selectOptions(screen.getByRole("combobox", { name: "Type" }), "pr");
    expect(screen.getByText("PR #4398")).toBeInTheDocument();
    expect(screen.queryByText("Issue #4398")).not.toBeInTheDocument();

    await user.selectOptions(screen.getByRole("combobox", { name: "Type" }), "all");
    await user.type(screen.getByRole("searchbox", { name: "Filter" }), "claude-sonnet");
    expect(screen.getByText("Issue #4398")).toBeInTheDocument();
    expect(screen.queryByText("PR #4398")).not.toBeInTheDocument();

    expect(getTelemetryCosts).toHaveBeenCalledWith(
      expect.objectContaining({
        scope: "summary",
        since: expect.stringMatching(/Z$/),
        until: expect.stringMatching(/Z$/),
      }),
      expect.objectContaining({ signal: expect.any(AbortSignal) }),
    );
  });

  it("keeps Cost usable and gives upgrade guidance when attributed costs are unsupported", async () => {
    window.location.hash = "#/cost";
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(client, "getTelemetryCosts").mockRejectedValue(
      new DaemonApiError(404, "not_found", "route not found"),
    );
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Cost summary" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Attributed costs are not supported by this daemon. Upgrade Goobers to enable pull request and issue cost reporting.",
      ),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Retry" })).not.toBeInTheDocument();
  });

  it("isolates a missing trend capability without blanking Cost", async () => {
    window.location.hash = "#/cost";
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const original = client.getTelemetryStats.bind(client);
    vi.spyOn(client, "getTelemetryStats").mockImplementation(async (request, options) => {
      const result = await original(request, options);
      return request?.trendBuckets ? { ...result, trend: undefined } : result;
    });
    render(<App client={client} />);

    expect(
      await screen.findByRole("heading", { name: "Cost summary" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Cost trends are not supported by this daemon. Upgrade Goobers to enable this section.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Cost by gaggle" })).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Cost by pull request and issue" }),
    ).toBeInTheDocument();
  });

  it("shows a cost trend and a same-length prior-period comparison for the selected scope", async () => {
    window.location.hash = "#/cost";
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
      expect(ranges.length).toBeGreaterThanOrEqual(2);
      expect(getTelemetryStats.mock.calls.some(([request]) => request?.trendBuckets === 14)).toBe(
        true,
      );
    });

    const trendRequestsBeforeAll = getTelemetryStats.mock.calls.filter(
      ([request]) => request?.trendBuckets !== undefined,
    ).length;
    await user.selectOptions(screen.getByLabelText("Time window"), "all");
    expect(
      await screen.findByText(
        "Trend and period comparison need a bounded time window — choose 24h, 7d, or 30d.",
      ),
    ).toBeInTheDocument();
    expect(
      getTelemetryStats.mock.calls.filter(([request]) => request?.trendBuckets !== undefined),
    ).toHaveLength(trendRequestsBeforeAll);
  });

  it("shows an instance-wide cost rollup broken down by gaggle, unaffected by the selected scope", async () => {
    window.location.hash = "#/cost";
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

    expect(await screen.findByRole("heading", { name: "Cost by gaggle" })).toBeInTheDocument();
    const coreLink = screen.getByRole("link", {
      name: /View instance spend for gaggle core: 8 samples, P50 \$0\.80, P95 \$2\.50/,
    });
    expect(coreLink).toBeInTheDocument();
    const toolsLink = screen.getByRole("link", {
      name: /View instance spend for gaggle tools: 3 samples, P50 \$0\.10, P95 \$5\.80/,
    });
    expect(toolsLink.compareDocumentPosition(coreLink) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0);

    // The all-gaggle breakdown is subordinate to the instance scope and leaves
    // the page when the operator narrows to one gaggle.
    await user.selectOptions(
      screen.getByLabelText("Scope"),
      JSON.stringify(["gaggle", "core"]),
    );
    expect(screen.queryByRole("heading", { name: "Cost by gaggle" })).not.toBeInTheDocument();
  });

  it("shows total cost without a browser-local soft budget", async () => {
    window.location.hash = "#/cost";
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
    render(<App client={client} />);

    expect(await screen.findByRole("heading", { name: "Cost" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Soft budget (USD)")).not.toBeInTheDocument();
  });

  it("keeps a selected Cost workflow visible without a gaggle aggregate row", async () => {
    window.location.hash = "#/cost?gaggle=core&workflow=implementation";
    const fixtures = populatedDaemonFixtures();
    fixtures.telemetryStats.gaggles = [];
    render(<App client={new FixtureDaemonClient(fixtures)} />);

    expect(await screen.findByRole("heading", { name: "Cost" })).toBeInTheDocument();
    expect(screen.getByLabelText("Scope")).toHaveDisplayValue("Workflow · implementation");
  });

  it("drills into every matching run error while keeping the selected filters", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const listTelemetryErrors = vi.spyOn(client, "listTelemetryErrors");
    const user = userEvent.setup();
    render(<App client={client} />);

    await user.selectOptions(
      await screen.findByLabelText("Scope"),
      JSON.stringify(["stage", "core", "implementation", "implement"]),
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
    await user.click(screen.getByRole("button", { name: "Insight" }));
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

    await screen.findByRole("heading", { name: "Failure reasons" });
    await user.click(
      screen.getByRole("link", {
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

    expect(
      await screen.findByRole("heading", { name: "Couldn't load Goobers data" }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Success and failure" })).not.toBeInTheDocument();
  });

  it("pre-selects the scope and time window from a deep link (#2528)", async () => {
    window.location.hash = "#/insight?gaggle=core&workflow=implementation&window=24h";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByLabelText("Scope")).toHaveDisplayValue(
      "Workflow · core / implementation",
    );
    expect(screen.getByLabelText("Time window")).toHaveDisplayValue("Last 24 hours");
    expect(window.location.hash).toBe("#/insight?gaggle=core&workflow=implementation&window=24h");
  });

  it("opens a focused detail section from a shareable URL", async () => {
    window.location.hash = "#/insight?section=failures";
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "Failure reasons" })).toBeInTheDocument();
    expect(screen.getByText("harness.crash")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Slowest stages" })).toBeInTheDocument();
  });

  it("keeps identity and time scope across Runs, Insight, and Cost primary pivots", async () => {
    window.location.hash = "#/insight?gaggle=core&workflow=implementation&window=24h";
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await screen.findByLabelText("Scope");
    await user.click(screen.getByRole("button", { name: "Runs" }));

    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    expect(screen.getByLabelText("Filter by gaggle")).toHaveDisplayValue("Core product");
    expect(screen.getByLabelText("Filter by workflow")).toHaveDisplayValue("Implementation");
    expect(window.location.hash).toContain("window=24h");

    await user.click(screen.getByRole("button", { name: "Cost" }));
    expect(await screen.findByRole("heading", { name: "Cost" })).toBeInTheDocument();
    expect(screen.getByLabelText("Time window")).toHaveDisplayValue("Last 24 hours");

    await user.click(screen.getByRole("button", { name: "Insight" }));
    expect(await screen.findByLabelText("Scope")).toHaveDisplayValue(
      "Workflow · core / implementation",
    );
    expect(screen.getByLabelText("Time window")).toHaveDisplayValue("Last 24 hours");
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
