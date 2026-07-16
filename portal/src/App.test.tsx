import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";

describe("App prototype", () => {
  beforeEach(() => {
    window.location.hash = "#/overview";
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

  it("uses the run's pinned workflow and does not reveal future attempt results", async () => {
    const user = userEvent.setup();
    const { container } = render(<App />);

    await user.click(screen.getByRole("button", { name: /Open run Live visual dashboard/i }));
    expect(await screen.findByText(/Implementation v7/)).toBeInTheDocument();
    expect(screen.getByText(/v7 · 589d28aa/)).toBeInTheDocument();

    fireEvent.change(screen.getByRole("slider", { name: "Replay position" }), {
      target: { value: "3" },
    });

    expect(await screen.findByRole("tab", { name: /Attempt 1.*running.*In progress/i })).toBeInTheDocument();
    expect(screen.queryByText("attempt-1-summary.md")).not.toBeInTheDocument();
    expect(screen.queryByText("Produced a partial API and portal client without a complete slice.")).not.toBeInTheDocument();
    expect(screen.queryByText("17 files, +812/-44")).not.toBeInTheDocument();
    expect(container.querySelector('[data-edge="review-gate->implement"]')).not.toHaveClass("graph-edge-active");

  });

  it("orders attempt classes, follows the selected event, and supports keyboard switching", async () => {
    const user = userEvent.setup();
    window.location.hash = "#/run/01JZ402DASHBOARD";
    render(<App />);

    await user.click(screen.getByRole("button", { name: "Select event 13: Implementation attempt 3 complete" }));
    const attemptList = screen.getByRole("tablist", { name: "Stage attempts" });
    const attempts = within(attemptList).getAllByRole("tab");

    expect(attempts).toHaveLength(3);
    expect(attempts[0]).toHaveTextContent(/Attempt 1.*initial.*completed.*6m 47s/i);
    expect(attempts[1]).toHaveTextContent(/Attempt 2.*infra.*completed.*6m 31s/i);
    expect(attempts[2]).toHaveTextContent(/Attempt 3.*policy.*completed.*5m 33s/i);
    expect(attempts[2]).toHaveAttribute("aria-selected", "true");

    await user.click(attempts[0]);
    expect(attempts[0]).toHaveAttribute("aria-selected", "true");
    attempts[0].focus();
    fireEvent.keyDown(attempts[0], { key: "ArrowRight" });
    expect(attempts[1]).toHaveAttribute("aria-selected", "true");
    expect(attempts[1]).toHaveFocus();

    await user.click(screen.getByRole("button", { name: "Select event 8: Implementation infrastructure retry" }));
    const selectedEventAttempt = screen.getByRole("tab", { name: /Attempt 2.*infra.*running.*In progress/i });
    expect(selectedEventAttempt).toHaveAttribute("aria-selected", "true");
    expect(within(screen.getByRole("tabpanel")).getByText("Outcome is not available at the selected event.")).toBeInTheDocument();
    expect(screen.queryByText("attempt-2-summary.md")).not.toBeInTheDocument();
  });

  it("shows artifact provenance and safely handles content, errors, downloads, and focus return", async () => {
    const user = userEvent.setup();
    window.location.hash = "#/run/01JZ441DAEMONAPI";
    render(<App />);

    fireEvent.change(screen.getByRole("slider", { name: "Replay position" }), {
      target: { value: "4" },
    });
    expect(screen.getByText("implementation-summary.md")).toBeInTheDocument();
    expect(screen.queryByText("18 passed")).not.toBeInTheDocument();

    fireEvent.change(screen.getByRole("slider", { name: "Replay position" }), {
      target: { value: "6" },
    });
    await user.click(screen.getByRole("button", { name: "Implement, complete" }));
    expect(screen.getByText("18 passed")).toBeInTheDocument();
    expect(screen.getByText("#472")).toBeInTheDocument();

    const summaryRow = screen.getByText("implementation-summary.md").closest("article");
    expect(summaryRow).not.toBeNull();
    expect(within(summaryRow!).getByText("text/markdown")).toBeInTheDocument();
    expect(within(summaryRow!).getByText("4.1 KB")).toBeInTheDocument();
    expect(within(summaryRow!).getByText("Attempt 1 · Seq 5")).toBeInTheDocument();
    expect(within(summaryRow!).getByText("Verified")).toBeInTheDocument();
    expect(within(summaryRow!).getByText(/^sha256:/)).toBeInTheDocument();

    const summaryButton = within(summaryRow!).getByRole("button", { name: "View content" });
    fireEvent.click(summaryButton);
    expect(screen.getByRole("status")).toHaveTextContent("Loading artifact content");
    expect(await screen.findByLabelText("implementation-summary.md content")).toHaveTextContent(
      "Added daemon read endpoints and fixture-backed coverage.",
    );
    await user.click(screen.getByRole("button", { name: "Close artifact viewer" }));
    expect(summaryButton).toHaveFocus();

    const pullRequestRow = screen.getByText("pull-request.json").closest("article");
    expect(pullRequestRow).not.toBeNull();
    const pullRequestButton = within(pullRequestRow!).getByRole("button", { name: "View content" });
    fireEvent.click(pullRequestButton);
    expect(await screen.findByLabelText("pull-request.json content")).toHaveTextContent('"number": 472');
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(pullRequestButton).toHaveFocus();

    const manifestRow = screen.getByText("artifact-manifest.json").closest("article");
    expect(manifestRow).not.toBeNull();
    fireEvent.click(within(manifestRow!).getByRole("button", { name: "View content" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Artifact unavailable");
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Artifact content could not be loaded from the local journal.",
    );
    await user.click(screen.getByRole("button", { name: "Close artifact viewer" }));

    const patchRow = screen.getByText("implementation.patch").closest("article");
    expect(patchRow).not.toBeNull();
    const download = within(patchRow!).getByRole("link", { name: "Download" });
    expect(download).toHaveAttribute("download");
    expect(download).toHaveAttribute("href", "/artifacts/01JZ441DAEMONAPI/implementation.patch");
    expect(within(patchRow!).queryByRole("button", { name: "View content" })).not.toBeInTheDocument();
  });

  it("keeps empty attempt detail and static definition context separate", async () => {
    const user = userEvent.setup();
    window.location.hash = "#/run/01JZ441DAEMONAPI";
    render(<App />);

    expect(screen.getByText("No artifacts recorded yet.")).toBeInTheDocument();
    expect(screen.getByText("Definition context")).toBeInTheDocument();
    await user.click(screen.getByText("View stage definition and YAML"));
    expect(screen.getByText(/goober: reviewer/)).toBeInTheDocument();
    expect(within(screen.getByRole("tabpanel")).getByText("Outcome is not available at the selected event.")).toBeInTheDocument();
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
