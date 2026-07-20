import { describe, expect, it } from "vitest";
import { goWireFixtures, type GoWireFixtures } from "./wire.generated";

const checkedFixtures: GoWireFixtures = goWireFixtures;

describe("Go daemon wire contract", () => {
  it("provides typed fixtures for every JSON response consumed by the portal", () => {
    expect(Object.keys(checkedFixtures)).toEqual([
      "health",
      "instance",
      "gaggles",
      "goobers",
      "workflows",
      "workflowDetail",
      "runs",
      "runDetail",
      "runEvents",
      "stageAttempts",
      "telemetryStats",
      "telemetryErrors",
    ]);
    expect(checkedFixtures.health.apiVersion).toBe("v1");
    expect(checkedFixtures.runDetail.graphStatus).toBe("pinned");
    expect(checkedFixtures.runEvents.events[0].type).toBe("stage.finished");
  });
});
