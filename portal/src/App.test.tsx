import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { FixtureDaemonClient } from "./api/fixtureClient";
import { runs } from "./prototypeData";
import { populatedDaemonFixtures } from "./test/daemonFixtures";

const storedValues = new Map<string, string>();

beforeEach(() => {
  storedValues.clear();
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      clear: () => storedValues.clear(),
      getItem: (key: string) => storedValues.get(key) ?? null,
      key: (index: number) => [...storedValues.keys()][index] ?? null,
      get length() {
        return storedValues.size;
      },
      removeItem: (key: string) => storedValues.delete(key),
      setItem: (key: string, value: string) => storedValues.set(key, value),
    } satisfies Storage,
  });
  delete document.documentElement.dataset.theme;
});

describe("portal foundation", () => {
  beforeEach(() => {
    window.location.hash = "#/overview";
  });

  it("shows the operational overview", async () => {
    renderLiveApp();

    expect(
      await screen.findByRole("heading", { name: "2 runs need attention." }),
    ).toBeInTheDocument();
    expect(screen.getByText("Daemon ready")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Needs attention" })).toBeInTheDocument();
  });

  it("opens a run and supports replay", async () => {
    const user = userEvent.setup();
    renderLiveApp();

    await user.click(
      await screen.findByRole("link", { name: "Open run 01JZ402DASHBOARD" }),
    );
    expect(await screen.findByRole("heading", { name: "Live visual dashboard and workflow DAG" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Execution graph" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Event ledger" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Play replay" }));
    expect(screen.getByRole("button", { name: "Pause replay" })).toBeInTheDocument();
  });

  it("uses the run's pinned workflow and does not reveal future attempt results", async () => {
    const user = userEvent.setup();
    const { container } = renderLiveApp();

    await user.click(
      await screen.findByRole("link", { name: "Open run 01JZ402DASHBOARD" }),
    );
    expect(await screen.findByText(/Implementation v7/)).toBeInTheDocument();
    expect(screen.getByText(/v7 · 589d28aa/)).toBeInTheDocument();

    fireEvent.change(screen.getByRole("slider", { name: "Replay position" }), {
      target: { value: "3" },
    });

    expect(await screen.findByText("In progress")).toBeInTheDocument();
    expect(screen.getByText("Attempt is still in progress at this point.")).toBeInTheDocument();
    expect(
      screen.queryByText("Added the initial daemon read endpoints with fixture-backed coverage."),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("attempt-1-summary.md")).not.toBeInTheDocument();
    expect(container.querySelector('[data-edge="review-gate->implement"]')).not.toHaveClass("graph-edge-active");
  });

  it.each([
    { hash: "#/overview", heading: "2 runs need attention." },
    { hash: "#/workflows", heading: "Workflows" },
    { hash: "#/runs", heading: "Runs" },
  ])("renders the $hash shell route from daemon fixtures", async ({ hash, heading }) => {
    window.location.hash = hash;
    renderLiveApp();

    expect(await screen.findByRole("heading", { name: heading })).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Primary" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Skip to main content" })).toHaveAttribute(
      "href",
      "#main-content",
    );
  });

  it("persists independently selected themes", async () => {
    window.localStorage.setItem("goobers-theme", "dark");
    const user = userEvent.setup();
    renderLiveApp();

    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    await user.click(screen.getByRole("button", { name: "Use light theme" }));

    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    expect(window.localStorage.getItem("goobers-theme")).toBe("light");
  });

  it("operates primary navigation from the keyboard and moves focus to the route content", async () => {
    const user = userEvent.setup();
    renderLiveApp();
    const workflowsButton = screen.getByRole("button", { name: "Workflows" });

    workflowsButton.focus();
    await user.keyboard("{Enter}");

    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("main")).toHaveFocus());
    expect(screen.getByRole("button", { name: "Workflows" })).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  it("skips to main content without changing the active hash route", async () => {
    window.location.hash = "#/workflows";
    const user = userEvent.setup();
    renderLiveApp();

    await user.click(screen.getByRole("link", { name: "Skip to main content" }));

    expect(window.location.hash).toBe("#/workflows");
    expect(screen.getByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    expect(screen.getByRole("main")).toHaveFocus();
  });

  it("supports directional graph selection and exposes the compact responsive contract", () => {
    window.location.hash = "#/workflow/implementation";
    render(<App />);
    const firstStage = screen.getByRole("button", { name: "Gather context, pending" });
    const secondStage = screen.getByRole("button", { name: "Implement, pending" });

    firstStage.focus();
    fireEvent.keyDown(firstStage, { key: "ArrowRight" });

    expect(secondStage).toHaveFocus();
    expect(secondStage).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("group", { name: /Implementation execution graph/ })).toHaveAttribute(
      "data-responsive-layout",
      "compact-under-820",
    );
  });

  it("gives run filters observable behavior", async () => {
    window.location.hash = "#/runs";
    const user = userEvent.setup();
    render(<App />);

    await user.click(screen.getByRole("button", { name: "attention" }));

    expect(screen.getByRole("button", { name: /Open run Live visual dashboard/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Open run Daemon read API/i })).not.toBeInTheDocument();
  });

  function renderLiveApp() {
    return render(<App client={new FixtureDaemonClient(populatedDaemonFixtures())} />);
  }
});

describe("escalation detail", () => {
  beforeEach(() => {
    setReducedMotion(false);
  });

  it("renders the structured gate cause and focuses its durable event", async () => {
    const user = userEvent.setup();
    const { container } = renderRun("01JZ402DASHBOARD");

    expect(
      screen.getByRole("heading", { name: "Scope could not converge within the repass budget" }),
    ).toBeInTheDocument();
    expect(screen.getByText("review-gate")).toBeInTheDocument();
    expect(screen.getByText("@escalate")).toBeInTheDocument();
    expect(screen.getByText("3 / 3 repass attempts")).toBeInTheDocument();
    expect(
      screen.getByText(
        "The final needs-changes verdict remained over-scoped after every allowed repass.",
      ),
    ).toBeInTheDocument();
    expectEvent(15, 16);

    const causalEvent = screen.getByRole("button", {
      name: "Select event 15: Repass budget exhausted (causal event)",
    });
    expect(causalEvent).toHaveAttribute("aria-current", "true");
    expect(
      screen.getByRole("button", { name: "Review gate, escalated, causal event" }),
    ).toHaveClass("graph-node-causal");
    expect(container.querySelector(".ledger-item-causal")).toContainElement(causalEvent);

    const escalationRegion = screen.getByRole("region", {
      name: "Scope could not converge within the repass budget",
    });
    escalationRegion.focus();
    expect(escalationRegion).toHaveFocus();

    expect(screen.getByText("Evidence at escalation")).toBeInTheDocument();
    await user.click(
      screen.getByRole("button", {
        name: "Inspect Implement evidence: 3 attempts, 3 artifacts",
      }),
    );
    expect(screen.getAllByRole("button", { name: /^Attempt [123]$/ })).toHaveLength(3);
    expect(screen.getAllByText("attempt-1-summary.md").length).toBeGreaterThan(0);
    expect(screen.getAllByText("attempt-2-summary.md").length).toBeGreaterThan(0);
    expect(screen.getAllByText("attempt-3-summary.md").length).toBeGreaterThan(0);

    expect(
      screen.queryByRole("button", { name: /approve|override|rerun/i }),
    ).not.toBeInTheDocument();
  });

  it("renders retry exhaustion from structured condition data", () => {
    renderRun("01JZ447RETRYBUDGET");

    expect(
      screen.getByRole("heading", { name: "Merge could not complete within the retry budget" }),
    ).toBeInTheDocument();
    expect(screen.getByText("merge retry policy")).toBeInTheDocument();
    expect(screen.getByText("2 / 2 retry attempts")).toBeInTheDocument();
    expectEvent(6, 7);
    expect(
      screen.getByRole("button", { name: "Select event 6: Retry budget exhausted (causal event)" }),
    ).toHaveAttribute("aria-current", "true");
    expect(screen.getAllByText("merge-attempt-1.json").length).toBeGreaterThan(0);
    expect(screen.getAllByText("merge-attempt-2.json").length).toBeGreaterThan(0);
  });

  it("keeps evidence anchored to the causal event as replay advances", () => {
    const run = runs.find(({ id }) => id === "01JZ402DASHBOARD");
    if (!run) {
      throw new Error("Escalated run fixture is missing");
    }
    const causalAttempt = run.attempts.find(({ id }) => id === "review-gate-3-escalated");
    if (!causalAttempt) {
      throw new Error("Causal attempt fixture is missing");
    }
    const postCausalArtifact = {
      name: "post-escalation-delivery.json",
      mediaType: "application/json",
      size: "1.1 KB",
      summary: "Delivery metadata recorded after the causal event.",
      digest: "sha256:2b5c8e1a9f4d7c3b6e0a2d5f8c1b4e7a0d3f6c9b2e5a8d1f4c7b0e3a6d9f2c5b",
      digestVerified: false,
      recordedSeq: 16,
    };
    causalAttempt.artifacts.push(postCausalArtifact);

    try {
      renderRun(run.id);
      const escalationPanel = screen.getByRole("region", {
        name: "Scope could not converge within the repass budget",
      });
      expect(
        within(escalationPanel).queryByText("post-escalation-delivery.json"),
      ).not.toBeInTheDocument();

      fireEvent.change(screen.getByRole("slider", { name: "Replay position" }), {
        target: { value: String(run.events.length - 1) },
      });

      expectEvent(16, 16);
      expect(screen.getByText("post-escalation-delivery.json")).toBeInTheDocument();
      expect(
        within(escalationPanel).queryByText("post-escalation-delivery.json"),
      ).not.toBeInTheDocument();
    } finally {
      causalAttempt.artifacts.splice(causalAttempt.artifacts.indexOf(postCausalArtifact), 1);
    }
  });

  it("renders an unresolved causal event without linking or highlighting the final event", () => {
    const run = runs.find(({ id }) => id === "01JZ402DASHBOARD");
    if (!run?.escalation) {
      throw new Error("Structured escalation fixture is missing");
    }
    const originalEscalation = run.escalation;
    run.escalation = { ...originalEscalation, causalEventSeq: 99 };

    try {
      const { container } = renderRun(run.id);

      expectEvent(16, 16);
      expect(screen.getByText("Seq 99 · Unavailable")).toBeInTheDocument();
      expect(
        screen.getByText(/point-in-time evidence is unavailable because the causal event could not be resolved/i),
      ).toBeInTheDocument();
      expect(
        screen.queryByRole("button", { name: /^Causal event/i }),
      ).not.toBeInTheDocument();
      expect(container.querySelector(".graph-node-causal")).not.toBeInTheDocument();
      expect(container.querySelector(".ledger-item-causal")).not.toBeInTheDocument();
      expect(
        screen.getByRole("button", { name: "Select event 16: Run escalated" }),
      ).toHaveAttribute("aria-current", "true");
    } finally {
      run.escalation = originalEscalation;
    }
  });

  it("shows an explicit unavailable state for a legacy escalation", () => {
    renderRun("01JZ450LEGACYCAUSE");

    expect(
      screen.getByRole("heading", { name: "Escalation cause unavailable" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/legacy run has no structured cause record/i),
    ).toBeInTheDocument();
    expect(screen.queryByText("Selected branch")).not.toBeInTheDocument();
    expect(screen.queryByText("Budget consumed")).not.toBeInTheDocument();
    expect(screen.getByText("Evidence at escalation")).toBeInTheDocument();
  });

  it.each([
    ["01JZ441DAEMONAPI", "Review, active"],
    ["01JZ455ESCALATE", "Merge, complete"],
  ])("does not add escalation chrome to non-escalated run %s", (runId, graphNodeName) => {
    renderRun(runId);

    expect(screen.queryByText("Attention · Escalation")).not.toBeInTheDocument();
    expect(screen.queryByText("Evidence at escalation")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: graphNodeName })).toBeInTheDocument();
  });

  it.each(["failed", "aborted"] as const)(
    "does not treat a non-escalated %s run as an escalation",
    (status) => {
      const run = runs.find(({ id }) => id === "01JZ455ESCALATE");
      if (!run) {
        throw new Error("Terminal run fixture is missing");
      }
      const originalStatus = run.status;
      run.status = status;

      try {
        renderRun(run.id);

        expect(screen.getByText(new RegExp(`^${status}$`, "i"))).toBeInTheDocument();
        expect(screen.queryByText("Attention · Escalation")).not.toBeInTheDocument();
        expect(screen.queryByText("Evidence at escalation")).not.toBeInTheDocument();
        expect(screen.queryByText("Causal event")).not.toBeInTheDocument();
      } finally {
        run.status = originalStatus;
      }
    },
  );
});

