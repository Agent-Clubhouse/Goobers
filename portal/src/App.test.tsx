import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";

beforeEach(() => {
  window.localStorage.clear();
  delete document.documentElement.dataset.theme;
});

describe("portal foundation", () => {
  beforeEach(() => {
    window.location.hash = "#/overview";
  });

  it.each([
    ["#/overview", "One run needs attention."],
    ["#/workflows", "Workflows"],
    ["#/runs", "Runs"],
  ])("renders the static fixture route %s", (hash, heading) => {
    window.location.hash = hash;
    render(<App />);

    expect(screen.getByRole("heading", { name: heading })).toBeInTheDocument();
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

  it("loads and persists an independently selected theme", async () => {
    window.localStorage.setItem("goobers-theme", "dark");
    const user = userEvent.setup();
    render(<App />);

    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    await user.click(screen.getByRole("button", { name: "Use light theme" }));
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    expect(window.localStorage.getItem("goobers-theme")).toBe("light");
  });

  it("supports directional keyboard navigation between primary routes", async () => {
    const user = userEvent.setup();
    render(<App />);

    screen.getByRole("button", { name: "Overview" }).focus();
    await user.keyboard("{ArrowRight}{Enter}");

    expect(await screen.findByRole("heading", { name: "Workflows" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Workflows" })).toHaveAttribute("aria-current", "page");
  });

  it("puts responsive layout classes on headers and actionable rows", () => {
    window.location.hash = "#/runs";
    render(<App />);

    expect(screen.getByText("Current stage").closest(".data-header")).toHaveClass("all-runs-grid");
    expect(screen.getByRole("button", { name: /Open run Live visual dashboard/i })).toHaveClass("all-runs-grid");
    expect(screen.getByRole("button", { name: "All runs" })).toHaveAttribute("aria-pressed", "true");
  });

  it("does not present unavailable artifact content as an action", () => {
    renderRun("01JZ402DASHBOARD");
    fireEvent.change(screen.getByRole("slider", { name: "Replay position" }), {
      target: { value: "12" },
    });

    const artifact = screen.getByText("attempt-3-summary.md");
    expect(artifact.closest("button")).toBeNull();
  });
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
