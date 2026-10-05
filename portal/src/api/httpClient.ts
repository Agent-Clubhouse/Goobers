import type { SuggestionSelection, SuggestionInventory, SuggestionBatch, SuggestionPreviewRequest, SuggestionPreview, SuggestionDecisionRequest, SuggestionReview } from "./workbenchSuggestionTypes";
import type { PRRepairCommand } from "./prRepairTypes";
import type { MetadataChangeRequest, MetadataPreview, MetadataProposalCommand } from "./workbenchProposalTypes";
import { readMetadataResponse } from "./workbenchProposalTransport";
import type { StartQueuePage, StartQueueItem, StartQueueCancelInput } from "./startQueueTypes";
import type { WorkbenchGraph } from "./workbenchGraphTypes";
import type { WorkbenchDocumentPage, WorkbenchDocumentPageRequest } from "./workbenchDocumentTypes";
import type { BacklogWriteCapabilities, BacklogPatchInput, BacklogEditCommand } from "./workbenchWriteTypes";
import type { BacklogItem, BacklogPage, BacklogItemRequest, BacklogPageRequest, WorkbenchSourcePage } from "./workbenchTypes";
import {
  DaemonApiError,
  DaemonAuthError,
  DaemonClientError,
  DaemonUnavailableError,
  MalformedResponseError,
  RequestCancelledError,
  RequestTimeoutError,
  assertSupportedContractVersion,
  isAdmissionFailure,
  isRecord,
} from "./errors";
import { apiRoutes, type ApiRoute } from "./contract.generated";
import {
  normalizeLegacyTelemetryCosts,
  normalizeLegacyWorkItemCost,
} from "./legacyCostCompatibility";
import { publishUpdateAvailability } from "../updateNotice";
import type {
  PortalDiagnostics,
  PortalRequestStatus,
} from "../portalDiagnostics";
import type {
  ApiErrorEnvelope,
  AdmissionDegradedState,
  ArtifactContent,
  AttemptList,
  DaemonClient,
  InteractiveCapabilities, InteractiveSession, SessionPage, SessionMessagePage, SessionCreateRequest, SessionMessageRequest, SessionCloseRequest, SessionAcceptance,
  InteractiveRunView, InteractiveRunCommand, InteractiveRunCommandResult,
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
  TelemetryCostOptions,
  TelemetryCostResult,
  TelemetryErrorsOptions,
  TelemetryErrorsPage,
  TelemetryStatsOptions,
  TelemetryStatsResult,
  TranscriptContent,
  WorkflowDetail,
  QueueEligibilityView,
  WorkflowPage,
  WorkItemDetail,
  WorkItemKind,
  WorkItemListOptions,
  WorkItemPage,
  ReadState,
} from "./types";

const DEFAULT_TIMEOUT_MS = 10_000;

type QueryValue = string | number | undefined;
type PathParameters = Readonly<Record<string, string>>;

// Keep all transport lookups tied directly to the generated route contract.
const clientRoutes = apiRoutes;

export interface HttpDaemonClientConfig {
  baseUrl?: string;
  timeoutMs?: number;
  fetch?: typeof fetch;
  diagnostics?: PortalDiagnostics;
  maxConcurrentRequests?: number;
  admissionMaxRetries?: number;
  admissionRetryBaseMs?: number;
  admissionRetryMaxMs?: number;
  onAdmissionState?: (state: AdmissionDegradedState | undefined) => void;
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
  private readonly requests: RequestCoordinator;
  private readonly sharedJSON = new Map<string, SharedJSONRequest>();

  constructor(config: HttpDaemonClientConfig = {}) {
    const timeoutMs = config.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
      throw new RangeError("Daemon request timeout must be a positive finite number.");
    }
    this.baseUrl = normalizeBaseUrl(config.baseUrl ?? "");
    this.diagnostics = config.diagnostics;
    this.onReadState = config.onReadState;
    this.requests = new RequestCoordinator({
      diagnostics: config.diagnostics,
      maxConcurrent: config.maxConcurrentRequests ?? 2,
      maxRetries: config.admissionMaxRetries ?? 1,
      retryBaseMs: config.admissionRetryBaseMs ?? 1_000,
      retryMaxMs: config.admissionRetryMaxMs ?? 30_000,
      onAdmissionState: config.onAdmissionState,
    });
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
    let timer: ReturnType<typeof globalThis.setTimeout> | undefined;
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
    // Observed, not polled (#4920): the update strip reads whatever health
    // responses the app already makes, so it adds no request of its own and
    // cannot perturb the order consumers of this endpoint depend on.
    publishUpdateAvailability(health.update);
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

