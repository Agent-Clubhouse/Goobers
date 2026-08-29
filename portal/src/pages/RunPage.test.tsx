import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "../App";
import { FixtureDaemonClient, fixtureKey } from "../api/fixtureClient";
import type {
  DaemonEventStream,
  DaemonUpdateEvent,
  EventList,
  RequestOptions,
  RunDetail,
  RunEvent,
} from "../api/types";
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

  it("renders stale run detail as visibly unmonitored", async () => {
    const fixtures = populatedDaemonFixtures();
    const detail = fixtures.runDetails?.["01JZ441DAEMONAPI"];
    if (!detail) {
      throw new Error("Expected active run detail fixture.");
    }
    detail.stale = true;
    renderRun("01JZ441DAEMONAPI", new FixtureDaemonClient(fixtures));

    expect(
      await screen.findByText("Stale / unmonitored", { selector: ".status-badge" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText("No recent run activity is available and the daemon heartbeat is stale."),
    ).toBeInTheDocument();
  });

  it("reveals the run directory when the local capability is available", async () => {
    const user = userEvent.setup();
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const revealRun = vi.spyOn(client, "revealRun").mockResolvedValue();
    renderRun("01JZ441DAEMONAPI", client);

    await user.click(await screen.findByRole("button", { name: "Reveal run files" }));

    expect(revealRun).toHaveBeenCalledWith("01JZ441DAEMONAPI");
  });

  it("hides run reveal when the daemon is not local", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const config = await client.getPortalConfig();
    vi.spyOn(client, "getPortalConfig").mockResolvedValue({
      ...config,
      capabilities: { revealRun: false },
    });
    renderRun("01JZ441DAEMONAPI", client);

    await screen.findByRole("heading", { name: "Run 01JZ441DAEMONAPI" });
    await waitFor(() => {
      expect(screen.queryByRole("button", { name: "Reveal run files" })).not.toBeInTheDocument();
    });
  });

  it("reports a failure to open the run directory", async () => {
    const user = userEvent.setup();
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    vi.spyOn(client, "revealRun").mockRejectedValue(new Error("Unable to open run files."));
    renderRun("01JZ441DAEMONAPI", client);

    await user.click(await screen.findByRole("button", { name: "Reveal run files" }));

    expect(
      await screen.findByText("Unable to open run files.", { selector: '[role="alert"]' }),
    ).toBeInTheDocument();
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

      let inspector = screen.getByRole("complementary", { name: "implementation · query attempt inspector" });
      expect(inspector).toHaveFocus();
      expect(scrollIntoView).toHaveBeenLastCalledWith({
        block: "start",
        inline: "nearest",
      });

      await user.click(screen.getByRole("button", { name: /^Select sequence 4:/ }));
      inspector = screen.getByRole("complementary", { name: "implementation · implement attempt inspector" });
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
    const inspector = screen.getByRole("complementary", { name: "implementation · review attempt inspector" });
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

  it("synchronizes graph, journal, and inspector when a replay chapter is selected", async () => {
    const user = userEvent.setup();
    const fixtures = populatedDaemonFixtures();
    const eventList = fixtures.runEvents?.["01JZ455ESCALATE"];
    const stageStart = eventList?.events.find((event) => event.seq === 2);
    if (!stageStart) {
      throw new Error("Expected completed run stage chapter.");
    }
    stageStart.category = "transition";
    stageStart.replayChapter = true;
    renderRun("01JZ455ESCALATE", new FixtureDaemonClient(fixtures));

    await user.click(
      await screen.findByRole("button", {
        name: /Go to Workflow transition chapter at event 2: Stage started/,
      }),
    );

    expect(screen.getByRole("button", { name: /^Select sequence 2:/ })).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(
      screen.getByRole("button", { name: "query, deterministic, Running at sequence 2" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(
      screen.getByRole("complementary", { name: "implementation · query attempt inspector" }),
    ).toBeInTheDocument();
  });

  it("prioritizes major events while preserving grouped evidence and exact all-event order", async () => {
    const runId = "01JZ455ESCALATE";
    const fixtures = populatedDaemonFixtures();
    const eventList = fixtures.runEvents?.[runId];
    const detail = fixtures.runDetails?.[runId];
    if (!eventList || !detail) {
      throw new Error("Expected completed run fixtures.");
    }
    const recorded = (
      seq: number,
      type: RunEvent["type"],
      fields: Partial<RunEvent> = {},
    ): RunEvent => ({
      schema: "v1",
      seq,
      type,
      branch: 0,
      time: `2026-07-18T02:00:${String(seq).padStart(2, "0")}Z`,
      knownSchema: true,
      ...fields,
    });
    eventList.events = [
      recorded(1, "run.started", { category: "transition" }),
      recorded(2, "gate.started", {
        category: "bookkeeping",
        gate: "review",
        attempt: 1,
      }),
      recorded(3, "span.recorded", {
        category: "evidence",
        stage: `${runId}:review`,
        name: "reviewer.transcript",
      }),
      recorded(4, "artifact.recorded", {
        category: "evidence",
        artifact: {
          name: "verdict/review-1.json",
          digest: "sha256:verdict-1",
          size: 80,
          mediaType: "application/json",
          stage: "review",
          attempt: 1,
        },
      }),
      recorded(5, "gate.evaluated", {
        category: "decision",
        gate: "review",
        verdict: "needs-changes",
        target: "implement",
      }),
      recorded(6, "stage.started", {
        category: "transition",
        stage: "implement",
        attempt: 1,
        attemptClass: "initial",
      }),
      recorded(7, "stage.heartbeat", {
        category: "liveness",
        stage: "implement",
        attempt: 1,
      }),
      recorded(8, "stage.finished", {
        category: "transition",
        stage: "implement",
        attempt: 1,
        status: "success",
      }),
      recorded(9, "gate.started", {
        category: "bookkeeping",
        gate: "review",
        attempt: 1,
      }),
      recorded(10, "span.recorded", {
        category: "evidence",
        stage: `${runId}:review`,
        name: "reviewer.transcript",
      }),
      recorded(11, "artifact.recorded", {
        category: "evidence",
        artifact: {
          name: "verdict/review-2.json",
          digest: "sha256:verdict-2",
          size: 80,
          mediaType: "application/json",
          stage: "review",
          attempt: 1,
        },
      }),
      recorded(12, "gate.evaluated", {
        category: "decision",
        gate: "review",
        verdict: "pass",
        target: "@complete",
      }),
      recorded(13, "ref.touched", {
        category: "result",
        stage: "review",
        externalRef: {
          provider: "github",
          kind: "pr",
          id: "1432",
          url: "https://github.example/pull/1432",
        },
        runner: { operation: "open" },
      }),
      recorded(14, "future.recorded", {
        category: "unknown",
        schema: "v2-preview",
        knownSchema: false,
      }),
      recorded(15, "run.finished", {
        category: "transition",
        status: "completed",
      }),
    ];
    detail.lastSeq = 15;
    const transcriptOne = "Review visit 1 transcript: implementation needs changes.";
    const transcriptTwo = "Review visit 2 transcript: implementation passes.";
    const verdictOne = JSON.stringify({ decision: "needs-changes", target: "implement" });
    const verdictTwo = JSON.stringify({ decision: "pass", target: "@complete" });
    fixtures.transcripts = {
      [fixtureKey(runId, "3")]: {
        seq: 3,
        stage: "review",
        name: "reviewer.transcript",
        size: transcriptOne.length,
        bytes: new TextEncoder().encode(transcriptOne).buffer,
      },
      [fixtureKey(runId, "10")]: {
        seq: 10,
        stage: "review",
        name: "reviewer.transcript",
        size: transcriptTwo.length,
        bytes: new TextEncoder().encode(transcriptTwo).buffer,
      },
    };
    fixtures.artifacts = {
      [fixtureKey(runId, "sha256:verdict-1")]: {
        digest: "sha256:verdict-1",
        mediaType: "application/json",
        size: verdictOne.length,
        etag: '"sha256:verdict-1"',
        bytes: new TextEncoder().encode(verdictOne).buffer,
      },
      [fixtureKey(runId, "sha256:verdict-2")]: {
        digest: "sha256:verdict-2",
        mediaType: "application/json",
        size: verdictTwo.length,
        etag: '"sha256:verdict-2"',
        bytes: new TextEncoder().encode(verdictTwo).buffer,
      },
    };
    renderRun(runId, new FixtureDaemonClient(fixtures));

    const majorEvents = await screen.findByRole("button", { name: "Major events" });
    expect(majorEvents).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("Unsupported schema v2-preview")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Select sequence 3:/ })).not.toBeInTheDocument();

    const firstEvent = screen.getByRole("button", { name: /^Select sequence 1:/ });
    const firstGroup = screen.getByRole("button", {
      name: /Expand 3 supporting events for Review · Visit 1, sequences 2 through 4/,
    });
    firstEvent.focus();
    fireEvent.keyDown(firstEvent, { key: "ArrowDown" });
    expect(firstGroup).toHaveFocus();

    fireEvent.click(firstGroup);
    const gateStart = screen.getByRole("button", { name: /^Select sequence 2:/ });
    const transcript = screen.getByRole("button", { name: /^Select sequence 3:/ });
    expect(screen.getByText("Transcript for Review was recorded. Select this event to inspect the evidence.")).toBeInTheDocument();
    expect(
      screen.getByText(
        "verdict/review-1.json captured the Review decision: needs-changes selecting implement. Select this event to inspect the artifact.",
      ),
    ).toBeInTheDocument();
    fireEvent.keyDown(firstGroup, { key: "ArrowDown" });
    expect(gateStart).toHaveFocus();
    fireEvent.keyDown(gateStart, { key: "ArrowDown" });
    expect(transcript).toHaveFocus();

    fireEvent.click(transcript);
    expect(
      screen.getByRole("button", { name: "review, gate, Running at sequence 3" }),
    ).toHaveAttribute("aria-pressed", "true");
    let inspector = screen.getByRole("complementary", { name: "implementation · review attempt inspector" });
    expect(within(inspector).getByText("review evidence · Visit 1 · Sequence 3")).toBeInTheDocument();
    fireEvent.click(within(inspector).getByRole("button", { name: "View transcript" }));
    expect(await within(inspector).findByText(transcriptOne)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^Select sequence 4:/ }));
    inspector = screen.getByRole("complementary", { name: "implementation · review attempt inspector" });
    expect(within(inspector).getByText("review evidence · Visit 1 · Sequence 4")).toBeInTheDocument();
    fireEvent.click(within(inspector).getByRole("button", { name: "View content" }));
    expect(await within(inspector).findByText(verdictOne)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "All events (15)" }));
    expect(screen.getAllByRole("button", { name: /^Select sequence/ })).toHaveLength(15);
    expect(
      screen.getByText(
        "Implement attempt 1 reported liveness; workflow state did not change.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "verdict/review-2.json captured the Review decision: pass selecting @complete. Select this event to inspect the artifact.",
      ),
    ).toBeInTheDocument();
    expect(screen.getByText("GitHub opened pull request #1432.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open linked pull request" })).toHaveAttribute(
      "href",
      "https://github.example/pull/1432",
    );

    fireEvent.click(screen.getByRole("button", { name: /^Select sequence 10:/ }));
    inspector = screen.getByRole("complementary", { name: "implementation · review attempt inspector" });
    expect(within(inspector).getByText("review evidence · Visit 2 · Sequence 10")).toBeInTheDocument();
    fireEvent.click(within(inspector).getByRole("button", { name: "View transcript" }));
    expect(await within(inspector).findByText(transcriptTwo)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /^Select sequence 11:/ }));
    inspector = screen.getByRole("complementary", { name: "implementation · review attempt inspector" });
    expect(within(inspector).getByText("review evidence · Visit 2 · Sequence 11")).toBeInTheDocument();
    fireEvent.click(within(inspector).getByRole("button", { name: "View content" }));
    expect(await within(inspector).findByText(verdictTwo)).toBeInTheDocument();

    expect(
      screen.getAllByRole("button", { name: /^Select sequence/ }).map((button) =>
        Number(button.getAttribute("aria-label")?.match(/^Select sequence (\d+):/)?.[1]),
      ),
    ).toEqual(Array.from({ length: 15 }, (_, index) => index + 1));
  });

  it("keeps the latest evidence visible when returning from replay history", async () => {
    const runId = "01JZ455ESCALATE";
    const fixtures = populatedDaemonFixtures();
    const eventList = fixtures.runEvents?.[runId];
    const detail = fixtures.runDetails?.[runId];
    if (!eventList || !detail) {
      throw new Error("Expected completed run fixtures.");
    }
    eventList.events = [
      {
        schema: "v1",
        seq: 1,
        type: "run.started",
        branch: 0,
        time: "2026-07-18T02:00:01Z",
        knownSchema: true,
        category: "transition",
      },
      {
        schema: "v1",
        seq: 2,
        type: "gate.started",
        branch: 0,
        time: "2026-07-18T02:00:02Z",
        knownSchema: true,
        category: "bookkeeping",
        gate: "review",
        attempt: 1,
      },
      {
        schema: "v1",
        seq: 3,
        type: "span.recorded",
        branch: 0,
        time: "2026-07-18T02:00:03Z",
        knownSchema: true,
        category: "evidence",
        stage: `${runId}:review`,
        name: "reviewer.transcript",
      },
    ];
    detail.lastSeq = 3;
    renderRun(runId, new FixtureDaemonClient(fixtures));

    expect(await screen.findByText("review evidence · Visit 1 · Sequence 3")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /^Select sequence 1:/ }));
    fireEvent.click(
      screen.getByRole("button", {
        name: /Expand 2 supporting events for Review · Visit 1, sequences 2 through 3/,
      }),
    );
    const transcript = screen.getByRole("button", { name: /^Select sequence 3:/ });
    fireEvent.click(transcript);
    expect(screen.getByText("review evidence · Visit 1 · Sequence 3")).toBeInTheDocument();
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
    const replayControls = within(fullscreenRoot).getByRole("region", {
      name: "Replay controls",
    });
    const scrubber = within(replayControls).getByRole("slider", {
      name: "Scrub replay timeline",
    });
    const lastReplayControl = within(replayControls).getByRole("button", {
      name: "Set playback speed to 10×",
    });
    const inspector = within(fullscreenRoot).getByRole("complementary", {
      name: /attempt inspector/,
    });
    expect(replayControls).toBeInTheDocument();
    expect(scrubber).toBeInTheDocument();
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
    expect(lastReplayControl).toHaveFocus();

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
        within(fullscreenRoot).getByRole("region", { name: "Replay controls" }),
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
      await vi.advanceTimersByTimeAsync(200);
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
      await vi.advanceTimersByTimeAsync(200);
    });

    expect(screen.getByRole("button", { name: /^Select sequence 4:/ })).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(screen.getByRole("button", { name: /^Select sequence 8:/ })).not.toHaveAttribute(
      "aria-current",
    );
    client.close();
  });

  it("pins the view when a non-latest graph stage is selected, and resumes latest for the latest stage (#2307)", async () => {
    const runId = "01JZ441DAEMONAPI";
    const fixtures = populatedDaemonFixtures();
    const events = fixtures.runEvents?.[runId];
    const detail = fixtures.runDetails?.[runId];
    if (!events || !detail) {
      throw new Error("Expected active run fixtures.");
    }
    const client = new LiveFixtureClient(fixtures);
    renderRun(runId, client);
    await screen.findByRole("heading", { name: `Run ${runId}` });

    expect(
      screen.getByRole("button", { name: "review, gate, Running at sequence 6" }),
    ).toHaveAttribute("aria-pressed", "true");

    fireEvent.click(
      screen.getByRole("button", { name: "query, deterministic, Completed at sequence 6" }),
    );
    expect(
      screen.getByRole("button", { name: "query, deterministic, Completed at sequence 6" }),
    ).toHaveAttribute("aria-pressed", "true");

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
    act(() => client.invalidateRun("fixture:1"));

    // The graph selection must survive the background refresh instead of
    // snapping back to the new latest stage (unlike the timeline-event and
    // replay-seek paths, this previously left followingLatest untouched).
    await screen.findByRole("button", { name: /^Select sequence 7:/ });
    expect(
      screen.getByRole("button", { name: "query, deterministic, Completed at sequence 6" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: /^Select sequence 7:/ })).not.toHaveAttribute(
      "aria-current",
    );

    fireEvent.click(screen.getByRole("button", { name: /^review, gate,/ }));

    // Selecting the latest stage resumes follow-latest immediately.
    expect(screen.getByRole("button", { name: /^Select sequence 7:/ })).toHaveAttribute(
      "aria-current",
      "true",
    );

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
    act(() => client.invalidateRun("fixture:2"));

    // Follow-latest continues to track subsequent live events.
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /^Select sequence 8:/ })).toHaveAttribute(
        "aria-current",
        "true",
      ),
    );
    client.close();
  });

  it("pins the view when an earlier attempt is selected (#2464)", async () => {
    const runId = "01JZ441DAEMONAPI";
    const fixtures = populatedDaemonFixtures();
    const events = fixtures.runEvents?.[runId];
    const detail = fixtures.runDetails?.[runId];
    if (!events || !detail) {
      throw new Error("Expected active run fixtures.");
    }
    fixtures.stageAttempts = {
      ...fixtures.stageAttempts,
      [fixtureKey(runId, "review")]: {
        runId,
        stage: "review",
        attempts: [
          {
            id: "sta-review-attempt-1",
            visit: 1,
            number: 1,
            class: "initial",
            status: "failure",
            startedSeq: 5,
            finishedSeq: 5,
            durationMillis: 1_000,
            artifacts: [],
          },
          {
            id: "sta-review-attempt-2",
            visit: 1,
            number: 2,
            class: "policy",
            status: "running",
            startedSeq: 6,
            durationMillis: 1_000,
            artifacts: [],
          },
        ],
      },
    };
    const client = new LiveFixtureClient(fixtures);
    renderRun(runId, client);

    fireEvent.click(
      await screen.findByRole("button", { name: "Visit 1 · Attempt 1" }),
    );
    expect(
      screen.getByRole("button", { name: "Visit 1 · Attempt 1" }),
    ).toHaveAttribute("aria-pressed", "true");

    events.events.push({
      schema: "v1",
      seq: 7,
      type: "gate.evaluated",
      branch: 0,
      time: "2026-07-18T06:00:07Z",
      knownSchema: true,
      gate: "review",
      attempt: 2,
      attemptClass: "policy",
      verdict: "needs-changes",
      target: "implement",
    });
    detail.lastSeq = 7;
    detail.currentStage = "implement";
    act(() => client.invalidateRun("fixture:attempt-selection"));

    await screen.findByRole("button", { name: /^Select sequence 7:/ });
    expect(
      screen.getByRole("button", { name: "review, gate, Running at sequence 6" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(
      screen.getByRole("button", { name: "Visit 1 · Attempt 1" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: /^Select sequence 7:/ })).not.toHaveAttribute(
      "aria-current",
    );
    client.close();
  });

  it("keeps run detail visible, without a busy banner, while a refresh is pending, and surfaces failures (#2530)", async () => {
    // Regression test for #2530: a background refresh triggered by a live
    // invalidation dips useLiveData's connection freshness through "stale"
    // for the round-trip (liveData.tsx's drainInvalidations) on every single
    // live event for an active run, not just on genuine disconnects. Before
    // this fix, RunPage rendered a "Refreshing run detail…" banner for any
    // stale-without-error state, so it popped in and out above the
    // graph/journal once per event — a recurrence of the
    // #2307/#2304/#2308 "background refresh must not visibly disrupt the
    // view" class, this time as a pure visual flicker with selection intact.
    // RunPage must behave like every other query-driven page in the portal
    // (WorkflowPage, ErrorsPage, InsightPage, GagglePage): stale-without-error
    // is invisible, and only stale-with-error surfaces anything.
    const runId = "01JZ441DAEMONAPI";
    const client = new LiveFixtureClient(populatedDaemonFixtures());
    renderRun(runId, client);
    await screen.findByRole("heading", { name: `Run ${runId}` });

    client.holdRefresh();
    await act(async () => {
      client.invalidateRun("fixture:stale");
      await client.waitForPendingRefresh();
    });

    // The refresh is genuinely pending (not yet resolved), and the run
    // detail must stay fully visible without any busy banner appearing.
    expect(screen.queryByText("Refreshing run detail…")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: `Run ${runId}` })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Execution graph" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Loading run" })).not.toBeInTheDocument();

    act(() => client.failRefresh(new Error("Unable to refresh this run.")));

    expect(await screen.findByText("Run detail may be stale")).toBeInTheDocument();
    expect(screen.getByText("Unable to refresh this run.")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: `Run ${runId}` })).toBeInTheDocument();
    expect(screen.queryByText("Refreshing run detail…")).not.toBeInTheDocument();

    client.holdRefresh();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Try again" }));
      await client.waitForPendingRefresh();
    });

    expect(screen.queryByText("Refreshing run detail…")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: `Run ${runId}` })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Loading run" })).not.toBeInTheDocument();

    act(() => client.failRefresh(new Error("Still unable to refresh this run.")));

    expect(await screen.findByText("Run detail may be stale")).toBeInTheDocument();
    expect(screen.getByText("Still unable to refresh this run.")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: `Run ${runId}` })).toBeInTheDocument();
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
    expect(portalStyles).toMatch(/\.playback-panel\s*\{[^}]*width:\s*100%/s);
    expect(screen.queryByRole("heading", { name: /attempt|escalation/i })).not.toBeInTheDocument();
  });

  it("pivots the run's gaggle/workflow identity into a pre-scoped Runs and Insight view (#2529)", async () => {
    const user = userEvent.setup();
    renderRun("01JZ400FAILED");

    await screen.findByRole("heading", { name: "Run 01JZ400FAILED" });
    expect(
      screen.getByRole("link", { name: "View core / implementation in Insight" }),
    ).toHaveAttribute("href", "#/insight?gaggle=core&workflow=implementation");

    await user.click(
      screen.getByRole("link", { name: "View core / implementation in Runs" }),
    );

    expect(await screen.findByRole("heading", { name: "Runs" })).toBeInTheDocument();
    expect(screen.getByLabelText("Insight drill-through scope")).toHaveTextContent(
      "core / implementation",
    );
  });

  it("surfaces the coded failure reason and deep-links from a failed run", async () => {
    const user = userEvent.setup();
    renderRun("01JZ400FAILED");

    const banner = await screen.findByRole("region", {
      name: /harness\.crash · Harness exited before producing a result envelope\./,
    });

    expect(within(banner).getAllByText("harness.crash", { selector: ".mono" })).toHaveLength(2);
    expect(within(banner).getByRole("link", { name: /view matching errors/i })).toHaveAttribute(
      "href",
      "#/errors?gaggle=core&workflow=implementation&stage=implement&code=harness.crash",
    );

    await user.click(within(banner).getByRole("button", { name: /Failing event/ }));

    expect(
      screen.getByRole("button", { name: "implement, agentic, Failed at sequence 5" }),
    ).toBeInTheDocument();
  });
});

