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
      "workbenchGraph", "workbenchDocuments", "workbenchSources", "workbenchItems", "workbenchItem",
      "workbenchWriteCapabilities", "workbenchPatch", "workbenchCommand",
      "sessionCreate", "sessionInput", "sessionClose", "session", "sessions", "sessionMessages", "sessionAccepted",
      "childPublicationCheck",
      "childPublicationResult",
      "childWorkflowPage",
      "interactiveRun",
      "interactiveRunCommand",
      "interactiveRunResult",
      "interactiveCapabilities",
      "childWorkflowSource",
      "childWorkflowResolve",
      "childWorkflowResolution",
      "childWorkflowStatus",
      "childWorkflowValidation",
      "childWorkflow",
      "triggerRequest",
      "triggerResponse",
      "triggerStatus",
      "cancelRequest",
      "cancelResult",
      "operatorMessageRequest",
      "operatorMessageResponse",
      "queueEligibility",
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
      "telemetryCosts",
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
    expect(checkedFixtures.workbenchGraph.edges[0].owner).toMatchObject({ kind: "manifest", sourceBindingId: "links", path: "links.yaml" });
    expect(checkedFixtures.workbenchGraph.edges[0].to.resolved).toBe(false);
    expect(checkedFixtures.workbenchGraph.partial).toBe(true);
    expect(checkedFixtures.workbenchGraph).not.toHaveProperty("credentials");
    expect(checkedFixtures.workbenchCommand).toMatchObject({ state: "unknown", receipt: { providerAcknowledged: false, observedMatches: true } });
    expect(checkedFixtures.workbenchSources.items[0]).toHaveProperty("bindingId");
    expect(checkedFixtures.workbenchItem.ref).toHaveProperty("sourceId");
    expect(checkedFixtures.workbenchItems).toHaveProperty("sourceTargetDigest");
    expect(checkedFixtures.childWorkflowValidation).toMatchObject({ valid: false, advisory: true });
    expect(checkedFixtures.childWorkflow).toMatchObject({ state: "queued", invocationKey: "inspect-1", cancellationRequested: false });
    expect(checkedFixtures.childWorkflow).not.toHaveProperty("source");
    expect(checkedFixtures.health.apiVersion).toBe("v1");
    expect(checkedFixtures.triggerResponse).toMatchObject({ state: "accepted", duplicate: true });
    expect(checkedFixtures.triggerResponse).not.toHaveProperty("runId");
    expect(checkedFixtures.triggerStatus).toMatchObject({ state: "dispatched", runId: "0123456789abcdef0123456789abcdef" });
    expect(checkedFixtures.cancelResult).toEqual({ code: "cancellation_requested" });
    // Recovery-inventory occupancy is emitted by the Go Instance struct, so
    // this pins the field names the Overview card reads rather than letting a
    // rename pass as an absent optional (#5343).
    expect(checkedFixtures.instance.recoveryInventory).toMatchObject({
      state: "warning",
      used: 104,
      limit: 128,
      unreadable: 2,
      highWaterPercent: 80,
    });
    expect(checkedFixtures.instance.telemetryExporterHealth?.destinations?.tenant).toMatchObject({
      mode: "azure-monitor",
      journal: { accepted: 12, dropped: 0, failures: 0 },
      diagnostics: { accepted: 3 },
      replay: { pendingRecords: 2, activeFailure: true, failureClass: "rejected" },
    });
    expect(checkedFixtures.goobers.items[0].harness).toBe("claude-code");
    expect(checkedFixtures.runDetail.graphStatus).toBe("pinned");
    expect(checkedFixtures.runDetail.terminalCauseStatus).toBe("unavailable");
    expect(checkedFixtures.runEvents.events[0].type).toBe("stage.finished");
    expect(checkedFixtures.runEvents.events[0]).toMatchObject({
      category: "transition",
      replayChapter: true,
    });
    expect(checkedFixtures.telemetryCosts.pullRequests[0]).toMatchObject({
      externalId: "4398",
      coverage: { lowerBound: true },
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
