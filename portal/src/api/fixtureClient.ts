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
  RunEvent,
  RunDetail,
  RunList,
  RunListOptions,
  RunPhase,
  RunSummary,
  StageAttemptStatus,
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
    let runs = this.fixtures.runs.runs.filter((run) =>
      matchesRunRequest(run, this.fixtures.runEvents?.[run.id]?.events ?? [], request),
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

  async getTelemetryStats(
    request?: TelemetryStatsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryStatsResult> {
    throwIfCancelled(options);
    const stats = this.fixtures.telemetryStats;
    return structuredClone({
      gaggles: stats.gaggles.filter(
        (item) => !request?.gaggle || item.gaggle === request.gaggle,
      ),
      runs: stats.runs.filter(
        (item) =>
          (!request?.gaggle || item.gaggle === request.gaggle) &&
          (!request?.workflow || item.workflow === request.workflow),
      ),
      stages: stats.stages.filter(
        (item) =>
          (!request?.gaggle || item.gaggle === request.gaggle) &&
          (!request?.workflow || item.workflow === request.workflow),
      ),
    });
  }

  listTelemetryErrors(
    _request?: TelemetryErrorsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorsPage> {
    return fixture(this.fixtures.telemetryErrors, options);
  }
}

interface FixtureStageAttempt {
  class: string;
  finishedAt?: string;
  number: number;
  startedAt?: string;
  status: StageAttemptStatus | "";
}

function matchesRunRequest(
  run: RunSummary,
  events: RunEvent[],
  request?: RunListOptions,
): boolean {
  if (
    (request?.gaggle && run.gaggle !== request.gaggle) ||
    (request?.workflow && run.workflow !== request.workflow) ||
    (request?.phase && run.phase !== request.phase) ||
    (request?.trigger && run.trigger.kind !== request.trigger) ||
    (request?.since && Date.parse(run.startedAt) < Date.parse(request.since)) ||
    (request?.until && Date.parse(run.startedAt) > Date.parse(request.until))
  ) {
    return false;
  }
  if ((request?.outcome || request?.population) && !run.terminal) {
    return false;
  }

  if (!request?.stage) {
    return !request?.outcome || matchesOutcome(run.phase, request.outcome);
  }
  const stageEvents = events.filter((event) => event.stage === request.stage);
  if (!request.outcome && !request.population) {
    return stageEvents.length > 0 || events.some((event) => event.gate === request.stage);
  }
  return fixtureStageAttempts(stageEvents).some(
    (attempt) =>
      (!request.population ||
        request.population === "attempts" ||
        isMeasuredAttempt(attempt)) &&
      (!request.outcome || matchesAttemptOutcome(attempt.status, request.outcome)),
  );
}

function fixtureStageAttempts(events: RunEvent[]): FixtureStageAttempt[] {
  const attempts: FixtureStageAttempt[] = [];
  for (const event of events) {
    if (event.type === "stage.started") {
      attempts.push({
        class: event.attemptClass ?? "initial",
        number: event.attempt ?? 0,
        startedAt: event.time,
        status: "running",
      });
      continue;
    }
    const status =
      event.type === "stage.finished"
        ? (event.status as StageAttemptStatus | undefined)
        : event.type === "error" && event.error?.code === "executor_error"
          ? "failure"
          : undefined;
    if (!status) {
      continue;
    }
    const attemptClass = event.attemptClass ?? "initial";
    const number = event.attempt ?? 0;
    let index = -1;
    for (let candidate = attempts.length - 1; candidate >= 0; candidate -= 1) {
      const attempt = attempts[candidate];
      if (!attempt.finishedAt && attempt.number === number && attempt.class === attemptClass) {
        index = candidate;
        break;
      }
    }
    if (index < 0) {
      index = attempts.push({
        class: attemptClass,
        number,
        status: "",
      }) - 1;
    }
    attempts[index].finishedAt = event.time;
    attempts[index].status = status;
  }
  return attempts;
}

function isMeasuredAttempt(attempt: FixtureStageAttempt): boolean {
  return (
    attempt.startedAt !== undefined &&
    attempt.finishedAt !== undefined &&
    Date.parse(attempt.finishedAt) >= Date.parse(attempt.startedAt)
  );
}

function matchesOutcome(
  status: RunPhase,
  outcome: NonNullable<RunListOptions["outcome"]>,
): boolean {
  switch (outcome) {
    case "finished":
      return status !== "running";
    case "terminal":
      return status === "completed" || status === "failed";
    case "success":
      return status === "completed";
    case "failure":
      return status === "failed";
    case "other":
      return status === "aborted" || status === "escalated";
  }
}

function matchesAttemptOutcome(
  status: StageAttemptStatus | "",
  outcome: NonNullable<RunListOptions["outcome"]>,
): boolean {
  switch (outcome) {
    case "finished":
      return true;
    case "terminal":
      return status === "success" || status === "failure";
    case "success":
      return status === "success";
    case "failure":
      return status === "failure";
    case "other":
      return status !== "success" && status !== "failure";
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
