import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

const canonicalRuns = [
  ["01JZ441DAEMONAPI", "Running"],
  ["01JZ455ESCALATE", "Completed"],
  ["01JZ400FAILED", "Failed"],
  ["01JZ300ABORTED", "Aborted"],
  ["01JZ402DASHBOARD", "Escalated"],
] as const;

beforeEach(() => {
  window.location.hash = "#/overview";
});

describe("run detail", () => {
  it.each(canonicalRuns)("deep-links %s with canonical %s status", async (runId, status) => {
    renderRun(runId);

    expect(
      await screen.findByRole("heading", { name: `Run ${runId}` }),
    ).toBeInTheDocument();
    expect(screen.getByText(status, { selector: ".status-badge" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Execution graph" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Event ledger" })).toBeInTheDocument();
  });

  it("defaults an active run to the latest event and synchronizes click selection", async () => {
    const user = userEvent.setup();
    renderRun("01JZ441DAEMONAPI");

    const latest = await screen.findByRole("button", { name: /^Select sequence 6:/ });
    expect(latest).toHaveAttribute("aria-current", "true");
    expect(
      screen.getByRole("button", { name: "review, gate, Running at sequence 6" }),
    ).toHaveAttribute("aria-pressed", "true");

    await user.click(screen.getByRole("button", { name: /^Select sequence 4:/ }));

    expect(
      screen.getByRole("button", { name: "implement, agentic, Running at sequence 4" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(
      screen.getByRole("button", { name: "review, gate, Pending at sequence 4" }),
    ).toBeInTheDocument();
  });

  it("keeps repasses on one graph node and exposes attempts in sequence", async () => {
    renderRun("01JZ402DASHBOARD");

    const graph = await screen.findByRole("group", {
      name: "implementation pinned execution graph",
    });
    const ledger = screen.getByRole("region", { name: "Event ledger" });
    expect(within(graph).getAllByRole("button", { name: /^implement,/ })).toHaveLength(1);
    expect(within(ledger).getAllByText("Attempt 2")).toHaveLength(4);
    expect(
      screen.getByRole("button", { name: "review, gate, Escalated at sequence 12" }),
    ).toBeInTheDocument();
  });

  it("keeps an unknown schema visible without rendering its raw payload", async () => {
    renderRun("01JZ455ESCALATE");

    expect(await screen.findByText("Unsupported schema v2-preview")).toBeInTheDocument();
    expect(screen.getByText("Type future.recorded")).toBeInTheDocument();
    expect(screen.queryByText(/preserved but not rendered/)).not.toBeInTheDocument();
  });

  it("operates the ledger and graph with directional keys", async () => {
    renderRun("01JZ441DAEMONAPI");

    const sequenceFour = await screen.findByRole("button", { name: /^Select sequence 4:/ });
    sequenceFour.focus();
    fireEvent.keyDown(sequenceFour, { key: "ArrowDown" });

    const sequenceFive = screen.getByRole("button", { name: /^Select sequence 5:/ });
    expect(sequenceFive).toHaveFocus();
    expect(sequenceFive).toHaveAttribute("aria-current", "true");
    const queryNode = screen.getByRole("button", {
      name: "query, deterministic, Completed at sequence 5",
    });
    const implementNode = screen.getByRole("button", {
      name: "implement, agentic, Completed at sequence 5",
    });
    queryNode.focus();
    fireEvent.keyDown(queryNode, { key: "ArrowRight" });
    expect(implementNode).toHaveFocus();
    expect(implementNode).toHaveAttribute("aria-pressed", "true");
  });

  it("renders pinned identity and narrow-layout semantics without later-slice UI", async () => {
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 480 });
    renderRun("01JZ300ABORTED");

    expect(
      await screen.findByText("sha256:tools", { selector: ".run-graph-pin .mono" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("group", { name: /pinned execution graph/ })).toHaveAttribute(
      "data-responsive-layout",
      "compact-under-820",
    );
    expect(document.querySelector(".run-detail-workspace")).toHaveAttribute(
      "data-responsive-layout",
      "stack-under-820",
    );
    expect(screen.queryByRole("button", { name: /play|replay/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /attempt|escalation/i })).not.toBeInTheDocument();
  });
});

function renderRun(runId: string) {
  window.location.hash = `#/run/${runId}`;
  return render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);
}