function setReducedMotion(matches: boolean) {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches,
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: () => true,
    })),
  });
}

function renderRun(runId = "01JZ455ESCALATE") {
  window.location.hash = `#/run/${runId}`;
  return render(<App />);
}

function expectEvent(position: number, count = 10) {
  expect(screen.getByText(new RegExp(`^Event ${position} of ${count} · Seq`))).toBeInTheDocument();
}

describe("deterministic run replay", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    setReducedMotion(false);
  });

  afterEach(() => {
    vi.clearAllTimers();
    vi.useRealTimers();
  });

  it.each([1, 5, 10] as const)("visits every durable event once at %sx", (speed) => {
    renderRun();

    fireEvent.click(screen.getByRole("button", { name: `${speed}x` }));
    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    expectEvent(1);

    for (let position = 2; position <= 10; position += 1) {
      act(() => vi.runOnlyPendingTimers());
      expectEvent(position);
    }

    expect(screen.getByRole("button", { name: "Play replay" })).toBeInTheDocument();
    expect(screen.getByText("Replay ended at event 10. Play restarts from event 1.")).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);
  });

  it.each([
    { speed: 1, delay: 1_500 },
    { speed: 5, delay: 300 },
    { speed: 10, delay: 150 },
  ] as const)("scales compressed waits at $speedx", ({ speed, delay }) => {
    renderRun();

    fireEvent.click(screen.getByRole("button", { name: `${speed}x` }));
    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    for (let position = 2; position <= 4; position += 1) {
      act(() => vi.runOnlyPendingTimers());
      expectEvent(position);
    }

    act(() => vi.advanceTimersByTime(delay - 1));
    expectEvent(4);
    act(() => vi.advanceTimersByTime(1));
    expectEvent(5);
  });

  it("pauses, resumes, stops at the end, and restarts from the first event", () => {
    renderRun();

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    act(() => vi.runOnlyPendingTimers());
    expectEvent(2);

    fireEvent.click(screen.getByRole("button", { name: "Pause replay" }));
    act(() => vi.advanceTimersByTime(60_000));
    expectEvent(2);

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    act(() => vi.runOnlyPendingTimers());
    expectEvent(3);

    for (let position = 4; position <= 10; position += 1) {
      act(() => vi.runOnlyPendingTimers());
      expectEvent(position);
    }
    expect(screen.getByRole("button", { name: "Play replay" })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    expectEvent(1);
    expect(screen.getByRole("button", { name: "Pause replay" })).toBeInTheDocument();
  });

  it("cancels playback for scrub, previous, and next selection", () => {
    renderRun();

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    act(() => vi.runOnlyPendingTimers());
    expectEvent(2);

    fireEvent.change(screen.getByRole("slider", { name: "Replay position" }), {
      target: { value: "5" },
    });
    expectEvent(6);
    expect(screen.getByRole("button", { name: "Play replay" })).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    fireEvent.click(screen.getByRole("button", { name: "Previous event" }));
    expectEvent(5);
    expect(screen.getByRole("button", { name: "Play replay" })).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    fireEvent.click(screen.getByRole("button", { name: "Next event" }));
    expectEvent(6);
    expect(screen.getByRole("button", { name: "Play replay" })).toBeInTheDocument();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("compresses idle waits while retaining real elapsed time", () => {
    renderRun();

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    for (let position = 2; position <= 4; position += 1) {
      act(() => vi.runOnlyPendingTimers());
      expectEvent(position);
    }

    expect(screen.getByText(/Real elapsed 0:04$/)).toBeInTheDocument();
    expect(screen.getByText("Next wait: 7m 36s compressed to 1.5s at 1x.")).toBeInTheDocument();

    act(() => vi.advanceTimersByTime(1_499));
    expectEvent(4);
    act(() => vi.advanceTimersByTime(1));
    expectEvent(5);
    expect(screen.getByText(/Real elapsed 7:40$/)).toBeInTheDocument();
  });

  it("keeps active-run history inspection in live-follow until replay is entered", () => {
    renderRun("01JZ441DAEMONAPI");

    expectEvent(7, 7);
    expect(screen.getByText("Live follow")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Select event 2: Gathering context" }));
    expectEvent(2, 7);
    expect(screen.getByText("Live follow · inspecting history")).toBeInTheDocument();
    expect(
      screen.getByText("Inspecting event 2; new durable events remain live-followed."),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Enter replay" }));
    expectEvent(1, 7);
    expect(screen.getByText("Replay mode")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Return to live" }));
    expectEvent(7, 7);
    expect(screen.getByText("Live follow")).toBeInTheDocument();
  });

  it("updates replay state without graph traversal animation in reduced motion", () => {
    setReducedMotion(true);
    const { container } = renderRun();

    expect(
      screen.getByText("Reduced motion: state changes are instant without graph traversal animation."),
    ).toBeInTheDocument();
    expect(container.querySelector(".run-workspace")).toHaveAttribute("data-replay-motion", "reduced");

    fireEvent.click(screen.getByRole("button", { name: "Play replay" }));
    for (let position = 2; position <= 4; position += 1) {
      act(() => vi.runOnlyPendingTimers());
      expectEvent(position);
    }
    expect(container.querySelector(".graph-edge-traversing")).not.toBeInTheDocument();
  });

  it("supports keyboard previous, next, and play controls", () => {
    renderRun();
    const controls = screen.getByRole("region", { name: "Replay controls" });

    controls.focus();
    fireEvent.keyDown(controls, { key: "ArrowLeft" });
    expectEvent(9);
    fireEvent.keyDown(controls, { key: "ArrowRight" });
    expectEvent(10);
    fireEvent.keyDown(controls, { key: " " });
    expectEvent(1);
    expect(screen.getByRole("button", { name: "Pause replay" })).toBeInTheDocument();
  });
});