  getWorkflowQueueEligibility(gaggle: string, workflow: string, options?: RequestOptions): Promise<QueueEligibilityView> {
    return this.getJSON(clientRoutes.workflowQueueEligibility, undefined, options, { gaggle, workflow });
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
    return this.getJSON<TelemetryStatsResult>(
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
    ).then(normalizeLegacyTelemetryCosts);
  }

  getTelemetryCosts(
    request: TelemetryCostOptions,
    options?: RequestOptions,
  ): Promise<TelemetryCostResult> {
    return this.getJSON(
      clientRoutes.telemetryCosts,
      {
        provider: request.provider,
        scope: request.scope,
        id: request.id,
        gaggle: request.gaggle,
        workflow: request.workflow,
        stage: request.stage,
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

  listWorkItems(
    request?: WorkItemListOptions,
    options?: RequestOptions,
  ): Promise<WorkItemPage> {
    return this.getJSON(
      clientRoutes.workItems,
      request && {
        provider: request.provider,
        kind: request.kind,
        limit: request.limit,
      },
      options,
    );
  }

  getWorkItem(
    provider: string,
    repository: string,
    kind: WorkItemKind,
    externalId: string,
    options?: RequestOptions,
  ): Promise<WorkItemDetail> {
    return this.getJSON<WorkItemDetail>(
      clientRoutes.workItemDetail,
      { repository },
      options,
      { provider, kind, id: externalId },
    ).then(normalizeLegacyWorkItemCost);
  }

  listSuggestionArtifacts(gaggle: string, run: string, after?: number, options?: RequestOptions): Promise<SuggestionInventory> {
    return this.withResponse(clientRoutes.workbenchSuggestionArtifacts, after === undefined ? undefined : { after }, options, "application/json", (response) => readMetadataResponse<SuggestionInventory>(response, 1 << 20), { gaggle, run });
  }
  loadSuggestions(gaggle: string, selection: SuggestionSelection, options?: RequestOptions): Promise<SuggestionBatch> {
    return this.withResponse(clientRoutes.workbenchSuggestionLoad, undefined, options, "application/json", (response) => readMetadataResponse<SuggestionBatch>(response, 1 << 20), { gaggle, run: selection.runId, sequence: String(selection.sequence) });
  }
  previewSuggestion(gaggle: string, input: SuggestionPreviewRequest, options?: RequestOptions): Promise<SuggestionPreview> {
    return this.suggestionRequest(clientRoutes.workbenchSuggestionPreview, gaggle, input, (4 << 20) + 4096, options);
  }
  decideSuggestion(gaggle: string, input: SuggestionDecisionRequest, options?: RequestOptions): Promise<SuggestionReview> {
    return this.suggestionRequest(clientRoutes.workbenchSuggestionDecide, gaggle, input, 1 << 20, options);
  }
  getSuggestionReview(gaggle: string, review: string, options?: RequestOptions): Promise<SuggestionReview> {
    return this.withResponse(clientRoutes.workbenchSuggestionReview, undefined, options, "application/json", (response) => readMetadataResponse<SuggestionReview>(response, 1 << 20), { gaggle, review });
  }
  private suggestionRequest<T>(route: ApiRoute, gaggle: string, input: SuggestionPreviewRequest | SuggestionDecisionRequest, limit: number, options?: RequestOptions): Promise<T> {
    const body = JSON.stringify(input);
    if (new TextEncoder().encode(body).byteLength > 16384) throw new Error("The suggestion request exceeds its byte bound.");
    return this.withResponse(route, undefined, options, "application/json", (response) => readMetadataResponse<T>(response, limit), { gaggle }, { body, headers: { "Content-Type": "application/json" } });
  }

  previewMetadataChange(gaggle: string, source: string, input: MetadataChangeRequest, options?: RequestOptions): Promise<MetadataPreview> {
    return this.metadataProposalRequest(clientRoutes.workbenchProposalPreview, { gaggle, source }, input, 4 << 20, undefined, options);
  }
  submitMetadataProposal(gaggle: string, source: string, key: string, input: MetadataChangeRequest, options?: RequestOptions): Promise<MetadataProposalCommand> {
    return this.metadataProposalRequest(clientRoutes.workbenchProposalSubmit, { gaggle, source }, input, 128 << 10, key, options);
  }
  getMetadataProposal(gaggle: string, source: string, command: string, options?: RequestOptions): Promise<MetadataProposalCommand> {
    return this.withResponse(clientRoutes.workbenchProposal, undefined, options, "application/json", (response) => readMetadataResponse<MetadataProposalCommand>(response, 128 << 10), { gaggle, source, command });
  }
  checkMetadataProposal(gaggle: string, source: string, command: string, options?: RequestOptions): Promise<MetadataProposalCommand> {
    return this.metadataProposalRequest(clientRoutes.workbenchProposalCheck, { gaggle, source, command }, {}, 128 << 10, undefined, options);
  }
  continueMetadataProposal(gaggle: string, source: string, command: string, options?: RequestOptions): Promise<MetadataProposalCommand> {
    return this.metadataProposalRequest(clientRoutes.workbenchProposalContinue, { gaggle, source, command }, {}, 128 << 10, undefined, options);
  }
  private async metadataProposalRequest<T>(route: ApiRoute, path: PathParameters, input: MetadataChangeRequest | Record<string, never>, limit: number, key?: string, options?: RequestOptions): Promise<T> {
    const body = JSON.stringify(input);
    if (new TextEncoder().encode(body).byteLength > (2 << 20)) throw new Error("The encoded source edit exceeds the two-MiB request bound.");
    const headers: Record<string, string> = { "Content-Type": "application/json" };
    if (key !== undefined) headers["Idempotency-Key"] = key;
    return this.withResponse(route, undefined, options, "application/json", (response) => readMetadataResponse<T>(response, limit), path, { body, headers });
  }

  getWorkbenchGraph(gaggle: string, options?: RequestOptions): Promise<WorkbenchGraph> {
    return this.getJSON(clientRoutes.workbenchGraph, undefined, options, { gaggle });
  }

  getWorkbenchWriteCapabilities(gaggle: string, source: string, options?: RequestOptions): Promise<BacklogWriteCapabilities> {
    return this.getJSON(clientRoutes.workbenchWriteCapabilities, undefined, options, { gaggle, source });
  }

  patchWorkbenchItem(gaggle: string, source: string, item: string, key: string, input: BacklogPatchInput, options?: RequestOptions): Promise<BacklogEditCommand> {
    return this.withResponse(clientRoutes.workbenchPatch, undefined, options, "application/json", async (response) => {
      try { return JSON.parse(await response.text()) as BacklogEditCommand; }
      catch (error) { throw new MalformedResponseError(undefined, { cause: error }); }
    }, { gaggle, source, item }, { body: JSON.stringify(input), headers: { "Content-Type": "application/json", "Idempotency-Key": key } });
  }

  getWorkbenchCommand(gaggle: string, source: string, command: string, options?: RequestOptions): Promise<BacklogEditCommand> {
    return this.getJSON(clientRoutes.workbenchCommand, undefined, options, { gaggle, source, command });
  }

  getPRRepairCommand(gaggle: string, command: string, options?: RequestOptions): Promise<PRRepairCommand> {
    return this.withResponse(clientRoutes.prRepairCommand, undefined, options, "application/json", (response) => readMetadataResponse<PRRepairCommand>(response, 128 << 10), { gaggle, command });
  }
  checkPRRepairCommand(gaggle: string, command: string, options?: RequestOptions): Promise<PRRepairCommand> {
    return this.withResponse(clientRoutes.prRepairCheck, undefined, options, "application/json", (response) => readMetadataResponse<PRRepairCommand>(response, 128 << 10), { gaggle, command }, { body: "{}", headers: { "Content-Type": "application/json" } });
  }
  inspectPullRequest(gaggle: string, source: string, pullRequest: string, options?: RequestOptions): Promise<import("./types").SessionPRRepairInspection> {
    return this.getJSON(clientRoutes.workbenchPRSelection, undefined, options, { gaggle, source, pullRequest });
  }

  listWorkbenchSources(gaggle: string, options?: RequestOptions): Promise<WorkbenchSourcePage> {
    return this.getJSON(clientRoutes.workbenchSources, undefined, options, { gaggle });
  }

  getStartQueue(gaggle: string, request?: PageRequest, options?: RequestOptions): Promise<StartQueuePage> {
    return this.getJSON(clientRoutes.startQueue, request && { cursor: request.cursor, limit: request.limit }, options, { gaggle });
  }
  getStartQueueItem(gaggle: string, acceptance: string, options?: RequestOptions): Promise<StartQueueItem> {
    return this.getJSON(clientRoutes.startQueueItem, undefined, options, { gaggle, acceptance });
  }
  cancelQueuedStart(gaggle: string, acceptance: string, input: StartQueueCancelInput, options?: RequestOptions): Promise<StartQueueItem> {
    return this.withResponse(clientRoutes.startQueueCancel, undefined, options, "application/json", async (response) => {
      try { return JSON.parse(await response.text()) as StartQueueItem; }
      catch (error) { throw new MalformedResponseError(undefined, { cause: error }); }
    }, { gaggle, acceptance }, { body: JSON.stringify(input), headers: { "Content-Type": "application/json" } });
  }

  getWorkbenchDocuments(gaggle: string, source: string, request?: WorkbenchDocumentPageRequest, options?: RequestOptions): Promise<WorkbenchDocumentPage> {
    return this.getJSON(clientRoutes.workbenchDocuments, request && { cursor: request.cursor, limit: request.limit }, options, { gaggle, source });
  }

  getWorkbenchItems(gaggle: string, source: string, request?: BacklogPageRequest, options?: RequestOptions): Promise<BacklogPage> {
    return this.getJSON(clientRoutes.workbenchItems, request && { cursor: request.cursor, limit: request.limit }, options, { gaggle, source });
  }

  getWorkbenchItem(gaggle: string, source: string, request: BacklogItemRequest, options?: RequestOptions): Promise<BacklogItem> {
    return this.getJSON(clientRoutes.workbenchItem, { expectedSourceId: request.expectedSourceId }, options, { gaggle, source, item: request.id });
  }

  getInteractiveCapabilities(gaggle: string, options?: RequestOptions): Promise<InteractiveCapabilities> {
    return this.getJSON(clientRoutes.gaggleInteractiveCapabilities, undefined, options, { gaggle });
  }

  listSessions(gaggle: string, request?: PageRequest, options?: RequestOptions): Promise<SessionPage> {
    return this.getJSON(clientRoutes.sessionList, request && { cursor: request.cursor, limit: request.limit }, options, { gaggle });
  }

  getSession(gaggle: string, session: string, options?: RequestOptions): Promise<InteractiveSession> {
    return this.getJSON(clientRoutes.sessionGet, undefined, options, { gaggle, session });
  }

  getSessionMessages(gaggle: string, session: string, after?: number, options?: RequestOptions): Promise<SessionMessagePage> {
    return this.getJSON(clientRoutes.sessionMessages, after === undefined ? undefined : { after }, options, { gaggle, session });
  }

  createSession(gaggle: string, key: string, input: SessionCreateRequest, options?: RequestOptions): Promise<SessionAcceptance> {
    return this.sessionCommand(clientRoutes.sessionCreate, { gaggle }, key, input, options);
  }

  sendSessionMessage(gaggle: string, session: string, key: string, input: SessionMessageRequest, options?: RequestOptions): Promise<SessionAcceptance> {
    return this.sessionCommand(clientRoutes.sessionMessage, { gaggle, session }, key, input, options);
  }

  closeSession(gaggle: string, session: string, key: string, input: SessionCloseRequest, options?: RequestOptions): Promise<SessionAcceptance> {
    return this.sessionCommand(clientRoutes.sessionClose, { gaggle, session }, key, input, options);
  }

  private sessionCommand(route: ApiRoute, path: PathParameters, key: string, input: unknown, options?: RequestOptions): Promise<SessionAcceptance> {
    return this.withResponse(route, undefined, options, "application/json", async (response) => {
      try { return JSON.parse(await response.text()) as SessionAcceptance; }
      catch (error) { throw new MalformedResponseError(undefined, { cause: error }); }
    }, path, { body: JSON.stringify(input), headers: { "Content-Type": "application/json", "Idempotency-Key": key } });
  }

  checkChildPublication(runId: string, key: string, command: import("./types").ChildPublicationCheckRequest, options?: RequestOptions): Promise<import("./types").ChildPublicationCheckResult> {
    return this.withResponse(clientRoutes.childPublicationCheck, undefined, options, "application/json", async (response) => {
      try { return JSON.parse(await response.text()) as import("./types").ChildPublicationCheckResult; }
      catch (error) { throw new MalformedResponseError(undefined, { cause: error }); }
    }, { run: runId }, { body: JSON.stringify(command), headers: { "Content-Type": "application/json", "Idempotency-Key": key } });
  }

  getChildWorkflows(runId: string, after?: string, options?: RequestOptions): Promise<import("./types").ChildWorkflowPage> {
    return this.getJSON(clientRoutes.childWorkflowMonitor, after ? { after } : undefined, options, { run: runId });
  }

  getInteractiveRun(runId: string, options?: RequestOptions): Promise<InteractiveRunView> {
    return this.getJSON(clientRoutes.interactiveRun, undefined, options, { run: runId });
  }

  commandInteractiveRun(runId: string, key: string, command: InteractiveRunCommand, options?: RequestOptions): Promise<InteractiveRunCommandResult> {
    return this.withResponse(clientRoutes.interactiveRunCommand, undefined, options, "application/json", async (response) => {
      try { return JSON.parse(await response.text()) as InteractiveRunCommandResult; }
      catch (error) { throw new MalformedResponseError(undefined, { cause: error }); }
    }, { run: runId }, { body: JSON.stringify(command), headers: { "Content-Type": "application/json", "Idempotency-Key": key } });
  }

  private async getJSON<T>(
    route: ApiRoute,
    query?: Record<string, QueryValue>,
    options?: RequestOptions,
    pathParameters?: PathParameters,
  ): Promise<T> {
    const requestUrl = this.url(route, query, pathParameters);
    return this.coalesceJSON<T>(requestUrl, options?.signal, (signal) =>
      this.withResponse(
        route,
        query,
        { signal },
        "application/json",
        async (response) => {
          let value: unknown;
          try {
            value = JSON.parse(await response.text());
          } catch (error) {
            throw new MalformedResponseError(undefined, { cause: error });
          }
          this.observeReadState(value);
          return value as T;
        },
        pathParameters,
      ),
    );
  }

  private coalesceJSON<T>(
    key: string,
    signal: AbortSignal | undefined,
    load: (signal: AbortSignal) => Promise<T>,
  ): Promise<T> {
    if (signal?.aborted) {
      return Promise.reject(new RequestCancelledError());
    }
    let shared = this.sharedJSON.get(key);
    if (shared) {
      this.diagnostics?.recordRequestQueue?.({
        endpoint: key,
        event: "coalesced",
        inFlight: 0,
        queueDepth: shared.subscribers.size,
        requestClass: requestClassFor(key),
      });
    }
    if (!shared) {
      const controller = new AbortController();
      const promise = load(controller.signal);
      shared = { controller, promise, subscribers: new Set() };
      this.sharedJSON.set(key, shared);
      void promise.then(
        () => {
          if (this.sharedJSON.get(key) === shared) this.sharedJSON.delete(key);
        },
        () => {
          if (this.sharedJSON.get(key) === shared) this.sharedJSON.delete(key);
        },
      );
    }

    const token = Symbol(key);
    shared.subscribers.add(token);
    return new Promise<T>((resolve, reject) => {
      let settled = false;
      const finish = () => {
        if (settled) return false;
        settled = true;
        signal?.removeEventListener("abort", cancel);
        shared!.subscribers.delete(token);
        return true;
      };
      const cancel = () => {
        if (!finish()) return;
        if (shared!.subscribers.size === 0) {
          if (this.sharedJSON.get(key) === shared) {
            this.sharedJSON.delete(key);
          }
          shared!.controller.abort();
        }
        reject(new RequestCancelledError());
      };
      signal?.addEventListener("abort", cancel, { once: true });
      void (shared!.promise as Promise<T>).then(
        (value) => {
          if (finish()) resolve(value);
        },
        (error: unknown) => {
          if (finish()) reject(error);
        },
      );
    });
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
    mutation?: { body: string; headers: Record<string, string> },
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
    let timer: ReturnType<typeof globalThis.setTimeout> | undefined;
    const requestUrl = this.url(route, query, pathParameters);
    const trace = this.diagnostics?.startRequest({
      endpoint: requestUrl,
      method: route.method,
    });
    let responseStatus: number | undefined;

    try {
      const attempt = async () => {
        timer = globalThis.setTimeout(() => {
          abortKind = "timeout";
          controller.abort();
        }, this.timeoutMs);
        try {
          const next = await this.fetch(requestUrl, {
            method: route.method,
            headers: { Accept: accept, ...mutation?.headers },
            body: mutation?.body,
            signal: controller.signal,
          });
          responseStatus = next.status;
          if (!next.ok) {
            throw await apiError(next);
          }
          return await read(next);
        } finally {
          if (timer !== undefined) {
            globalThis.clearTimeout(timer);
            timer = undefined;
          }
        }
      };
      // Mutation retries belong to the operator and must retain the same key.
      return await (mutation ? attempt() : this.requests.run(requestUrl, controller.signal, attempt));
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
      if (timer !== undefined) {
        globalThis.clearTimeout(timer);
      }
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
  return new DaemonApiError(
    response.status,
    value.error.code,
    value.error.message,
    retryAfterMilliseconds(response.headers.get("Retry-After")),
  );
}

function retryAfterMilliseconds(value: string | null): number | undefined {
  if (!value) return undefined;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0) {
    return seconds * 1_000;
  }
  const date = Date.parse(value);
  return Number.isFinite(date) ? Math.max(0, date - Date.now()) : undefined;
}

interface SharedJSONRequest {
  controller: AbortController;
  promise: Promise<unknown>;
  subscribers: Set<symbol>;
}

interface CoordinatedRequest<T> {
  endpoint: string;
  requestClass: RequestClass;
  queuedAt: number;
  signal: AbortSignal;
  attempt: () => Promise<T>;
  resolve: (value: T) => void;
  reject: (error: unknown) => void;
  failures: number;
  cancelled: boolean;
  cancel: () => void;
}

type RequestClass = "aggregate" | "interactive";

interface RequestCoordinatorConfig {
  diagnostics?: PortalDiagnostics;
  maxConcurrent: number;
  maxRetries: number;
  retryBaseMs: number;
  retryMaxMs: number;
  onAdmissionState?: (state: AdmissionDegradedState | undefined) => void;
}

class RequestCoordinator {
  private active = 0;
  private readonly blockedUntil = new Map<RequestClass, number>();
  private readonly blockTimers = new Map<RequestClass, ReturnType<typeof setTimeout>>();
  private readonly degraded = new Set<RequestClass>();
  private readonly probes = new Set<RequestClass>();
  private readonly queue: CoordinatedRequest<unknown>[] = [];

  constructor(private readonly config: RequestCoordinatorConfig) {
    if (!Number.isInteger(config.maxConcurrent) || config.maxConcurrent < 1) {
      throw new RangeError("Maximum concurrent daemon requests must be a positive integer.");
    }
    if (!Number.isInteger(config.maxRetries) || config.maxRetries < 0) {
      throw new RangeError("Maximum admission retries must be a non-negative integer.");
    }
  }

  run<T>(endpoint: string, signal: AbortSignal, attempt: () => Promise<T>): Promise<T> {
    if (signal.aborted) {
      return Promise.reject(new RequestCancelledError());
    }
    return new Promise<T>((resolve, reject) => {
      const request: CoordinatedRequest<T> = {
        endpoint,
        requestClass: requestClassFor(endpoint),
        queuedAt: performance.now(),
        signal,
        attempt,
        resolve,
        reject,
        failures: 0,
        cancelled: false,
        cancel: () => {
          request.cancelled = true;
          signal.removeEventListener("abort", request.cancel);
          reject(new RequestCancelledError());
        },
      };
      signal.addEventListener("abort", request.cancel, { once: true });
      this.queue.push(request as CoordinatedRequest<unknown>);
      this.config.diagnostics?.recordRequestQueue?.({
        endpoint,
        event: "queued",
        inFlight: this.active,
        queueDepth: this.queue.length,
        requestClass: request.requestClass,
      });
      this.drain();
    });
  }

  private drain(): void {
    while (this.active < this.config.maxConcurrent) {
      const index = this.queue.findIndex((candidate) => this.canStart(candidate));
      if (index < 0) return;
      const [request] = this.queue.splice(index, 1);
      if (request.cancelled || request.signal.aborted) continue;
      if (this.degraded.has(request.requestClass)) {
        this.probes.add(request.requestClass);
      }
      this.active += 1;
      this.config.diagnostics?.recordRequestQueue?.({
        endpoint: request.endpoint,
        event: "started",
        inFlight: this.active,
        queueDepth: this.queue.length,
        queueWaitMs: Math.max(0, performance.now() - request.queuedAt),
        requestClass: request.requestClass,
      });
      void this.start(request);
    }
  }

  private canStart(request: CoordinatedRequest<unknown>): boolean {
    if (request.cancelled || request.signal.aborted) {
      return true;
    }
    const deadline = this.blockedUntil.get(request.requestClass) ?? 0;
    if (deadline > Date.now()) {
      this.armBlockTimer(request.requestClass, deadline);
      return false;
    }
    return !this.probes.has(request.requestClass);
  }

  private armBlockTimer(requestClass: RequestClass, deadline: number): void {
    if (this.blockTimers.has(requestClass)) {
      return;
    }
    const timer = setTimeout(() => {
      this.blockTimers.delete(requestClass);
      this.drain();
    }, Math.max(0, deadline - Date.now()));
    this.blockTimers.set(requestClass, timer);
  }

  private async start(request: CoordinatedRequest<unknown>): Promise<void> {
    try {
      const value = await request.attempt();
      if (!request.cancelled) {
        request.signal.removeEventListener("abort", request.cancel);
        request.resolve(value);
        if (this.degraded.delete(request.requestClass)) {
          this.blockedUntil.delete(request.requestClass);
          this.config.onAdmissionState?.(undefined);
        }
      }
    } catch (error) {
      if (!request.cancelled && isAdmissionFailure(error)) {
        request.failures += 1;
        const exponential = Math.min(
          this.config.retryBaseMs * 2 ** Math.max(0, request.failures - 1),
          this.config.retryMaxMs,
        );
        const requiredDelay = Math.max(error.retryAfterMs ?? 0, exponential);
        const delay = requiredDelay + Math.floor(requiredDelay * 0.1 * Math.random());
        const deadline = Math.max(
          this.blockedUntil.get(request.requestClass) ?? 0,
          Date.now() + delay,
        );
        this.blockedUntil.set(request.requestClass, deadline);
        this.degraded.add(request.requestClass);
        this.config.onAdmissionState?.({
          endpoint: request.endpoint,
          retryAt: new Date(deadline).toISOString(),
        });
        this.config.diagnostics?.recordRequestQueue?.({
          endpoint: request.endpoint,
          event: "backoff",
          inFlight: this.active,
          queueDepth: this.queue.length,
          requestClass: request.requestClass,
          retryAt: new Date(deadline).toISOString(),
        });
        if (request.failures > this.config.maxRetries) {
          request.signal.removeEventListener("abort", request.cancel);
          request.reject(error);
        } else {
          this.queue.unshift(request);
        }
      } else if (!request.cancelled) {
        request.signal.removeEventListener("abort", request.cancel);
        request.reject(error);
      }
    } finally {
      this.probes.delete(request.requestClass);
      this.active -= 1;
      this.drain();
    }
  }
}

function requestClassFor(endpoint: string): RequestClass {
  const path = endpoint.split("?", 1)[0].toLowerCase();
  return path.includes("/telemetry/") || path.endsWith("/runs")
    ? "aggregate"
    : "interactive";
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
  const eventType = event.type === "update" ? "invalidate" : event.type;
  if (
    (eventType !== "snapshot" && eventType !== "invalidate") ||
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
    type: eventType,
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
