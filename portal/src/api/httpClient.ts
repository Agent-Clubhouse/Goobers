import {
  DaemonApiError,
  DaemonAuthError,
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
  PortalDiagnostics,
  PortalRequestStatus,
} from "../portalDiagnostics";
import type {
  ApiErrorEnvelope,
  ArtifactContent,
  AttemptList,
  DaemonClient,
  DaemonEventStream,
  DaemonUpdateEvent,
  EventList,
  EventStreamRequest,
  GaggleConnections,
  GagglePage,
  GooberPage,
  Health,
  Instance,
  PageRequest,
  PortalConfig,
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
  TranscriptContent,
  WorkflowDetail,
  WorkflowPage,
  ReadState,
} from "./types";

const DEFAULT_TIMEOUT_MS = 10_000;

type QueryValue = string | number | undefined;
type PathParameters = Readonly<Record<string, string>>;

const clientRoutes = {
  health: apiRoutes.health,
  instance: apiRoutes.instance,
  portalConfig: apiRoutes.portalConfig,
  gaggles: apiRoutes.gaggles,
  gaggleGoobers: apiRoutes.gaggleGoobers,
  gaggleWorkflows: apiRoutes.gaggleWorkflows,
  gaggleConnections: apiRoutes.gaggleConnections,
  workflowDetail: apiRoutes.workflowDetail,
  runs: apiRoutes.runs,
  runDetail: apiRoutes.runDetail,
  runReveal: apiRoutes.runReveal,
  runEvents: apiRoutes.runEvents,
  stageAttempts: apiRoutes.stageAttempts,
  runArtifact: apiRoutes.runArtifact,
  runTranscript: apiRoutes.runTranscript,
  telemetryStats: apiRoutes.telemetryStats,
  telemetryErrorSignatures: apiRoutes.telemetryErrorSignatures,
  telemetryErrors: apiRoutes.telemetryErrors,
  events: apiRoutes.events,
  // Tier-2 human-intervention stub routes (HITL-7/#469). No DaemonClient
  // method calls these yet — the real approve/override/rerun UI wiring is
  // #466/#468's scope — but they must appear here for this exhaustiveness
  // check to keep passing as the contract grows.
  approveStage: apiRoutes.approveStage,
  overrideStage: apiRoutes.overrideStage,
  rerunStage: apiRoutes.rerunStage,
  // Daemon write planes (#3509): machine seams (claims, trigger ingestion)
  // plus HITL escalation resolution. The portal calls none of them yet — an
  // escalation-resolution UI would be the first consumer — but the
  // exhaustiveness check requires the full contract here as it grows.
  claimAcquire: apiRoutes.claimAcquire,
  claimRenew: apiRoutes.claimRenew,
  claimRelease: apiRoutes.claimRelease,
  claimSettle: apiRoutes.claimSettle,
  claimList: apiRoutes.claimList,
  triggerIngest: apiRoutes.triggerIngest,
  resolveEscalation: apiRoutes.resolveEscalation,
  journalEmit: apiRoutes.journalEmit,
  credentialResolve: apiRoutes.credentialResolve,
  // The blob plane (decision 010/012, §2a): a mode-3 stage pod's BlobClient
  // fetches and puts content-addressed artifacts by digest. Pod-only, like
  // the credential plane — the portal never calls these and never will (the
  // daemon refuses a human principal outright) — but the exhaustiveness
  // check requires the full contract here as it grows.
  blobGet: apiRoutes.blobGet,
  blobPut: apiRoutes.blobPut,
  // The surrender plane (#3699): a mode-3 stage pod's dispatch-exec
  // entrypoint PUTs its terminal result here. Pod-only, like the credential
  // and blob planes — the portal never calls this and never will — but the
  // exhaustiveness check requires the full contract here as it grows.
  stageSurrender: apiRoutes.stageSurrender,
  // The scheduler-state plane (#3878, decision 005 R3): a mode-3 stage pod
  // reads and compare-and-swaps its gaggle's scheduler state (blocked.json,
  // the backlog scan cursors, the reconcile ledger, the sibling-context
  // cache) here, under the same locks the in-process path takes. Pod-only,
  // like the credential, blob, and surrender planes — the portal never calls
  // these — but the exhaustiveness check requires the full contract here as
  // it grows.
  gaggleStateGet: apiRoutes.gaggleStateGet,
  gaggleStatePut: apiRoutes.gaggleStatePut,
} satisfies { [K in keyof typeof apiRoutes]: (typeof apiRoutes)[K] };

export interface HttpDaemonClientConfig {
  baseUrl?: string;
  timeoutMs?: number;
  fetch?: typeof fetch;
  diagnostics?: PortalDiagnostics;
  /**
   * Called with the readState envelope on every JSON response that carries one
   * (#1928).
   *
   * Wired here, at the single JSON decode point, rather than in each of the
   * twelve query hooks: freshness is a property of the CONNECTION TO THE DATA,
   * not of any one query, and threading it through every hook would mean each
   * one could forget. One place, or the "data freshness" indicator quietly
   * reflects only the surfaces someone remembered to wire.
   */
  onReadState?: (state: ReadState) => void;
}

export class HttpDaemonClient implements DaemonClient {
  private readonly baseUrl: string;
  private readonly diagnostics: PortalDiagnostics | undefined;
  private readonly timeoutMs: number;
  private readonly fetch: typeof fetch;
  private readonly onReadState: ((state: ReadState) => void) | undefined;

