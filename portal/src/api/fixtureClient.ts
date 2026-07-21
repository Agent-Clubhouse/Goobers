import {
  DaemonApiError,
  RequestCancelledError,
  assertSupportedContractVersion,
} from "./errors";
import type {
  ArtifactContent,
  AttemptList,
  DaemonClient,
  DaemonEventStream,
  DaemonUpdateEvent,
  EventList,
  EventStreamRequest,
  GagglePage,
  GooberPage,
  Health,
  Instance,
  PageRequest,
  RequestOptions,
  RunDetail,
  RunList,
  RunListOptions,
  RunSummary,
  TelemetryErrorsOptions,
  TelemetryErrorsPage,
  TelemetryStatsOptions,
  TelemetryStatsResult,
  WorkflowDetail,
  WorkflowPage,
} from "./types";

export interface DaemonFixtures {
  health: Health;
  instance: Instance;
  gaggles: GagglePage;
  goobers?: Record<string, GooberPage>;
  workflows?: Record<string, WorkflowPage>;
  workflowDetails?: Record<string, WorkflowDetail>;
  runs: RunList;
  runDetails?: Record<string, RunDetail>;
  runEvents?: Record<string, EventList>;
  stageAttempts?: Record<string, AttemptList>;
  artifacts?: Record<string, ArtifactContent>;
  telemetryStats: TelemetryStatsResult;
  telemetryErrors: TelemetryErrorsPage;
}

const DEFAULT_RUN_LIMIT = 50;

interface FixtureRunCursor {
  startedAt: string;
  id: string;
}

export class FixtureDaemonClient implements DaemonClient {
  constructor(private readonly fixtures: DaemonFixtures) {
    assertSupportedContractVersion(fixtures.health);
    assertSupportedContractVersion(fixtures.instance);
  }

  connectEvents(
    request?: EventStreamRequest,
    options?: RequestOptions,
  ): Promise<DaemonEventStream> {
    throwIfCancelled(options);
    return Promise.resolve(fixtureEventStream(request?.cursor, options?.signal));
  }

  getHealth(options?: RequestOptions): Promise<Health> {
    return fixture(this.fixtures.health, options);
  }

  getInstance(options?: RequestOptions): Promise<Instance> {
    return fixture(this.fixtures.instance, options);
  }

  listGaggles(_request?: PageRequest, options?: RequestOptions): Promise<GagglePage> {
    return fixture(this.fixtures.gaggles, options);
  }

  async listGoobers(
    gaggle: string,
    _request?: PageRequest,
    options?: RequestOptions,
  ): Promise<GooberPage> {
    return fixture(required(this.fixtures.goobers, gaggle, "goobers"), options);
  }

  async listWorkflows(
    gaggle: string,
    _request?: PageRequest,
    options?: RequestOptions,
  ): Promise<WorkflowPage> {
    return fixture(required(this.fixtures.workflows, gaggle, "workflows"), options);
  }

  async getWorkflow(
    gaggle: string,
    workflow: string,
    options?: RequestOptions,
  ): Promise<WorkflowDetail> {
    return fixture(
      required(this.fixtures.workflowDetails, fixtureKey(gaggle, workflow), "workflow"),
      options,
    );
  }

  // Emulates the daemon's deterministic run listing so filtered and paginated
  // reads exercise the same server-side contract in tests as in production:
  // newest StartedAt first with RunID ascending as the tie-break, gaggle /
  // workflow / phase / trigger filtered server-side, and keyset pagination on
  // (StartedAt, RunID). See internal/readservice/runs.go ListRuns.
  async listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList> {
    throwIfCancelled(options);
    const limit = request?.limit ?? DEFAULT_RUN_LIMIT;
    let runs = this.fixtures.runs.runs.filter(
      (run) =>
        (!request?.gaggle || run.gaggle === request.gaggle) &&
        (!request?.workflow || run.workflow === request.workflow) &&
        (!request?.phase || run.phase === request.phase) &&
        (!request?.trigger || run.trigger.kind === request.trigger),
    );
    runs = [...runs].sort(compareRunsNewestFirst);
    if (request?.cursor) {
      const cursor = decodeFixtureCursor(request.cursor);
      runs = runs.filter((run) => runAfterCursor(run, cursor));
    }
    let nextCursor: string | undefined;
    if (runs.length > limit) {
      runs = runs.slice(0, limit);
      nextCursor = encodeFixtureCursor(runs[runs.length - 1]);
    }
    return structuredClone(nextCursor ? { runs, nextCursor } : { runs });
  }

