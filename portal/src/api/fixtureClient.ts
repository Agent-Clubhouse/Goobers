import {
  DaemonApiError,
  RequestCancelledError,
  assertSupportedContractVersion,
} from "./errors";
import type {
  ArtifactContent,
  AttemptList,
  DaemonClient,
  EventList,
  GagglePage,
  GooberPage,
  Health,
  Instance,
  PageRequest,
  RequestOptions,
  RunDetail,
  RunList,
  RunListOptions,
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

export class FixtureDaemonClient implements DaemonClient {
  constructor(private readonly fixtures: DaemonFixtures) {
    assertSupportedContractVersion(fixtures.health);
    assertSupportedContractVersion(fixtures.instance);
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

  listRuns(_request?: RunListOptions, options?: RequestOptions): Promise<RunList> {
    return fixture(this.fixtures.runs, options);
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