// The journal is scanned by stage: the reader is looking for "the second
// implement attempt", and until stage was a column that meant reading every
// row's prose. Filtering narrows a long ledger to one stage's story.
describe("journal stage column", () => {
  it("renders the stage as its own column and announces it", async () => {
    renderRun("01JZ441DAEMONAPI");

    await screen.findByRole("heading", { name: "Event ledger" });
    fireEvent.click(screen.getByRole("button", { name: /^All events/ }));

    const rows = screen.getAllByRole("button", { name: /^Select sequence/ });
    const labels = rows.map((row) => row.getAttribute("aria-label") ?? "");
    // Every row names its scope right after the sequence, so the column is
    // populated for gate and run-level events too, not only stages.
    for (const label of labels) {
      expect(label).toMatch(/^Select sequence \d+: .+?\. /);
    }
    expect(labels.some((label) => /^Select sequence \d+: implement\. /.test(label))).toBe(true);
  });

  it("filters the ledger to one stage and back", async () => {
    const user = userEvent.setup();
    renderRun("01JZ441DAEMONAPI");

    await screen.findByRole("heading", { name: "Event ledger" });
    fireEvent.click(screen.getByRole("button", { name: /^All events/ }));
    const all = screen.getAllByRole("button", { name: /^Select sequence/ }).length;

    const filter = screen.getByLabelText("Stage");
    await user.selectOptions(filter, "implement");

    const filtered = screen.getAllByRole("button", { name: /^Select sequence/ });
    expect(filtered.length).toBeGreaterThan(0);
    expect(filtered.length).toBeLessThan(all);
    for (const row of filtered) {
      expect(row.getAttribute("aria-label")).toMatch(/^Select sequence \d+: implement\. /);
    }

    await user.selectOptions(filter, "");
    expect(screen.getAllByRole("button", { name: /^Select sequence/ })).toHaveLength(all);
  });
});

