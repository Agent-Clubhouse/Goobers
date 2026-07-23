import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient } from "../api/fixtureClient";
import type { DaemonEventStream, DaemonUpdateEvent } from "../api/types";
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

afterEach(() => {
  vi.useRealTimers();
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

  it("follows appended live events without overwriting a historical selection", async () => {
    vi.useFakeTimers();
    const runId = "01JZ441DAEMONAPI";
    const fixtures = populatedDaemonFixtures();
    const events = fixtures.runEvents?.[runId];
    const detail = fixtures.runDetails?.[runId];
    if (!events || !detail) {
      throw new Error("Expected active run fixtures.");
    }
    const client = new LiveFixtureClient(fixtures);
    renderRun(runId, client);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    events.events.push({
      schema: "v1",
      seq: 7,
      type: "gate.evaluated",
      branch: 0,
      time: "2026-07-18T06:00:07Z",
      knownSchema: true,
      gate: "review",
      attempt: 1,
      attemptClass: "initial",
      verdict: "needs-changes",
      target: "implement",
    });
    detail.lastSeq = 7;
    detail.currentStage = "implement";
    client.invalidateRun("fixture:1");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });

    expect(screen.getByRole("button", { name: /^Select sequence 7:/ })).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(
      screen.getByRole("button", { name: "review, gate, Completed at sequence 7" }),
    ).toHaveAttribute("aria-pressed", "true");

    fireEvent.click(screen.getByRole("button", { name: /^Select sequence 4:/ }));
    events.events.push({
      schema: "v1",
      seq: 8,
      type: "stage.started",
      branch: 0,
      time: "2026-07-18T06:00:08Z",
      knownSchema: true,
      stage: "implement",
      attempt: 2,
      attemptClass: "policy",
    });
    detail.lastSeq = 8;
    client.invalidateRun("fixture:2");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(50);
    });

    expect(screen.getByRole("button", { name: /^Select sequence 4:/ })).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(screen.getByRole("button", { name: /^Select sequence 8:/ })).not.toHaveAttribute(
      "aria-current",
    );
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

  it("renders pinned identity and narrow-layout semantics with the replay scrubber", async () => {
    Object.defineProperty(window, "innerWidth", { configurable: true, value: 480 });
    renderRun("01JZ300ABORTED");

    expect(
      await screen.findByText("sha256:tools", { selector: ".run-graph-pin .mono" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("group", { name: /pinned execution graph/ })).toHaveAttribute(
      "data-responsive-layout",
      "scroll-under-820",
    );
    expect(document.querySelector(".run-detail-workspace")).toHaveAttribute(
      "data-responsive-layout",
      "stack-under-820",
    );
    expect(
      screen.getByRole("button", { name: "implement, agentic, Aborted at sequence 5" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Play replay" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /attempt|escalation/i })).not.toBeInTheDocument();
  });
});

function renderRun(
  runId: string,
  client = new FixtureDaemonClient(populatedDaemonFixtures()),
) {
  window.location.hash = `#/run/${runId}`;
  return render(<App client={client} />);
}

class LiveFixtureClient extends FixtureDaemonClient {
  private readonly stream = new PushEventStream();

  override connectEvents(): Promise<DaemonEventStream> {
    return Promise.resolve(this.stream);
  }

  invalidateRun(id: string): void {
    this.stream.push({
      id,
      type: "invalidate",
      data: { cursor: id, models: ["run"] },
    });
  }
}

class PushEventStream implements DaemonEventStream {
  private closed = false;
  private readonly queue: DaemonUpdateEvent[] = [];
  private readonly readers: ((result: IteratorResult<DaemonUpdateEvent>) => void)[] = [];

  push(event: DaemonUpdateEvent): void {
    const reader = this.readers.shift();
    if (reader) {
      reader({ done: false, value: event });
    } else {
      this.queue.push(event);
    }
  }

  close(): void {
    if (this.closed) {
      return;
    }
    this.closed = true;
    for (const reader of this.readers.splice(0)) {
      reader({ done: true, value: undefined });
    }
  }

  [Symbol.asyncIterator](): AsyncIterator<DaemonUpdateEvent> {
    return {
      next: () => {
        const event = this.queue.shift();
        if (event) {
          return Promise.resolve({ done: false, value: event });
        }
        if (this.closed) {
          return Promise.resolve({ done: true, value: undefined });
        }
        return new Promise((resolve) => this.readers.push(resolve));
      },
    };
  }
}
