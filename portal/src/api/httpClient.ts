import {
  DaemonApiError,
  DaemonClientError,
  DaemonUnavailableError,
  MalformedResponseError,
  RequestCancelledError,
  RequestTimeoutError,
  assertSupportedContractVersion,
  isRecord,
} from "./errors";
import type {
  ApiErrorEnvelope,
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

const API_PREFIX = "/api/v1";
const DEFAULT_TIMEOUT_MS = 10_000;

type QueryValue = string | number | undefined;

export interface HttpDaemonClientConfig {
  baseUrl?: string;
  timeoutMs?: number;
  fetch?: typeof fetch;
}

export class HttpDaemonClient implements DaemonClient {
  private readonly baseUrl: string;
  private readonly timeoutMs: number;
  private readonly fetch: typeof fetch;

  constructor(config: HttpDaemonClientConfig = {}) {
    const timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
      throw new RangeError("Daemon request timeout must be a positive finite number.");
    }
    this.baseUrl = normalizeBaseUrl(config.baseUrl ?? "");
    this.timeoutMs = timeoutMs;
    const fetcher = config.fetch ?? globalThis.fetch;
    if (typeof fetcher !== "function") {
      throw new TypeError("A Fetch API implementation is required.");
    }
    this.fetch = fetcher.bind(globalThis);
  }

  async getHealth(options?: RequestOptions): Promise<Health> {
    const health = await this.getJSON<Health>("/health", undefined, options);
    assertSupportedContractVersion(health);
    return health;
  }

  async getInstance(options?: RequestOptions): Promise<Instance> {
    const instance = await this.getJSON<Instance>("/instance", undefined, options);
    assertSupportedContractVersion(instance);
    return instance;
  }

  listGaggles(request?: PageRequest, options?: RequestOptions): Promise<GagglePage> {
    return this.getJSON("/gaggles", pageQuery(request), options);
  }

  listGoobers(
    gaggle: string,
    request?: PageRequest,
    options?: RequestOptions,
  ): Promise<GooberPage> {
    return this.getJSON(`/gaggles/${segment(gaggle)}/goobers`, pageQuery(request), options);
  }

  listWorkflows(
    gaggle: string,
    request?: PageRequest,
    options?: RequestOptions,
  ): Promise<WorkflowPage> {
    return this.getJSON(`/gaggles/${segment(gaggle)}/workflows`, pageQuery(request), options);
  }

  getWorkflow(
    gaggle: string,
    workflow: string,
    options?: RequestOptions,
  ): Promise<WorkflowDetail> {
    return this.getJSON(
      `/gaggles/${segment(gaggle)}/workflows/${segment(workflow)}`,
      undefined,
      options,
    );
  }

  listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList> {
    return this.getJSON(
      "/runs",
      request && {
        gaggle: request.gaggle,
        workflow: request.workflow,
        phase: request.phase,
        trigger: request.trigger,
        limit: request.limit,
        cursor: request.cursor,
      },
      options,
    );
  }

  getRun(runId: string, options?: RequestOptions): Promise<RunDetail> {
    return this.getJSON(`/runs/${segment(runId)}`, undefined, options);
  }

  listRunEvents(runId: string, options?: RequestOptions): Promise<EventList> {
    return this.getJSON(`/runs/${segment(runId)}/events`, undefined, options);
  }

  listStageAttempts(
    runId: string,
    stage: string,
    options?: RequestOptions,
  ): Promise<AttemptList> {
    return this.getJSON(
      `/runs/${segment(runId)}/stages/${segment(stage)}/attempts`,
      undefined,
      options,
    );
  }

  async getArtifact(
    runId: string,
    digest: string,
    options?: RequestOptions,
  ): Promise<ArtifactContent> {
    return this.withResponse(
      `/runs/${segment(runId)}/artifacts/${segment(digest)}`,
      undefined,
      options,
      "*/*",
      async (response) => {
        const responseDigest = response.headers.get("X-Goobers-Digest");
        const mediaType = response.headers.get("Content-Type");
        const rawSize = response.headers.get("Content-Length");
        const size = rawSize === null ? Number.NaN : Number(rawSize);
        if (
          !responseDigest ||
          responseDigest !== digest ||
          !mediaType ||
          !Number.isSafeInteger(size) ||
          size < 0
        ) {
          throw new MalformedResponseError("The daemon returned invalid artifact metadata.");
        }
        const bytes = await response.arrayBuffer();
        if (bytes.byteLength !== size) {
          throw new MalformedResponseError("The daemon returned an artifact with an invalid size.");
        }
        return {
          digest: responseDigest,
          mediaType,
          size,
          etag: response.headers.get("ETag"),
          bytes,
        };
      },
    );
  }

  getTelemetryStats(
    request?: TelemetryStatsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryStatsResult> {
    return this.getJSON(
      "/telemetry/stats",
      request && {
        workflow: request.workflow,
        gaggle: request.gaggle,
        since: request.since,
        until: request.until,
      },
      options,
    );
  }

  listTelemetryErrors(
    request?: TelemetryErrorsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorsPage> {
    return this.getJSON(
      "/telemetry/errors",
      request && {
        workflow: request.workflow,
        gaggle: request.gaggle,
        class: request.errorClass,
        since: request.since,
        until: request.until,
        limit: request.limit,
        cursor: request.cursor,
      },
      options,
    );
  }

  private async getJSON<T>(
    path: string,
    query?: Record<string, QueryValue>,
    options?: RequestOptions,
  ): Promise<T> {
    return this.withResponse(path, query, options, "application/json", async (response) => {
      let value: unknown;
      try {
        value = JSON.parse(await response.text());
      } catch (error) {
        throw new MalformedResponseError(undefined, { cause: error });
      }
      return value as T;
    });
  }

  private async withResponse<T>(
    path: string,
    query: Record<string, QueryValue> | undefined,
    options: RequestOptions | undefined,
    accept: string,
    read: (response: Response) => Promise<T>,
  ): Promise<T> {
    if (options?.signal?.aborted) {
      throw new RequestCancelledError();
    }

    const controller = new AbortController();
    let abortKind: "cancelled" | "timeout" | undefined;
    const cancel = () => {
      abortKind = "cancelled";
      controller.abort();
    };
    options?.signal?.addEventListener("abort", cancel, { once: true });
    const timer = globalThis.setTimeout(() => {
      abortKind = "timeout";
      controller.abort();
    }, this.timeoutMs);

    try {
      const response = await this.fetch(this.url(path, query), {
        method: "GET",
        headers: { Accept: accept },
        signal: controller.signal,
      });
      if (!response.ok) {
        throw await apiError(response);
      }
      return await read(response);
    } catch (error) {
      if (abortKind === "cancelled" || options?.signal?.aborted) {
        throw new RequestCancelledError({ cause: error });
      }
      if (abortKind === "timeout") {
        throw new RequestTimeoutError(this.timeoutMs, { cause: error });
      }
      if (error instanceof DaemonClientError) {
        throw error;
      }
      throw new DaemonUnavailableError({ cause: error });
    } finally {
      globalThis.clearTimeout(timer);
      options?.signal?.removeEventListener("abort", cancel);
    }
  }

  private url(path: string, query?: Record<string, QueryValue>): string {
    const search = new URLSearchParams();
    for (const [name, value] of Object.entries(query ?? {})) {
      if (value !== undefined) {
        search.set(name, String(value));
      }
    }
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `${this.baseUrl}${API_PREFIX}${path}${suffix}`;
  }
}

async function apiError(response: Response): Promise<DaemonApiError | MalformedResponseError> {
  let value: unknown;
  try {
    value = JSON.parse(await response.text());
  } catch (error) {
    return new MalformedResponseError("The daemon returned a malformed error response.", {
      cause: error,
    });
  }
  if (!isApiErrorEnvelope(value)) {
    return new MalformedResponseError("The daemon returned a malformed error response.");
  }
  return new DaemonApiError(response.status, value.error.code, value.error.message);
}

function isApiErrorEnvelope(value: unknown): value is ApiErrorEnvelope {
  return (
    isRecord(value) &&
    isRecord(value.error) &&
    typeof value.error.code === "string" &&
    typeof value.error.message === "string"
  );
}

function normalizeBaseUrl(value: string): string {
  if (value === "/") {
    return "";
  }
  return value.replace(/\/+$/, "");
}

function segment(value: string): string {
  return encodeURIComponent(value);
}

function pageQuery(request?: PageRequest): Record<string, QueryValue> | undefined {
  return request && { limit: request.limit, cursor: request.cursor };
}