// A long, high-fanout run has no way to jump to "the timeout row" other than
// reading every entry; free-text search closes that gap and must compose
// with the stage filter rather than replace it.
describe("journal search", () => {
  it("filters the ledger by matching text and reports an explicit empty state", async () => {
    const user = userEvent.setup();
    renderRun("01JZ441DAEMONAPI");

    await screen.findByRole("heading", { name: "Event ledger" });
    fireEvent.click(screen.getByRole("button", { name: /^All events/ }));
    const all = screen.getAllByRole("button", { name: /^Select sequence/ }).length;

    const search = screen.getByPlaceholderText("Search events");
    await user.type(search, "evaluation");

    const filtered = screen.getAllByRole("button", { name: /^Select sequence/ });
    expect(filtered.length).toBeGreaterThan(0);
    expect(filtered.length).toBeLessThan(all);
    for (const row of filtered) {
      expect(row.getAttribute("aria-label")).toMatch(/evaluation/i);
    }

    await user.clear(search);
    await user.type(search, "no-such-event-text");
    expect(screen.getAllByRole("heading", { name: "Event ledger" })).toHaveLength(1);
    expect(screen.getByText("No events match")).toBeInTheDocument();
    expect(screen.queryAllByRole("button", { name: /^Select sequence/ })).toHaveLength(0);

    await user.clear(search);
    expect(screen.getAllByRole("button", { name: /^Select sequence/ })).toHaveLength(all);
  });

  it("composes the search with the stage filter", async () => {
    const user = userEvent.setup();
    renderRun("01JZ441DAEMONAPI");

    await screen.findByRole("heading", { name: "Event ledger" });
    fireEvent.click(screen.getByRole("button", { name: /^All events/ }));

    const stageSelect = screen.getByLabelText("Stage");
    await user.selectOptions(stageSelect, "implement");
    const stageOnly = screen.getAllByRole("button", { name: /^Select sequence/ }).length;

    const search = screen.getByPlaceholderText("Search events");
    await user.type(search, "evaluation");

    // "evaluation" only appears on the review gate's summary, which isn't
    // the implement stage, so composing the two filters empties the ledger
    // rather than falling back to either filter alone.
    expect(screen.getByText("No events match")).toBeInTheDocument();

    await user.selectOptions(stageSelect, "");
    const searchOnly = screen.getAllByRole("button", { name: /^Select sequence/ }).length;
    expect(searchOnly).toBeGreaterThan(0);
    expect(searchOnly).toBeLessThan(stageOnly);
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
  private refreshError: Error | undefined;
  private refreshGate: Deferred | undefined;
  private refreshStarted: Deferred | undefined;

  override connectEvents(): Promise<DaemonEventStream> {
    return Promise.resolve(this.stream);
  }

  // Closes the push stream so a `for await` consumer left mid-iteration by
  // this test unblocks immediately instead of staying suspended on the next
  // read across the test boundary, where it otherwise outlives the
  // unmounted component.
  close(): void {
    this.stream.close();
  }

  override async getRun(runId: string, options?: RequestOptions): Promise<RunDetail> {
    await this.waitForRefresh();
    return super.getRun(runId, options);
  }

  override async listRunEvents(
    runId: string,
    options?: RequestOptions,
  ): Promise<EventList> {
    await this.waitForRefresh();
    return super.listRunEvents(runId, options);
  }

  holdRefresh(): void {
    this.refreshError = undefined;
    this.refreshGate = deferred();
    this.refreshStarted = deferred();
  }

  // Since the removal of the #2530 stale-without-error banner leaves no DOM
  // signal that a held refresh has actually reached the gate (the previous
  // version of this test relied on `findByText("Refreshing run detail…")`
  // for that synchronization), tests must await this instead of racing
  // `failRefresh`/`release` against a refresh that hasn't started yet.
  async waitForPendingRefresh(): Promise<void> {
    await this.refreshStarted?.promise;
  }

  failRefresh(error: Error): void {
    this.refreshError = error;
    this.refreshGate?.resolve();
    this.refreshGate = undefined;
  }

  invalidateRun(id: string): void {
    this.stream.push({
      id,
      type: "invalidate",
      data: { cursor: id, models: ["run"] },
    });
  }

  private async waitForRefresh(): Promise<void> {
    const gate = this.refreshGate;
    if (!gate) {
      return;
    }
    this.refreshStarted?.resolve();
    await gate.promise;
    if (this.refreshError) {
      throw this.refreshError;
    }
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

interface Deferred {
  promise: Promise<void>;
  resolve: () => void;
}

function deferred(): Deferred {
  let resolve: () => void = () => undefined;
  const promise = new Promise<void>((promiseResolve) => {
    resolve = promiseResolve;
  });
  return { promise, resolve };
}
