import { describe, expect, it } from "vitest";
import { DaemonApiError, RequestCancelledError } from "./errors";
import { FixtureDaemonClient, fixtureKey, type DaemonFixtures } from "./fixtureClient";
import {
  API_VERSION,
  SCHEMA_VERSION,
  type DaemonClient,
  type Health,
  type Instance,
} from "./types";

const health: Health = {
  apiVersion: API_VERSION,
  schemaVersion: SCHEMA_VERSION,
  ready: true,
  instance: { name: "fixture", environment: "dev" },
  freshness: {
    observedAt: "2026-07-18T00:00:00Z",
    definitionsLoadedAt: "2026-07-18T00:00:00Z",
    journalUpdatedAt: null,
  },
};

const instance: Instance = {
  apiVersion: API_VERSION,
  schemaVersion: SCHEMA_VERSION,
  name: "fixture",
  environment: "dev",
  ready: true,
  status: "ready",
  concurrency: { activeRuns: 0, maxConcurrentRuns: 1 },
  counts: { gaggles: 0, goobers: 0, workflows: 0, activeRuns: 0 },
  warnings: [],
};

function fixtures(): DaemonFixtures {
  return {
    health,
    instance,
    gaggles: {
      items: [],
      page: { limit: 50, total: 0, hasMore: false, nextCursor: "" },
    },
    runs: { runs: [] },
    runDetails: {
      "run-1": {
        id: "run-1",
        workflow: "implementation",
        workflowVersion: 1,
        gaggle: "core",
        trigger: { kind: "item", ref: "445" },
        phase: "running",
        terminal: false,
        startedAt: "2026-07-18T00:00:00Z",
        durationMillis: 1,
        lastSeq: 1,
        repassCount: 0,
        retryCount: 0,
        policyRetryCount: 0,
        infraRetryCount: 0,
        graphStatus: "unavailable",
      },
    },
    artifacts: {
      [fixtureKey("run-1", "sha256:abc")]: {
        digest: "sha256:abc",
        mediaType: "text/plain",
        size: 2,
        etag: '"sha256:abc"',
        bytes: new TextEncoder().encode("ok").buffer,
      },
    },
    telemetryStats: { runs: [], stages: [] },
    telemetryErrors: { items: [] },
  };
}

describe("FixtureDaemonClient", () => {
  it("satisfies the daemon interface with explicit empty fixtures", async () => {
    const client: DaemonClient = new FixtureDaemonClient(fixtures());

    await expect(client.getHealth()).resolves.toEqual(fixtures().health);
    await expect(client.listGaggles()).resolves.toMatchObject({ items: [] });
    await expect(client.listRuns()).resolves.toEqual({ runs: [] });
    await expect(client.getRun("run-1")).resolves.toMatchObject({ id: "run-1" });
    await expect(client.getArtifact("run-1", "sha256:abc")).resolves.toMatchObject({
      digest: "sha256:abc",
      size: 2,
    });
  });

  it("uses the same structured error type for missing fixture resources", async () => {
    const client = new FixtureDaemonClient(fixtures());

    await expect(client.getRun("missing")).rejects.toBeInstanceOf(DaemonApiError);
    await expect(client.getRun("missing")).rejects.toMatchObject({
      status: 404,
      code: "not_found",
    });
  });

  it("honors cancelled requests", async () => {
    const client = new FixtureDaemonClient(fixtures());
    const controller = new AbortController();
    controller.abort();

    await expect(client.listRuns(undefined, { signal: controller.signal })).rejects.toBeInstanceOf(
      RequestCancelledError,
    );
  });
});
