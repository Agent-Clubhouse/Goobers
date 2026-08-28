import { describe, expect, it } from "vitest";
import type { RunEvent } from "./api/types";
import { deriveVisitLineage, deriveWaterfall } from "./causalDiagnosis";

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
});
