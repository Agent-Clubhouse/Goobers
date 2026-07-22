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
import { populatedDaemonFixtures } from "../test/daemonFixtures";

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
        lastActivityAt: "2026-07-18T00:00:01Z",
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
    telemetryStats: { gaggles: [], runs: [], stages: [], models: [] },
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

  it("returns only runs in the requested outcome and stage-attempt population", async () => {
    const client = new FixtureDaemonClient(populatedDaemonFixtures());
    const ids = async (request: Parameters<DaemonClient["listRuns"]>[0]) =>
      (await client.listRuns(request)).runs.map((run) => run.id);

    await expect(
      ids({ gaggle: "core", workflow: "implementation", outcome: "finished" }),
    ).resolves.toEqual(["01JZ402DASHBOARD", "01JZ400FAILED", "01JZ455ESCALATE"]);
    await expect(
      ids({ gaggle: "core", workflow: "implementation", outcome: "terminal" }),
    ).resolves.toEqual(["01JZ400FAILED", "01JZ455ESCALATE"]);
    await expect(
      ids({ gaggle: "core", workflow: "implementation", outcome: "success" }),
    ).resolves.toEqual(["01JZ455ESCALATE"]);
    await expect(
      ids({ gaggle: "core", workflow: "implementation", outcome: "failure" }),
    ).resolves.toEqual(["01JZ400FAILED"]);
    await expect(
      ids({ gaggle: "core", workflow: "implementation", outcome: "other" }),
    ).resolves.toEqual(["01JZ402DASHBOARD"]);

    await expect(
      ids({
        gaggle: "core",
        workflow: "implementation",
        stage: "implement",
        population: "attempts",
      }),
    ).resolves.toEqual(["01JZ402DASHBOARD", "01JZ400FAILED", "01JZ455ESCALATE"]);
    await expect(
      ids({
        gaggle: "core",
        workflow: "implementation",
        stage: "implement",
        population: "measured",
      }),
    ).resolves.toEqual(["01JZ402DASHBOARD", "01JZ400FAILED", "01JZ455ESCALATE"]);
    await expect(
      ids({
        gaggle: "core",
        workflow: "implementation",
        stage: "implement",
        outcome: "failure",
      }),
    ).resolves.toEqual(["01JZ402DASHBOARD", "01JZ400FAILED"]);
    await expect(
      ids({
        gaggle: "core",
        workflow: "implementation",
        stage: "implement",
        outcome: "other",
      }),
    ).resolves.toEqual([]);
    await expect(
      ids({
        gaggle: "tools",
        workflow: "implementation",
        stage: "implement",
        population: "attempts",
      }),
    ).resolves.toEqual(["01JZ300ABORTED"]);
    await expect(
      ids({
        gaggle: "tools",
        workflow: "implementation",
        stage: "implement",
        population: "measured",
      }),
    ).resolves.toEqual([]);
  });
});
