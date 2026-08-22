import { render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

beforeEach(() => {
  window.location.hash = "#/overview";
});

describe("overview page", () => {
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
    expect(active.getByText("review · recent heartbeat 30s ago · claim active/verified")).toBeInTheDocument();
    expect(active.getByText("review · PR via open-pr · finish review")).toBeInTheDocument();
    expect(
      active.getByText(
        "Error provider.rate_limit: quota exhausted · Review needs-changes: Show operator context. · Blockers: provider quota is exhausted",
      ),
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
});
