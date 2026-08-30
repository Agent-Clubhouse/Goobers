import { describe, expect, it } from "vitest";
import type { ApiErrorEnvelope, DaemonUpdateEvent } from "./types";
import { goWireFixtures, type GoWireFixtures } from "./wire.generated";

const checkedFixtures: GoWireFixtures = goWireFixtures;
const checkedUpdateEvent: DaemonUpdateEvent = {
  id: "fixture:9",
  type: "invalidate",
  data: checkedFixtures.eventInvalidation,
};
const checkedErrorEnvelope: ApiErrorEnvelope = checkedFixtures.errorEnvelope;

describe("Go daemon wire contract", () => {
  it("provides typed fixtures for every JSON response consumed by the portal", () => {
    expect(Object.keys(checkedFixtures)).toEqual([
      "health",
      "instance",
      "portalConfig",
      "gaggles",
      "goobers",
      "workflows",
      "workflowDetail",
      "runs",
      "runDetail",
      "runEvents",
      "stageAttempts",
      "telemetryStats",
      "telemetryErrorSignatures",
      "telemetryErrors",
      "configSources",
      "configDocuments",
      "configDocumentRequest",
      "configDocument",
      "configPreviewRequest",
      "configPreview",
      "configWriteRequest",
      "configWriteOutcome",
      "configAuthoringError",
      "eventInvalidation",
      "errorEnvelope",
    ]);
    expect(checkedFixtures.health.apiVersion).toBe("v1");
    expect(checkedFixtures.goobers.items[0].harness).toBe("claude-code");
    expect(checkedFixtures.runDetail.graphStatus).toBe("pinned");
    expect(checkedFixtures.runEvents.events[0].type).toBe("stage.finished");
    expect(checkedFixtures.runEvents.events[0]).toMatchObject({
      category: "transition",
      replayChapter: true,
    });
    expect(checkedFixtures.configSources.items.map(({ kind }) => kind)).toEqual([
      "local",
      "git",
      "provider",
    ]);
    expect(checkedFixtures.configPreviewRequest.changeSet.changes).toHaveLength(2);
    expect(checkedFixtures.configWriteOutcome).toMatchObject({
      strategy: "review",
      review: { id: "review:42" },
    });
    expect(checkedFixtures.configAuthoringError.error.code).toBe(
      "config_stale_revision",
    );
    expect(checkedUpdateEvent.data.models).toEqual(["instance", "run", "workflow"]);
    expect(checkedErrorEnvelope.error.code).toBe("not_found");
  });
});