  async getRun(runId: string, options?: RequestOptions): Promise<RunDetail> {
    return fixture(required(this.fixtures.runDetails, runId, "run"), options);
  }

  async listRunEvents(runId: string, options?: RequestOptions): Promise<EventList> {
    return fixture(required(this.fixtures.runEvents, runId, "run events"), options);
  }

  async listStageAttempts(
    runId: string,
    stage: string,
    options?: RequestOptions,
  ): Promise<AttemptList> {
    return fixture(
      required(this.fixtures.stageAttempts, fixtureKey(runId, stage), "stage attempts"),
      options,
    );
  }

  async getArtifact(
    runId: string,
    digest: string,
    options?: RequestOptions,
  ): Promise<ArtifactContent> {
    const value = required(this.fixtures.artifacts, fixtureKey(runId, digest), "artifact");
    throwIfCancelled(options);
    return { ...value, bytes: value.bytes.slice(0) };
  }

  getTelemetryStats(
    _request?: TelemetryStatsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryStatsResult> {
    return fixture(this.fixtures.telemetryStats, options);
  }

  listTelemetryErrors(
    _request?: TelemetryErrorsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorsPage> {
    return fixture(this.fixtures.telemetryErrors, options);
  }
}

export function fixtureKey(...parts: string[]): string {
  return JSON.stringify(parts);
}

function required<T>(
  values: Record<string, T> | undefined,
  key: string,
  resource: string,
): T {
  const value = values?.[key];
  if (value === undefined) {
    throw new DaemonApiError(404, "not_found", `Fixture ${resource} not found.`);
  }
  return value;
}

async function fixture<T>(value: T, options?: RequestOptions): Promise<T> {
  throwIfCancelled(options);
  return structuredClone(value);
}

function throwIfCancelled(options?: RequestOptions): void {
  if (options?.signal?.aborted) {
    throw new RequestCancelledError();
  }
}

function compareRunsNewestFirst(left: RunSummary, right: RunSummary): number {
  return (
    Date.parse(right.startedAt) - Date.parse(left.startedAt) ||
    left.id.localeCompare(right.id)
  );
}

function runAfterCursor(run: RunSummary, cursor: FixtureRunCursor): boolean {
  const runStarted = Date.parse(run.startedAt);
  const cursorStarted = Date.parse(cursor.startedAt);
  return runStarted < cursorStarted || (runStarted === cursorStarted && run.id > cursor.id);
}

function encodeFixtureCursor(run: RunSummary): string {
  return JSON.stringify({ startedAt: run.startedAt, id: run.id } satisfies FixtureRunCursor);
}

function decodeFixtureCursor(value: string): FixtureRunCursor {
  return JSON.parse(value) as FixtureRunCursor;
}

function fixtureEventStream(
  cursor: string | undefined,
  signal: AbortSignal | undefined,
): DaemonEventStream {
  let closed = false;
  let release!: () => void;
  const stopped = new Promise<void>((resolve) => {
    release = resolve;
  });
  const close = () => {
    if (closed) {
      return;
    }
    closed = true;
    signal?.removeEventListener("abort", close);
    release();
  };
  signal?.addEventListener("abort", close, { once: true });

  return {
    close,
    async *[Symbol.asyncIterator]() {
      if (!cursor) {
        const snapshot: DaemonUpdateEvent = {
          id: "fixture:0",
          type: "snapshot",
          data: {
            cursor: "fixture:0",
            models: ["instance", "run", "workflow"],
          },
        };
        yield snapshot;
      }
      await stopped;
    },
  };
}
