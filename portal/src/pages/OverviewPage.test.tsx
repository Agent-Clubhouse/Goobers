import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { emptyDaemonFixtures, populatedDaemonFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/overview";
  window.localStorage.clear();
});

describe("overview page", () => {
  it("shows durable root identity and warns for a historical root", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.instance.computerName = "MDB5";
    fixtures.instance.rootIdentity = {
      id: "0123456789abcdef0123456789abcdef",
      decommissionedAt: "2026-09-08T08:00:00Z",
      decommissionReason: "migrated to replacement",
    };
    render(<App client={new FixtureDaemonClient(fixtures)} />);
    await screen.findByText("0123456789abcdef0123456789abcdef");
    const tooltip = document.getElementById("portal-context-tooltip");
    expect(tooltip).not.toBeNull();
    expect(tooltip).toHaveTextContent("0123456789abcdef0123456789abcdef");
    expect(tooltip).toHaveTextContent(fixtures.instance.instanceRoot);
    expect(tooltip).toHaveTextContent("MDB5");
    expect(screen.getByText(/Historical root; do not use/)).toHaveTextContent("migrated to replacement");
  });

  it("shows the authoritative daemon binary version from health metadata", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.health.build = {
      version: "v1.2.3",
      commit: "abcdef0123456789",
      date: "2026-09-10T00:00:00Z",
    };

    render(<App client={new FixtureDaemonClient(fixtures)} />);

    await screen.findByText("v1.2.3 · abcdef0123456789");
    expect(document.getElementById("portal-context-tooltip")).toHaveTextContent(
      "v1.2.3 · abcdef0123456789",
    );
  });

  it("renders fixture-driven attention, active, and recent run groups", async () => {
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    expect(await screen.findByRole("heading", { name: "2 runs need attention." })).toBeInTheDocument();
    expect(
      within(screen.getByRole("region", { name: "Active runs" })).getByRole("link", {
        name: "Open run 01JZ441DAEMONAPI",
      }),
    ).toBeInTheDocument();
    expect(
      within(screen.getByRole("region", { name: "Recent outcomes" })).getByRole("link", {
        name: "Open run 01JZ455ESCALATE",
      }),
    ).toBeInTheDocument();
    const active = within(screen.getByRole("region", { name: "Active runs" }));
    expect(active.getByText("#3088 Operator status progress")).toBeInTheDocument();
    expect(active.getByText("core / implementation")).toBeInTheDocument();
    expect(active.getByText("review · recent heartbeat 30s ago · claim active/verified")).toBeInTheDocument();
    expect(active.getByText("review · PR via open-pr · finish review")).toBeInTheDocument();
    expect(
      active.getByText(
        "Error provider.rate_limit: quota exhausted · Review needs-changes: Show operator context. · Blockers: provider quota is exhausted",
      ),
    ).toBeInTheDocument();
  });

  it("shows an accessible local completion time for every recent outcome", async () => {
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const recent = within(await screen.findByRole("region", { name: "Recent outcomes" }));
    const completionTimes = recent.getAllByText(/^Completed /, { selector: "time" });
    expect(completionTimes).toHaveLength(2);

    const finishedAt = "2026-07-18T03:00:00Z";
    const completionTime = completionTimes.find(
      (time) => time.getAttribute("datetime") === finishedAt,
    );
    const timestamp = new Date(finishedAt);
    const visibleTime = new Intl.DateTimeFormat(undefined, {
      dateStyle: "medium",
      timeStyle: "short",
    }).format(timestamp);
    const preciseTime = new Intl.DateTimeFormat(undefined, {
      dateStyle: "full",
      timeStyle: "long",
    }).format(timestamp);

    expect(completionTime).toHaveTextContent(`Completed ${visibleTime}`);
    expect(completionTime).toHaveAccessibleName(`Completed ${preciseTime}`);
    expect(completionTime).toHaveAttribute("title", `Completed ${preciseTime}`);
  });

  it("groups repeated attention runs by linked issue and expands direct run links", async () => {
    const fixtures = populatedDaemonFixtures();
    const repeated = fixtures.runs.runs.filter(
      (run) => run.phase === "failed" || run.phase === "escalated",
    );
    for (const run of repeated) {
      run.operator = {
        issue: { number: "4449", title: "Repeated implementation failure" },
        liveness: "finished",
        trajectory: "blocked",
        claim: { leaseStatus: "released", providerMarker: "verified" },
        potentialBlockers: [],
      };
    }
    const user = userEvent.setup();

    render(<App client={new FixtureDaemonClient(fixtures)} />);

    expect(await screen.findByText("#4449 Repeated implementation failure")).toBeInTheDocument();
    expect(screen.getByText(/2 runs ·/)).toBeInTheDocument();
    expect(
      screen.queryByText("implementation · 01JZ402DASHBOARD"),
    ).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Show runs" }));
    expect(
      screen.getByRole("link", { name: /01JZ402DASHBOARD/ }),
    ).toHaveAttribute("href", "#/run/01JZ402DASHBOARD");
    expect(
      screen.getByRole("link", { name: /01JZ400FAILED/ }),
    ).toHaveAttribute("href", "#/run/01JZ400FAILED");

    const groupSelection = screen.getByRole("checkbox", {
      name: "Select all 2 runs in #4449 Repeated implementation failure",
    });
    await user.click(groupSelection);
    expect(screen.getByRole("button", { name: "Dismiss 2 selected" })).toBeInTheDocument();
    await user.click(groupSelection);
    expect(screen.queryByRole("button", { name: /Dismiss \d+ selected/ })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Dismiss run 01JZ402DASHBOARD" }));
    expect(await screen.findByRole("heading", { name: "One run needs attention." })).toBeInTheDocument();
    await user.click(screen.getByRole("button", {
      name: "Dismiss all runs in #4449 Repeated implementation failure",
    }));
    expect(
      await screen.findByRole("heading", { name: "Daemon is running — Healthy." }),
    ).toBeInTheDocument();
  });

  it("selects, deselects, dismisses, and restores all visible attention runs", async () => {
    const user = userEvent.setup();
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const selectAll = await screen.findByRole("checkbox", {
      name: "Select all visible attention runs",
    });
    await user.click(selectAll);
    expect(selectAll).toBeChecked();
    expect(screen.getByRole("button", { name: "Dismiss 2 selected" })).toBeInTheDocument();

    await user.click(selectAll);
    expect(selectAll).not.toBeChecked();
    expect(screen.queryByRole("button", { name: /Dismiss \d+ selected/ })).not.toBeInTheDocument();

    await user.click(selectAll);
    await user.click(screen.getByRole("button", { name: "Dismiss 2 selected" }));
    expect(
      await screen.findByRole("heading", { name: "Daemon is running — Healthy." }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("checkbox", {
      name: "Select all visible attention runs",
    })).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Show dismissed (2)" }));
    await user.click(screen.getByRole("button", { name: "Restore all" }));
    expect(await screen.findByRole("heading", { name: "2 runs need attention." })).toBeInTheDocument();
  });

  it("renders instance identity while an empty inventory is still loading", async () => {
    const client = new FixtureDaemonClient(emptyDaemonFixtures());
    const realListGaggles = client.listGaggles.bind(client);
    let releaseInventory: () => void = () => {};
    const inventoryGate = new Promise<void>((resolve) => {
      releaseInventory = resolve;
    });
    vi.spyOn(client, "listGaggles").mockImplementation(async (...args) => {
      await inventoryGate;
      return realListGaggles(...args);
    });

    render(<App client={client} />);

    expect(
      await screen.findByRole("status", { name: "Loading overview" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/Loading inventory and run activity/)).toBeInTheDocument();
    expect(
      screen.queryByRole("heading", { name: "Connecting to Goobers Instance" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "No gaggles configured" })).not.toBeInTheDocument();
    expect(screen.queryByText("goobers init --guided")).not.toBeInTheDocument();

    act(() => releaseInventory());
    expect(
      await screen.findByRole("heading", { name: "No gaggles configured" }),
    ).toBeInTheDocument();
  });

  it("shows retryable partial inventory failure without empty-instance recovery guidance", async () => {
    const client = new FixtureDaemonClient(emptyDaemonFixtures());
    const realListGaggles = client.listGaggles.bind(client);
    let failInventory = true;
    vi.spyOn(client, "listGaggles").mockImplementation((...args) => {
      if (failInventory) {
        return Promise.reject(new Error("inventory temporarily unavailable"));
      }
      return realListGaggles(...args);
    });
    const user = userEvent.setup();

    render(<App client={client} />);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      /inventory could not be read just now/i,
    );
    expect(screen.queryByRole("heading", { name: "No gaggles configured" })).not.toBeInTheDocument();
    expect(screen.queryByText("goobers init --guided")).not.toBeInTheDocument();

    failInventory = false;
    await user.click(screen.getByRole("button", { name: "Retry inventory" }));
    expect(
      await screen.findByRole("heading", { name: "No gaggles configured" }),
    ).toBeInTheDocument();
  });

  // #3346: what the reader could not verify is labelled as such and never joins
  // the run's blockers, so a credential-less read cannot make a healthy run look
  // blocked.
  it("labels reader-side diagnostics limitations separately from run blockers", async () => {
    const fixtures = populatedDaemonFixtures();
    const running = fixtures.runs.runs.find((summary) => summary.id === "01JZ441DAEMONAPI");
    if (!running?.operator) {
      throw new Error("fixture is missing the running run's operator summary");
    }
    running.operator = {
      ...running.operator,
      latestError: undefined,
      review: undefined,
      potentialBlockers: [],
      diagnosticsLimitations: [
        "provider claim marker verification unavailable: no credential in GOOBERS_CRED_GITHUB_ISSUES_READ env var",
      ],
    };

    render(<App client={new FixtureDaemonClient(fixtures)} />);

    const active = within(await screen.findByRole("region", { name: "Active runs" }));
    expect(
      active.getByText(
        "Diagnostics limited (not a run blocker): provider claim marker verification unavailable: no credential in GOOBERS_CRED_GITHUB_ISSUES_READ env var",
      ),
    ).toBeInTheDocument();
    expect(active.queryByText(/Blockers:/)).not.toBeInTheDocument();
  });

  it("renders a completed retention sweep status", async () => {
    const completed = populatedDaemonFixtures();
    completed.instance.maintenance = {
      kind: "retention-sweep",
      state: "completed",
      trigger: "periodic",
      lastCompletedAt: "2026-09-05T21:30:00Z",
      candidates: 7,
      removed: 4,
      failures: 0,
      lastResult: "completed",
    };

    render(<App client={new FixtureDaemonClient(completed)} />);
    expect(
      await screen.findByRole("status", { name: "Retention sweep completed" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/periodic trigger/i)).toBeInTheDocument();
  });

  it("renders telemetry retention policy and latest dry-run pass", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.instance.telemetryRetention = {
      enabled: true,
      window: "30d",
      maxRuns: 900,
      firstEnable: "gracePeriod",
      enforceAt: "2026-09-19T08:00:00Z",
      lastPassAt: "2026-09-12T08:00:00Z",
      lastPassMode: "dry-run",
      candidateCount: 17,
    };

    render(<App client={new FixtureDaemonClient(fixtures)} />);
    expect(
      await screen.findByRole("status", { name: "Telemetry retention enabled" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/30 days · Max 900 runs/)).toBeInTheDocument();
    expect(screen.getByText(/last pass dry-run/i)).toHaveTextContent("17 candidates");
    expect(screen.getByText(/enforcement begins/i)).toBeInTheDocument();
    expect(screen.queryByText(/Restart required/)).not.toBeInTheDocument();
  });

  it("does not present a stale enforcement date for a disabled retention policy", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.instance.telemetryRetention = {
      enabled: false,
      window: "30d",
      maxRuns: 900,
      firstEnable: "gracePeriod",
      enforceAt: "2026-09-19T08:00:00Z",
      lastPassAt: "2026-09-12T08:00:00Z",
      lastPassMode: "dry-run",
      candidateCount: 17,
    };

    render(<App client={new FixtureDaemonClient(fixtures)} />);
    expect(
      await screen.findByRole("status", { name: "Telemetry retention disabled" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/last pass dry-run/i)).toHaveTextContent("17 candidates");
    expect(screen.queryByText(/enforcement begins/)).not.toBeInTheDocument();
  });

  it("renders a failed retention sweep status", async () => {
    const failed = populatedDaemonFixtures();
    failed.instance.maintenance = {
      kind: "retention-sweep",
      state: "failed",
      trigger: "periodic",
      startedAt: "2026-09-05T21:32:00Z",
      lastProgressAt: "2026-09-05T21:32:10Z",
      currentPhase: "projection-retention",
      candidates: 9,
      removed: 5,
      failures: 1,
      lastResult: "failed",
      errorSummary: "git remote timed out",
    };
    render(<App client={new FixtureDaemonClient(failed)} />);
    render(<App client={new FixtureDaemonClient(failed)} />);
    const alert = await screen.findByRole("alert", { name: "Retention sweep failed" });
    expect(alert).toHaveTextContent("git remote timed out");
    expect(alert).toHaveTextContent(/periodic trigger/i);
  });

  it("renders a cancelled retention sweep status", async () => {
    const cancelled = populatedDaemonFixtures();
    cancelled.instance.maintenance = {
      kind: "retention-sweep",
      state: "cancelled",
      trigger: "periodic",
      lastCompletedAt: "2026-09-05T21:28:00Z",
      candidates: 0,
      removed: 0,
      failures: 0,
      lastResult: "cancelled",
    };

    render(<App client={new FixtureDaemonClient(cancelled)} />);
    expect(
      await screen.findByRole("status", { name: "Retention sweep cancelled" }),
    ).toBeInTheDocument();
  });

  it("renders live sweep progress with elapsed and recent activity details", async () => {
    const running = populatedDaemonFixtures();
    running.instance.maintenance = {
      kind: "retention-sweep",
      state: "running",
      trigger: "startup",
      startedAt: new Date(Date.now() - 3 * 60 * 1000).toISOString(),
      lastProgressAt: new Date(Date.now() - 15 * 1000).toISOString(),
      currentPhase: "projection-retention",
      candidates: 23,
      removed: 12,
      failures: 0,
      lastResult: "running",
    };

    render(<App client={new FixtureDaemonClient(running)} />);
    expect(
      await screen.findByRole("status", { name: "Retention sweep running" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/last progress/i)).toBeInTheDocument();
    expect(screen.getByText(/projection-retention/i)).toBeInTheDocument();
  });

  it("renders the latest completed sweep when no sweep is running", async () => {
    const noSweep = populatedDaemonFixtures();
    noSweep.instance.maintenance = {
      kind: "retention-sweep",
      state: "none",
      trigger: "periodic",
      lastCompletedAt: "2026-09-05T21:25:00Z",
      candidates: 0,
      removed: 0,
      failures: 0,
      lastResult: "completed",
    };

    render(<App client={new FixtureDaemonClient(noSweep)} />);
    expect(
      await screen.findByRole("status", { name: "No retention sweep running" }),
    ).toBeInTheDocument();
    expect(screen.getByText(/last completed at/i)).toBeInTheDocument();
  });
});

// #3658: a phase whose query failed used to render as an empty group, so the
// page claimed there was no recent activity when it simply could not read it.
describe("overview recovery inventory capacity (#5343)", () => {
  it("reports occupancy and the effective limit when the inventory is healthy", async () => {
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    const row = await screen.findByRole("group", { name: "Recovery inventory healthy" });
    expect(row).toHaveTextContent("12/128 slots");
    expect(row).toHaveTextContent("(9%)");
    expect(row).toHaveTextContent(
      "Durable handoffs, worktree cleanup and unrelated runs fail when it is full.",
    );
    expect(within(row).getByRole("link", { name: "Recovery capacity and operator actions" })).toHaveAttribute(
      "href",
      expect.stringContaining("retained-implementation.md#inventory-capacity"),
    );
  });

  it("elevates the high-water warning and names the consequence", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.instance.recoveryInventory = {
      ...fixtures.instance.recoveryInventory!,
      state: "warning",
      used: 104,
      unreadable: 2,
    };

    render(<App client={new FixtureDaemonClient(fixtures)} />);

    const row = await screen.findByRole("alert", { name: "Recovery inventory warning" });
    expect(row.className).toContain("instance-summary-row-warning");
    expect(row).toHaveTextContent("104/128 slots");
    expect(row).toHaveTextContent("passed the 80% high-water mark");
    expect(row).toHaveTextContent("2 incomplete reservations still occupying slots");
  });

  it("elevates exhaustion as critical and says unrelated runs fail", async () => {
    const fixtures = populatedDaemonFixtures();
    fixtures.instance.recoveryInventory = {
      ...fixtures.instance.recoveryInventory!,
      state: "exhausted",
      used: 128,
      unreadable: 1,
    };

    render(<App client={new FixtureDaemonClient(fixtures)} />);

    const row = await screen.findByRole("alert", { name: "Recovery inventory exhausted" });
    expect(row.className).toContain("instance-summary-row-error");
    expect(row).toHaveTextContent("Full");
    expect(row).toHaveTextContent("128/128 slots");
    expect(row).toHaveTextContent(
      "Durable handoffs, worktree cleanup and unrelated runs fail until capacity is freed.",
    );
    expect(row).toHaveTextContent("1 incomplete reservation still occupying slots");
  });

  it("omits the card entirely when the daemon reports no reading", async () => {
    const fixtures = populatedDaemonFixtures();
    delete fixtures.instance.recoveryInventory;

    render(<App client={new FixtureDaemonClient(fixtures)} />);

    await screen.findByRole("region", { name: "Active runs" });
    expect(screen.queryByText("Recovery inventory")).not.toBeInTheDocument();
  });
});

describe("overview partial run-phase failures (#3658)", () => {
  it("warns that the run groups are incomplete when one phase query fails", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const real = client.listRuns.bind(client);
    vi.spyOn(client, "listRuns").mockImplementation(async (request, options) => {
      if (request?.phase === "completed") {
        throw new Error("The daemon request timed out after 10000ms.");
      }
      return real(request, options);
    });

    render(<App client={client} />);

    const warning = await screen.findByRole("alert");
    expect(warning).toHaveTextContent(/Run activity for the completed phase could not be read/);
    // The phases that did read are still rendered.
    expect(
      within(screen.getByRole("region", { name: "Active runs" })).getByRole("link", {
        name: "Open run 01JZ441DAEMONAPI",
      }),
    ).toBeInTheDocument();
  });

  it("shows no incomplete-data warning when every phase reads successfully", async () => {
    render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);

    await screen.findByRole("region", { name: "Active runs" });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
