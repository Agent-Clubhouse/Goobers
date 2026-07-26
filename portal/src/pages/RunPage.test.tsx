import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient, fixtureKey } from "../api/fixtureClient";
import type { DaemonEventStream, DaemonUpdateEvent } from "../api/types";
import { populatedDaemonFixtures } from "../test/daemonFixtures";

const portalStyles = readFileSync("src/styles.css", "utf8");

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

  it("reveals and focuses the inspector for direct graph and journal selections", async () => {
    const user = userEvent.setup();
    const previousScrollIntoView = Object.getOwnPropertyDescriptor(
      HTMLElement.prototype,
      "scrollIntoView",
    );
    const scrollIntoView = vi.fn();
    Object.defineProperty(HTMLElement.prototype, "scrollIntoView", {
      configurable: true,
      value: scrollIntoView,
    });

    try {
      renderRun("01JZ441DAEMONAPI");
      await user.click(
        await screen.findByRole("button", {
          name: "query, deterministic, Completed at sequence 6",
        }),
      );

      let inspector = screen.getByRole("complementary", { name: "query attempt inspector" });
      expect(inspector).toHaveFocus();
      expect(scrollIntoView).toHaveBeenLastCalledWith({
        block: "start",
        inline: "nearest",
      });

      await user.click(screen.getByRole("button", { name: /^Select sequence 4:/ }));
      inspector = screen.getByRole("complementary", { name: "implement attempt inspector" });
      expect(inspector).toHaveFocus();
      expect(scrollIntoView).toHaveBeenCalledTimes(2);
    } finally {
      if (previousScrollIntoView) {
        Object.defineProperty(
          HTMLElement.prototype,
          "scrollIntoView",
          previousScrollIntoView,
        );
      } else {
        Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
      }
    }
  });

  it("keeps a tall inspector and long journal in page flow", async () => {
    const runId = "01JZ441DAEMONAPI";
    const fixtures = populatedDaemonFixtures();
    const eventList = fixtures.runEvents?.[runId];
    const detail = fixtures.runDetails?.[runId];
    const lastEvent = eventList?.events.at(-1);
    if (!eventList || !detail || !lastEvent) {
      throw new Error("Expected active run fixtures.");
    }
    eventList.events.push(
      ...Array.from({ length: 34 }, (_, index) => ({
        ...lastEvent,
        seq: index + 7,
        time: `2026-07-18T06:00:${String(index + 7).padStart(2, "0")}Z`,
      })),
    );
    detail.lastSeq = 40;
    const artifacts = Array.from({ length: 30 }, (_, index) => ({
      name: `artifact-${index + 1}.txt`,
      digest: `sha256:${String(index + 1).padStart(64, "0")}`,
      size: 1024 + index,
      mediaType: "text/plain",
      recordedSeq: 40,
    }));
    fixtures.stageAttempts = {
      [fixtureKey(runId, "review")]: {
        runId,
        stage: "review",
        attempts: [
          {
            id: "sta-review-1",
            visit: 1,
            number: 1,
            class: "initial",
            status: "running",
            startedSeq: 6,
            durationMillis: 34_000,
            outputs: { digest: "a".repeat(180) },
            artifacts,
          },
        ],
      },
    };
    const previewText = "expanded preview\n".repeat(80);
    fixtures.artifacts = {
      [fixtureKey(runId, artifacts[0].digest)]: {
        digest: artifacts[0].digest,
        mediaType: "text/plain",
        size: previewText.length,
        etag: null,
        bytes: new TextEncoder().encode(previewText).buffer,
      },
    };
    renderRun(runId, new FixtureDaemonClient(fixtures));

    expect(await screen.findByText("artifact-30.txt")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /^Select sequence/ })).toHaveLength(40);
    const workspace = document.querySelector(".run-detail-workspace");
    const inspector = screen.getByRole("complementary", { name: "review attempt inspector" });
    const ledger = screen.getByRole("region", { name: "Event ledger" });
    expect(workspace).toHaveAttribute("data-scroll-owner", "page");
    expect(
      inspector.compareDocumentPosition(ledger) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();

    fireEvent.click(screen.getAllByRole("button", { name: "View content" })[0]);
    expect(await screen.findByText(/expanded preview/)).toBeInTheDocument();

    expect(portalStyles).not.toMatch(
      /\.run-inspector\s*\{[^}]*position:\s*sticky/s,
    );
    expect(portalStyles).toMatch(
      /\.artifact-content\s*\{[^}]*overflow:\s*visible/s,
    );
    expect(portalStyles).toMatch(
      /\.output-line code\s*\{[^}]*overflow-wrap:\s*anywhere/s,
    );
  });

  it("keeps replay and stage inspection in the fallback fullscreen workspace", async () => {
    const user = userEvent.setup();
    renderRun("01JZ441DAEMONAPI");

    const graph = await screen.findByRole("group", {
      name: "implementation pinned execution graph",
    });
    const fullscreenRoot = graph.closest(".run-graph-fullscreen-root");
    if (!(fullscreenRoot instanceof HTMLElement)) {
      throw new Error("Expected run graph fullscreen root.");
    }

    await user.click(within(fullscreenRoot).getByRole("button", { name: "Fullscreen" }));

    expect(fullscreenRoot).toHaveClass("workflow-graph-shell-expanded");
    expect(fullscreenRoot).toHaveAttribute("data-fullscreen", "fallback");
    expect(fullscreenRoot).toHaveAttribute("role", "dialog");
    const replayControls = within(fullscreenRoot).getByRole("group", {
      name: "Replay controls",
    });
    const scrubber = within(replayControls).getByRole("slider", {
      name: "Scrub to event",
    });
    const inspector = within(fullscreenRoot).getByRole("complementary", {
      name: /attempt inspector/,
    });
    expect(replayControls).toBeInTheDocument();
    expect(inspector).toBeInTheDocument();
    expect(
      within(fullscreenRoot).queryByRole("heading", { name: "Event ledger" }),
    ).not.toBeInTheDocument();

    inspector.focus();
    fireEvent.keyDown(window, { key: "Tab" });
    expect(
      within(fullscreenRoot).getByRole("button", { name: "Zoom out" }),
    ).toHaveFocus();

    inspector.focus();
    expect(inspector).toHaveFocus();
    fireEvent.keyDown(window, { key: "Tab", shiftKey: true });
    expect(scrubber).toHaveFocus();

    fireEvent.keyDown(window, { key: "Escape" });
    expect(fullscreenRoot).not.toHaveClass("workflow-graph-shell-expanded");
    expect(screen.getByRole("button", { name: "Fullscreen" })).toHaveFocus();
  });

  it("requests native fullscreen for the complete run graph workspace", async () => {
    renderRun("01JZ441DAEMONAPI");

    const graph = await screen.findByRole("group", {
      name: "implementation pinned execution graph",
    });
    const fullscreenRoot = graph.closest(".run-graph-fullscreen-root");
    if (!(fullscreenRoot instanceof HTMLElement)) {
      throw new Error("Expected run graph fullscreen root.");
    }
    const fullscreenDescriptor = Object.getOwnPropertyDescriptor(
      document,
      "fullscreenElement",
    );
    const exitFullscreenDescriptor = Object.getOwnPropertyDescriptor(
      document,
      "exitFullscreen",
    );
    let fullscreenElement: Element | null = null;
    Object.defineProperty(document, "fullscreenElement", {
      configurable: true,
      get: () => fullscreenElement,
    });
    const requestFullscreen = vi.fn(async () => {
      fullscreenElement = fullscreenRoot;
      document.dispatchEvent(new Event("fullscreenchange"));
    });
    const exitFullscreen = vi.fn(async () => {
      fullscreenElement = null;
      document.dispatchEvent(new Event("fullscreenchange"));
    });
    Object.defineProperty(fullscreenRoot, "requestFullscreen", {
      configurable: true,
      value: requestFullscreen,
    });
    Object.defineProperty(document, "exitFullscreen", {
      configurable: true,
      value: exitFullscreen,
    });

    try {
      fireEvent.click(
        within(fullscreenRoot).getByRole("button", { name: "Fullscreen" }),
      );
      await waitFor(() =>
        expect(fullscreenRoot).toHaveAttribute("data-fullscreen", "native"),
      );
      expect(requestFullscreen).toHaveBeenCalledOnce();
      expect(
        within(fullscreenRoot).getByRole("group", { name: "Replay controls" }),
      ).toBeInTheDocument();

      fireEvent.click(
        within(fullscreenRoot).getByRole("button", { name: "Exit fullscreen" }),
      );
      await waitFor(() =>
        expect(fullscreenRoot).toHaveAttribute("data-fullscreen", "none"),
      );
      expect(exitFullscreen).toHaveBeenCalledOnce();
    } finally {
      if (fullscreenDescriptor) {
        Object.defineProperty(
          document,
          "fullscreenElement",
          fullscreenDescriptor,
        );
      } else {
        Reflect.deleteProperty(document, "fullscreenElement");
      }
      if (exitFullscreenDescriptor) {
        Object.defineProperty(
          document,
          "exitFullscreen",
          exitFullscreenDescriptor,
        );
      } else {
        Reflect.deleteProperty(document, "exitFullscreen");
      }
    }
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
    const implement = within(graph).getByRole("button", { name: /^implement,/ });
    expect(within(graph).getAllByRole("button", { name: /^implement,/ })).toHaveLength(1);
    expect(
      screen.getByRole("button", { name: "review, gate, Escalated at sequence 12" }),
    ).toBeInTheDocument();

    fireEvent.click(implement);
    const visits = await screen.findByRole("group", { name: "Stage visits" });
    const visit1 = within(visits).getByRole("button", { name: "Visit 1" });
    const visit2 = within(visits).getByRole("button", { name: "Visit 2" });
    expect(visit2).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("Addressed reviewer feedback.")).toBeInTheDocument();
    const repassContext = screen.getByText("Repass decision · Sequence 7").parentElement;
    if (!repassContext) {
      throw new Error("Expected repass decision context.");
    }
    expect(
      within(repassContext).getByText("Review returned needs-changes and selected implement."),
    ).toBeInTheDocument();

    fireEvent.click(visit1);
    expect(screen.getByText("Initial implementation.")).toBeInTheDocument();
    expect(screen.queryByText(/Repass decision/)).not.toBeInTheDocument();
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
