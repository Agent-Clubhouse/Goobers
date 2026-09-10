import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { RunSummary } from "../api/types";
import { RunTiming } from "./RunTiming";

afterEach(() => { cleanup(); vi.useRealTimers(); });

const run = {
  id: "run", terminal: false, lastSeq: 2,
  startedAt: "2026-09-07T00:00:00Z", durationMillis: 18 * 60_000,
  activeStages: [{ name: "implement", kind: "stage", goober: "implementer",
    attempt: 1, startedAt: "2026-09-07T00:06:00Z" }],
} as RunSummary;

it("separates live workflow and goober elapsed time despite browser clock skew", () => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2030-01-01T00:00:00Z"));
  const view = render(<RunTiming run={run} />);
  expect(screen.getByLabelText("Run elapsed time")).toHaveTextContent("18m");
  expect(screen.getByLabelText("Goober implementer elapsed time")).toHaveTextContent("12m");
  act(() => { vi.advanceTimersByTime(60_000); });
  expect(screen.getByLabelText("Run elapsed time")).toHaveTextContent("19m");
  expect(screen.getByLabelText("Goober implementer elapsed time")).toHaveTextContent("13m");
  view.unmount();
  expect(vi.getTimerCount()).toBe(0);
});

it("updates the attempt start on refresh and does not invent deterministic owners", () => {
  vi.useFakeTimers();
  const view = render(<RunTiming run={run} />);
  view.rerender(<RunTiming run={{ ...run, lastSeq: 4, durationMillis: 20 * 60_000,
    activeStages: [{ name: "build", kind: "stage", attempt: 2, startedAt: "2026-09-07T00:19:00Z" }] }} />);
  expect(screen.getByLabelText("Stage build elapsed time")).toHaveTextContent("1m");
  expect(screen.queryByLabelText(/Goober/)).not.toBeInTheDocument();
});

it("shows parallel activity, missing timestamps and truncation explicitly", () => {
  render(<RunTiming run={{ ...run, activityTruncated: true, activeStages: [
    ...run.activeStages!, { name: "other", kind: "stage", branch: 2, startedAt: "0001-01-01T00:00:00Z" },
  ] }} />);
  expect(screen.getByLabelText("Stage other elapsed time")).toHaveTextContent("other · branch 2 · elapsed unavailable");
  expect(screen.getByText("Additional stage activity omitted")).toBeInTheDocument();
});

it("keeps terminal durations fixed without an active timer", () => {
  vi.useFakeTimers();
  render(<RunTiming run={{ ...run, terminal: true }} />);
  expect(screen.queryByLabelText("Run elapsed time")).not.toBeInTheDocument();
  expect(vi.getTimerCount()).toBe(0);
});