  constructor(config: HttpDaemonClientConfig = {}) {
    const timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
      throw new RangeError("Daemon request timeout must be a positive finite number.");
    }
    this.baseUrl = normalizeBaseUrl(config.baseUrl ?? "");
    this.diagnostics = config.diagnostics;
    this.onReadState = config.onReadState;
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
    const requestUrl = this.url(clientRoutes.events);
    const trace = this.diagnostics?.startRequest({
      endpoint: requestUrl,
      method: clientRoutes.events.method,
    });
    let responseStatus: number | undefined;

    try {
      const headers = new Headers({ Accept: "text/event-stream" });
      if (request?.cursor) {
        headers.set("Last-Event-ID", request.cursor);
      }
      const response = await this.fetch(requestUrl, {
        method: clientRoutes.events.method,
        headers,
        signal: controller.signal,
      });
      responseStatus = response.status;
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
    } finally {
      trace?.finish(responseStatus ?? diagnosticStatus(abortKind));
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

  getPortalConfig(options?: RequestOptions): Promise<PortalConfig> {
    return this.getJSON(clientRoutes.portalConfig, undefined, options);
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

  getGaggleConnections(
    gaggle: string,
    options?: RequestOptions,
  ): Promise<GaggleConnections> {
    return this.getJSON(clientRoutes.gaggleConnections, undefined, options, { gaggle });
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
        latestPerWorkflow: request.latestPerWorkflow ? "true" : undefined,
        showNoWork: request.showNoWork ? "true" : undefined,
        orderByActivity: request.orderByActivity ? "true" : undefined,
      },
      options,
    );
  }

  getRun(runId: string, options?: RequestOptions): Promise<RunDetail> {
    return this.getJSON(clientRoutes.runDetail, undefined, options, { run: runId });
  }

  revealRun(runId: string, options?: RequestOptions): Promise<void> {
    return this.withResponse(
      clientRoutes.runReveal,
      undefined,
      options,
      "application/json",
      async () => undefined,
      { run: runId },
    );
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

  async getTranscript(
    runId: string,
    seq: number,
    options?: RequestOptions,
  ): Promise<TranscriptContent> {
    return this.withResponse(
      clientRoutes.runTranscript,
      undefined,
      options,
      "text/plain",
      async (response) => {
        const responseSeq = Number(response.headers.get("X-Goobers-Event-Sequence"));
        const stage = response.headers.get("X-Goobers-Stage");
        const name = response.headers.get("X-Goobers-Transcript-Name");
        const rawSize = response.headers.get("Content-Length");
        const size = rawSize === null ? Number.NaN : Number(rawSize);
        if (
          responseSeq !== seq ||
          !stage ||
          !name ||
          !Number.isSafeInteger(size) ||
          size <= 0
        ) {
          throw new MalformedResponseError("The daemon returned invalid transcript metadata.");
        }
        const bytes = await response.arrayBuffer();
        if (bytes.byteLength !== size) {
          throw new MalformedResponseError("The daemon returned a transcript with an invalid size.");
        }
        return { seq: responseSeq, stage, name, size, bytes };
      },
      { run: runId, seq: String(seq) },
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
        trendSince: request.trendSince,
        trendUntil: request.trendUntil,
        trendBuckets: request.trendBuckets,
        trendPreviousSince: request.trendPreviousSince,
        trendPreviousUntil: request.trendPreviousUntil,
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
      this.observeReadState(value);
      return value as T;
    }, pathParameters);
  }

  /**
   * Reports a response's freshness envelope, if it carries one.
   *
   * Deliberately tolerant: the field is optional (the CLI and standalone
   * topologies attach no read model), an older daemon omits it entirely, and a
   * malformed one must not break a response that is otherwise fine. This is
   * metadata about an answer that already parsed — it cannot be allowed to fail
   * the answer.
   */
  private observeReadState(value: unknown): void {
    if (!this.onReadState || typeof value !== "object" || value === null) {
      return;
    }
    const candidate = (value as { readState?: unknown }).readState;
    if (typeof candidate !== "object" || candidate === null) {
      return;
    }
    const state = candidate as Partial<ReadState>;
    if (typeof state.lagSeconds !== "number" || !Array.isArray(state.degraded)) {
      return;
    }
    this.onReadState(state as ReadState);
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
    const requestUrl = this.url(route, query, pathParameters);
    const trace = this.diagnostics?.startRequest({
      endpoint: requestUrl,
      method: route.method,
    });
    let responseStatus: number | undefined;

    try {
      const response = await this.fetch(requestUrl, {
        method: route.method,
        headers: { Accept: accept },
        signal: controller.signal,
      });
      responseStatus = response.status;
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
      trace?.finish(responseStatus ?? diagnosticStatus(abortKind));
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

function diagnosticStatus(
  abortKind: "cancelled" | "timeout" | undefined,
): PortalRequestStatus {
  return abortKind ?? "error";
}

async function apiError(
  response: Response,
): Promise<DaemonApiError | DaemonAuthError | MalformedResponseError> {
  // A 401/403 is classified from the status alone, before the body is ever
  // read. An intermediary in front of the daemon (a reverse proxy, an SSO
  // gateway) can reject a request with an HTML login page or plain text
  // instead of the daemon's JSON error envelope; that must still be
  // reported as an auth failure rather than a malformed response or, once
  // it unwinds through the caller, a misleading "daemon unavailable" (#2916).
  if (response.status === 401 || response.status === 403) {
    return new DaemonAuthError(response.status);
  }
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
