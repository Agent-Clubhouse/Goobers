import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";

const reducedMotionQuery = "(prefers-reduced-motion: reduce)";
const compactLayoutQuery = "(max-width: 820px)";

describe("portal foundations", () => {
  beforeEach(() => {
    window.location.hash = "#/overview";
    window.localStorage.clear();
    delete document.documentElement.dataset.theme;
    setMediaMatches({});
  });

  it("shows the operational overview", () => {
    render(<App />);

    expect(screen.getByRole("heading", { name: "One run needs attention." })).toBeInTheDocument();
    expect(screen.getByText("Daemon connected")).toBeInTheDocument();
    expect(screen.getByText("Scope could not converge within the repass budget")).toBeInTheDocument();
  });

  it("opens a run and supports replay", async () => {
    const user = userEvent.setup();
    render(<App />);

    await user.click(screen.getByRole("button", { name: /Open run Live visual dashboard/i }));
    expect(await screen.findByRole("heading", { name: "Live visual dashboard and workflow DAG" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Execution graph" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Event ledger" })).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Play replay" }));
    expect(screen.getByRole("button", { name: "Pause replay" })).toBeInTheDocument();
  });

  it.each([
    ["#/overview", "One run needs attention."],
    ["#/workflows", "Workflows"],
    ["#/runs", "Runs"],
  ])("renders the %s fixture route", (hash, heading) => {
    window.location.hash = hash;
    render(<App />);

    expect(screen.getByRole("heading", { name: heading })).toBeInTheDocument();
  });

  it("persists independently tuned themes locally", async () => {
    window.localStorage.setItem("goobers-theme", "dark");
    const user = userEvent.setup();
    render(<App />);

    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    await user.click(screen.getByRole("button", { name: "Use light theme" }));
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    expect(window.localStorage.getItem("goobers-theme")).toBe("light");
  });

  it("operates primary navigation and graph selection from the keyboard", async () => {
    const user = userEvent.setup();
    const { unmount } = render(<App />);
    const workflowsNavigation = screen.getByRole("button", { name: "Workflows" });

    workflowsNavigation.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    expect(workflowsNavigation).toHaveAttribute("aria-current", "page");

    unmount();
    window.location.hash = "#/workflow/implementation";
    render(<App />);
    const firstStage = screen.getByRole("button", { name: "Gather context, pending" });
    const secondStage = screen.getByRole("button", { name: "Implement, pending" });

    firstStage.focus();
    fireEvent.keyDown(firstStage, { key: "ArrowRight" });
    expect(secondStage).toHaveFocus();
    await user.keyboard("{Enter}");
    expect(secondStage).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("complementary", { name: "Implement inspector" })).toBeInTheDocument();
  });

  it("skips to main content without changing the current hash route", async () => {
    window.location.hash = "#/runs";
    const user = userEvent.setup();
    render(<App />);

    await user.click(screen.getByRole("link", { name: "Skip to content" }));
    expect(screen.getByRole("main")).toHaveFocus();
    expect(window.location.hash).toBe("#/runs");
    expect(screen.getByRole("heading", { name: "Runs" })).toBeInTheDocument();
  });

  it("keeps semantic run columns aligned in compact layouts", () => {
    setMediaMatches({ [compactLayoutQuery]: true });
    const { container, unmount } = render(<App />);

    expect(container.querySelector(".portal-frame")).toHaveAttribute("data-layout", "compact");
    expect(screen.getByRole("button", { name: "Workflows" })).toBeInTheDocument();
    const activeRun = screen.getByRole("button", { name: /Open run Daemon read-only HTTP API/i });
    expect(activeRun).toHaveClass("run-grid");
    expect(activeRun.querySelector(".row-primary")).toHaveTextContent("Daemon read-only HTTP API");
    expect(activeRun.querySelector(".run-workflow")).toHaveTextContent("Implementation");
    expect(container.querySelector(".data-header .run-workflow")).toHaveTextContent("Workflow");

    unmount();
    window.location.hash = "#/runs";
    const runsView = render(<App />);
    const historyRun = screen.getByRole("button", { name: /Open run Daemon read-only HTTP API/i });
    expect(historyRun.querySelector(".row-primary")).toHaveTextContent("Daemon read-only HTTP API");
    expect(historyRun.querySelector(".run-started")).toHaveTextContent("Today at 9:12 PM");
    expect(runsView.container.querySelector(".data-header .run-started")).toHaveTextContent("Started");
  });

  it("pairs semantic status text with a non-color icon cue", () => {
    render(<App />);

    const completed = screen.getAllByText("Completed")[0].closest(".status-badge");
    expect(completed).toHaveTextContent("Completed");
    expect(completed?.querySelector("svg")).toBeInTheDocument();
  });

  it("keeps rendered shell actions purposeful", async () => {
    const user = userEvent.setup();
    render(<App />);

    await user.click(screen.getByRole("button", { name: "Dismiss warning preview" }));
    expect(screen.queryByText("Config revision differs from the last successful run")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /View all runs/i }));
    const attentionFilter = await screen.findByRole("button", { name: "attention" });
    await user.click(attentionFilter);
    expect(attentionFilter).toHaveAttribute("aria-pressed", "true");
    expect(screen.queryByRole("button", { name: /Open run Daemon read API/i })).not.toBeInTheDocument();
  });

  it("uses the run's pinned workflow and does not reveal future attempt results", async () => {
    const user = userEvent.setup();
    const { container } = render(<App />);

    await user.click(screen.getByRole("button", { name: /Open run Live visual dashboard/i }));
    expect(await screen.findByText(/Implementation v7/)).toBeInTheDocument();
    expect(screen.getByText(/v7 · 589d28aa/)).toBeInTheDocument();

    fireEvent.change(screen.getByRole("slider", { name: "Replay position" }), {
      target: { value: "3" },
    });

    expect(await screen.findByText("In progress")).toBeInTheDocument();
    expect(screen.queryByText("attempt-1-summary.md")).not.toBeInTheDocument();
    expect(container.querySelector('[data-edge="review-gate->implement"]')).not.toHaveClass("graph-edge-active");
  });
});

function setMediaMatches(matchesByQuery: Record<string, boolean>) {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: matchesByQuery[query] ?? false,
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

function setReducedMotion(matches: boolean) {
  setMediaMatches({ [reducedMotionQuery]: matches });
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
