import { render, screen } from "@testing-library/react";
import { createElement } from "react";
import { describe, expect, it } from "vitest";
import type { RunDetail, RunEvent } from "./api/types";
import { CausalDiagnosis } from "./components/CausalDiagnosis";
import { deriveFailureBreadcrumb, deriveVisitLineage, deriveWaterfall } from "./causalDiagnosis";

function event(fields: Partial<RunEvent>): RunEvent {
  return {
    schema: "v1",
    seq: 1,
    type: "stage.started",
    branch: 0,
    time: "2026-01-01T00:00:00.000Z",
    knownSchema: true,
    ...fields,
  };
}

describe("causal diagnosis helpers", () => {
  it("keeps repeated stage visits and their attempts distinct", () => {
    const events = [
      event({ seq: 1, type: "stage.started", stage: "build", attempt: 1 }),
      event({ seq: 2, type: "stage.finished", stage: "build", attempt: 1, status: "failure" }),
      event({ seq: 3, type: "stage.started", stage: "build", attempt: 2, time: "2026-01-01T00:00:02.000Z" }),
      event({ seq: 4, type: "stage.finished", stage: "build", attempt: 2, status: "success", time: "2026-01-01T00:00:03.000Z" }),
    ];
    expect(deriveVisitLineage(events)).toMatchObject([
      { stage: "build", visit: 1, attempt: 1, status: "failure" },
      { stage: "build", visit: 2, attempt: 2, status: "success" },
    ]);
  });

  it("reports durations and idle gaps from durable timestamps", () => {
    const events = [
      event({ seq: 1, stage: "build", time: "2026-01-01T00:00:00.000Z" }),
      event({ seq: 2, type: "stage.finished", stage: "build", time: "2026-01-01T00:00:01.000Z" }),
      event({ seq: 3, type: "stage.started", stage: "test", time: "2026-01-01T00:00:04.000Z" }),
      event({ seq: 4, type: "stage.finished", stage: "test", time: "2026-01-01T00:00:06.000Z" }),
    ];
    expect(deriveWaterfall(events)).toMatchObject([
      { stage: "build", durationMillis: 1000, idleBeforeMillis: 0 },
      { stage: "test", durationMillis: 2000, idleBeforeMillis: 3000 },
    ]);
  });

  it("keeps orphaned finishes visible with an unknown start", () => {
    const visits = deriveVisitLineage([
      event({ type: "stage.finished", stage: "late", attempt: 3, status: "failure" }),
    ]);
    expect(visits).toEqual([
      expect.objectContaining({
        stage: "late",
        visit: 1,
        attempt: 3,
        finishedAt: "2026-01-01T00:00:00.000Z",
        status: "failure",
      }),
    ]);
  });

  it("does not derive durations from invalid timestamps", () => {
    const rows = deriveWaterfall([
      event({ seq: 1, stage: "build", time: "not-a-time" }),
      event({ seq: 2, type: "stage.finished", stage: "build", time: "also-invalid" }),
    ]);
    expect(rows[0]).toMatchObject({ durationMillis: undefined, idleBeforeMillis: 0 });
  });

  it("tracks idle gaps independently for parallel branches", () => {
    const rows = deriveWaterfall([
      event({ seq: 1, branch: 1, stage: "a", time: "2026-01-01T00:00:00.000Z" }),
      event({ seq: 2, branch: 2, stage: "b", time: "2026-01-01T00:00:00.000Z" }),
      event({ seq: 3, branch: 1, type: "stage.finished", stage: "a", time: "2026-01-01T00:00:02.000Z" }),
      event({ seq: 4, branch: 2, type: "stage.finished", stage: "b", time: "2026-01-01T00:00:03.000Z" }),
      event({ seq: 5, branch: 1, stage: "c", time: "2026-01-01T00:00:04.000Z" }),
    ]);
    expect(rows.map((row) => row.idleBeforeMillis)).toEqual([0, 0, 2000]);
  });

  it("uses the latest relevant failure and its stage and attempt", () => {
    const run = { phase: "failed" } as RunDetail;
    const breadcrumb = deriveFailureBreadcrumb(run, [
      event({ seq: 1, stage: "old", error: { code: "old", message: "old error" } }),
      event({ seq: 2, stage: "failed-stage", attempt: 4, error: { code: "new", message: "new error" } }),
    ]);
    expect(breadcrumb).toBe("new error → failed-stage · Attempt 4");
  });

  it("renders the empty diagnosis state when no transitions exist", () => {
    render(
      createElement(CausalDiagnosis, {
        events: [],
        run: { id: "run-1", phase: "running" } as RunDetail,
      }),
    );
    expect(screen.getByText("No stage transitions are available for this run.")).toBeInTheDocument();
  });

  it("renders lineage and waterfall details for populated transitions", () => {
    const events = [
      event({ seq: 1, stage: "build", attempt: 1, time: "2026-01-01T00:00:00.000Z" }),
      event({
        seq: 2,
        type: "stage.finished",
        stage: "build",
        attempt: 1,
        status: "success",
        time: "2026-01-01T00:00:02.000Z",
      }),
    ];
    render(
      createElement(CausalDiagnosis, {
        events,
        run: { id: "run-1", phase: "completed" } as RunDetail,
      }),
    );

    expect(screen.getAllByText("build")).toHaveLength(2);
    expect(screen.getByText("Attempt 1 · success")).toBeInTheDocument();
    expect(screen.getByText(/2s · success/)).toBeInTheDocument();
  });
});
