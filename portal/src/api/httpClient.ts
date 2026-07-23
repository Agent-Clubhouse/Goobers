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
import { apiRoutes, type ApiRoute } from "./contract.generated";
import type {
  ApiErrorEnvelope,
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
  TelemetryErrorSignaturesOptions,
  TelemetryErrorSignaturesResult,
  TelemetryErrorsOptions,
  TelemetryErrorsPage,
  TelemetryStatsOptions,
  TelemetryStatsResult,
  WorkflowDetail,
  WorkflowPage,
} from "./types";

const DEFAULT_TIMEOUT_MS = 10_000;

type QueryValue = string | number | undefined;
type PathParameters = Readonly<Record<string, string>>;

const clientRoutes = {
  health: apiRoutes.health,
  instance: apiRoutes.instance,
  gaggles: apiRoutes.gaggles,
  gaggleGoobers: apiRoutes.gaggleGoobers,
  gaggleWorkflows: apiRoutes.gaggleWorkflows,
  workflowDetail: apiRoutes.workflowDetail,
  runs: apiRoutes.runs,
  runDetail: apiRoutes.runDetail,
  runEvents: apiRoutes.runEvents,
  stageAttempts: apiRoutes.stageAttempts,
  runArtifact: apiRoutes.runArtifact,
  telemetryStats: apiRoutes.telemetryStats,
  telemetryErrorSignatures: apiRoutes.telemetryErrorSignatures,
  telemetryErrors: apiRoutes.telemetryErrors,
  events: apiRoutes.events,
} satisfies { [K in keyof typeof apiRoutes]: (typeof apiRoutes)[K] };

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

  async connectEvents(
    request?: EventStreamRequest,
    options?: RequestOptions,
  ): Promise<DaemonEventStream> {
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
      const headers = new Headers({ Accept: "text/event-stream" });
      if (request?.cursor) {
        headers.set("Last-Event-ID", request.cursor);
      }
      const response = await this.fetch(this.url(clientRoutes.events), {
        method: clientRoutes.events.method,
        headers,
        signal: controller.signal,
      });
      globalThis.clearTimeout(timer);
      if (!response.ok) {
        options?.signal?.removeEventListener("abort", cancel);
        throw await apiError(response);
      }
      if (!response.headers.get("Content-Type")?.toLowerCase().startsWith("text/event-stream")) {
        options?.signal?.removeEventListener("abort", cancel);
        controller.abort();
        await response.body?.cancel();
        throw new MalformedResponseError("The daemon returned an invalid event stream.");
      }
      if (!response.body) {
        options?.signal?.removeEventListener("abort", cancel);
        throw new MalformedResponseError("The daemon returned an empty event stream.");
      }
      return new HttpDaemonEventStream(
        response.body,
        controller,
        () => options?.signal?.removeEventListener("abort", cancel),
      );
    } catch (error) {
      globalThis.clearTimeout(timer);
      options?.signal?.removeEventListener("abort", cancel);
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
    }
  }

  async getHealth(options?: RequestOptions): Promise<Health> {
    const health = await this.getJSON<Health>(clientRoutes.health, undefined, options);
    assertSupportedContractVersion(health);
    return health;
  }

  async getInstance(options?: RequestOptions): Promise<Instance> {
    const instance = await this.getJSON<Instance>(clientRoutes.instance, undefined, options);
    assertSupportedContractVersion(instance);
    return instance;
  }

  listGaggles(request?: PageRequest, options?: RequestOptions): Promise<GagglePage> {
    return this.getJSON(clientRoutes.gaggles, pageQuery(request), options);
  }

  listGoobers(
    gaggle: string,
    request?: PageRequest,
    options?: RequestOptions,
  ): Promise<GooberPage> {
    return this.getJSON(clientRoutes.gaggleGoobers, pageQuery(request), options, { gaggle });
  }

  listWorkflows(
    gaggle: string,
    request?: PageRequest,
    options?: RequestOptions,
  ): Promise<WorkflowPage> {
    return this.getJSON(clientRoutes.gaggleWorkflows, pageQuery(request), options, { gaggle });
  }

  getWorkflow(
    gaggle: string,
    workflow: string,
    options?: RequestOptions,
  ): Promise<WorkflowDetail> {
    return this.getJSON(
      clientRoutes.workflowDetail,
      undefined,
      options,
      { gaggle, workflow },
    );
  }

  listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList> {
    return this.getJSON(
      clientRoutes.runs,
      request && {
        gaggle: request.gaggle,
        workflow: request.workflow,
        stage: request.stage,
        outcome: request.outcome,
        population: request.population,
        phase: request.phase,
        trigger: request.trigger,
        since: request.since,
        until: request.until,
        limit: request.limit,
        cursor: request.cursor,
      },
      options,
    );
  }

  getRun(runId: string, options?: RequestOptions): Promise<RunDetail> {
    return this.getJSON(clientRoutes.runDetail, undefined, options, { run: runId });
  }

  listRunEvents(runId: string, options?: RequestOptions): Promise<EventList> {
    return this.getJSON(clientRoutes.runEvents, undefined, options, { run: runId });
  }

  listStageAttempts(
    runId: string,
    stage: string,
    options?: RequestOptions,
  ): Promise<AttemptList> {
    return this.getJSON(
      clientRoutes.stageAttempts,
      undefined,
      options,
      { run: runId, stage },
    );
  }

  async getArtifact(
    runId: string,
    digest: string,
    options?: RequestOptions,
  ): Promise<ArtifactContent> {
    return this.withResponse(
      clientRoutes.runArtifact,
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
      { run: runId, digest },
    );
  }

  getTelemetryStats(
    request?: TelemetryStatsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryStatsResult> {
    return this.getJSON(
      clientRoutes.telemetryStats,
      request && {
        workflow: request.workflow,
        gaggle: request.gaggle,
        since: request.since,
        until: request.until,
      },
      options,
    );
  }

  getTelemetryErrorSignatures(
    request?: TelemetryErrorSignaturesOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorSignaturesResult> {
    return this.getJSON(
      clientRoutes.telemetryErrorSignatures,
      request && {
        workflow: request.workflow,
        gaggle: request.gaggle,
        stage: request.stage,
        since: request.since,
        until: request.until,
        limit: request.limit,
      },
      options,
    );
  }

  listTelemetryErrors(
    request?: TelemetryErrorsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorsPage> {
    return this.getJSON(
      clientRoutes.telemetryErrors,
      request && {
        workflow: request.workflow,
        gaggle: request.gaggle,
        stage: request.stage,
        code: request.code,
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
    route: ApiRoute,
    query?: Record<string, QueryValue>,
    options?: RequestOptions,
    pathParameters?: PathParameters,
  ): Promise<T> {
    return this.withResponse(route, query, options, "application/json", async (response) => {
      let value: unknown;
      try {
        value = JSON.parse(await response.text());
      } catch (error) {
        throw new MalformedResponseError(undefined, { cause: error });
      }
      return value as T;
    }, pathParameters);
  }

  private async withResponse<T>(
    route: ApiRoute,
    query: Record<string, QueryValue> | undefined,
    options: RequestOptions | undefined,
    accept: string,
    read: (response: Response) => Promise<T>,
    pathParameters?: PathParameters,
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
      const response = await this.fetch(this.url(route, query, pathParameters), {
        method: route.method,
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

  private url(
    route: ApiRoute,
    query?: Record<string, QueryValue>,
    pathParameters?: PathParameters,
  ): string {
    const search = new URLSearchParams();
    for (const [name, value] of Object.entries(query ?? {})) {
      if (value !== undefined) {
        search.set(name, String(value));
      }
    }
    const suffix = search.size > 0 ? `?${search.toString()}` : "";
    return `${this.baseUrl}${routePath(route.path, pathParameters)}${suffix}`;
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

function routePath(template: string, parameters?: PathParameters): string {
  return template.replace(/\{([^}]+)\}/g, (_match, name: string) => {
    const value = parameters?.[name];
    if (value === undefined) {
      throw new TypeError(`Missing path parameter: ${name}`);
    }
    return segment(value);
  });
}

function pageQuery(request?: PageRequest): Record<string, QueryValue> | undefined {
  return request && { limit: request.limit, cursor: request.cursor };
}

interface RawServerEvent {
  data: string;
  id?: string;
  type: string;
}

class HttpDaemonEventStream implements DaemonEventStream {
  private closed = false;
  private readonly reader: ReadableStreamDefaultReader<Uint8Array>;

  constructor(
    body: ReadableStream<Uint8Array>,
    private readonly controller: AbortController,
    private readonly cleanup: () => void,
  ) {
    this.reader = body.getReader();
  }

  close(): void {
    if (this.closed) {
      return;
    }
    this.closed = true;
    this.cleanup();
    this.controller.abort();
    void this.reader.cancel().catch(() => undefined);
  }

  async *[Symbol.asyncIterator](): AsyncIterator<DaemonUpdateEvent> {
    const decoder = new TextDecoder();
    const parser = new ServerEventParser();
    try {
      for (;;) {
        const { done, value } = await this.reader.read();
        if (done) {
          for (const event of parser.finish(decoder.decode())) {
            yield parseUpdateEvent(event);
          }
          return;
        }
        for (const event of parser.push(decoder.decode(value, { stream: true }))) {
          yield parseUpdateEvent(event);
        }
      }
    } catch (error) {
      if (this.controller.signal.aborted) {
        throw new RequestCancelledError({ cause: error });
      }
      if (error instanceof DaemonClientError) {
        throw error;
      }
      throw new DaemonUnavailableError({ cause: error });
    } finally {
      this.close();
    }
  }
}

class ServerEventParser {
  private buffer = "";
  private data: string[] = [];
  private eventId: string | undefined;
  private type = "message";

  push(chunk: string): RawServerEvent[] {
    this.buffer += chunk;
    const events: RawServerEvent[] = [];
    for (;;) {
      const lineEnd = this.buffer.indexOf("\n");
      if (lineEnd < 0) {
        return events;
      }
      let line = this.buffer.slice(0, lineEnd);
      this.buffer = this.buffer.slice(lineEnd + 1);
      if (line.endsWith("\r")) {
        line = line.slice(0, -1);
      }
      const event = this.line(line);
      if (event) {
        events.push(event);
      }
    }
  }

  finish(chunk: string): RawServerEvent[] {
    const events = this.push(chunk);
    if (this.buffer) {
      let line = this.buffer;
      this.buffer = "";
      if (line.endsWith("\r")) {
        line = line.slice(0, -1);
      }
      const event = this.line(line);
      if (event) {
        events.push(event);
      }
    }
    const event = this.dispatch();
    if (event) {
      events.push(event);
    }
    return events;
  }

  private line(line: string): RawServerEvent | undefined {
    if (line === "") {
      return this.dispatch();
    }
    if (line.startsWith(":")) {
      return undefined;
    }
    const colon = line.indexOf(":");
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) {
      value = value.slice(1);
    }
    switch (field) {
      case "data":
        this.data.push(value);
        break;
      case "event":
        this.type = value;
        break;
      case "id":
        if (!value.includes("\0")) {
          this.eventId = value;
        }
        break;
    }
    return undefined;
  }

  private dispatch(): RawServerEvent | undefined {
    if (this.data.length === 0) {
      this.type = "message";
      return undefined;
    }
    const event = { data: this.data.join("\n"), id: this.eventId, type: this.type };
    this.data = [];
    this.eventId = undefined;
    this.type = "message";
    return event;
  }
}

function parseUpdateEvent(event: RawServerEvent): DaemonUpdateEvent {
  let data: unknown;
  try {
    data = JSON.parse(event.data);
  } catch (error) {
    throw new MalformedResponseError("The daemon returned malformed event data.", {
      cause: error,
    });
  }
  if (!isRecord(data) || typeof data.cursor !== "string" || data.cursor === "") {
    throw new MalformedResponseError("The daemon returned an invalid update event.");
  }
  if (event.type === "heartbeat") {
    return { type: "heartbeat", data: { cursor: data.cursor } };
  }
  if (
    (event.type !== "snapshot" && event.type !== "invalidate") ||
    !event.id ||
    event.id !== data.cursor ||
    !Array.isArray(data.models) ||
    data.models.length === 0 ||
    !data.models.every(isUpdateModel)
  ) {
    throw new MalformedResponseError("The daemon returned an invalid update event.");
  }
  const runIds = optionalStringArray(data.runIds, "run IDs");
  const workflows = optionalWorkflowReferences(data.workflows);
  return {
    id: event.id,
    type: event.type,
    data: {
      cursor: data.cursor,
      models: [...new Set(data.models)],
      ...(runIds ? { runIds } : {}),
      ...(workflows ? { workflows } : {}),
    },
  };
}

function isUpdateModel(value: unknown): value is "instance" | "run" | "workflow" {
  return value === "instance" || value === "run" || value === "workflow";
}

function optionalStringArray(value: unknown, label: string): string[] | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (!Array.isArray(value) || !value.every((item) => typeof item === "string")) {
    throw new MalformedResponseError(`The daemon returned invalid ${label}.`);
  }
  return value;
}

function optionalWorkflowReferences(
  value: unknown,
): { gaggle: string; name: string }[] | undefined {
  if (value === undefined) {
    return undefined;
  }
  if (!Array.isArray(value) || !value.every(isWorkflowReference)) {
    throw new MalformedResponseError("The daemon returned invalid workflow references.");
  }
  return value;
}

function isWorkflowReference(value: unknown): value is { gaggle: string; name: string } {
  return (
    isRecord(value) &&
    typeof value.gaggle === "string" &&
    typeof value.name === "string" &&
    value.name !== ""
  );
}
