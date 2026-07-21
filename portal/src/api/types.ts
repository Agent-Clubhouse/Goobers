export const API_VERSION = "v1";
export const SCHEMA_VERSION = "v1";

export type JsonScalar = string | number | boolean | null;
export type JsonValue = JsonScalar | JsonValue[] | { [key: string]: JsonValue };

export type Environment = "dev" | "staging" | "prod";
export type Provider = "github" | "ado";
export type InstanceStatus = "starting" | "ready" | "degraded";
export type DefinitionStatus = "configured";
export type Harness = "copilot";
export type EvaluatorKind = "automated" | "agentic" | "human";
export type GraphNodeKind = "deterministic" | "agentic" | "gate";
export type GraphTerminal = "complete" | "abort" | "escalate";
export type RunPhase = "running" | "completed" | "failed" | "aborted" | "escalated";
export type RunTriggerKind = "manual" | "schedule" | "signal" | "item";
export type AttemptClass = "initial" | "policy" | "infra";
export type StageAttemptStatus = "running" | "success" | "failure" | "blocked" | "no-work";
export type ValidationSeverity = "error" | "warning";
export type ValidationWarningCode = "VER001" | "VER002" | "VER003" | "MODEL002";
export type UpdateModel = "instance" | "run" | "workflow";

export interface RequestOptions {
  signal?: AbortSignal;
}

export interface EventStreamRequest {
  cursor?: string;
}

export interface WorkflowUpdateReference {
  gaggle: string;
  name: string;
}

export interface ModelInvalidation {
  cursor: string;
  models: UpdateModel[];
  runIds?: string[];
  workflows?: WorkflowUpdateReference[];
}

export type DaemonUpdateEvent =
  | {
      id: string;
      type: "snapshot" | "invalidate";
      data: ModelInvalidation;
    }
  | {
      type: "heartbeat";
      data: { cursor: string };
    };

export interface DaemonEventStream extends AsyncIterable<DaemonUpdateEvent> {
  close(): void;
}

export interface PageRequest {
  limit?: number;
  cursor?: string;
}

export interface PageInfo {
  limit: number;
  total: number;
  hasMore: boolean;
  nextCursor: string;
}

export interface ApiError {
  code: string;
  message: string;
}

export interface ApiErrorEnvelope {
  error: ApiError;
}

export interface ContractVersion {
  apiVersion: typeof API_VERSION;
  schemaVersion: typeof SCHEMA_VERSION;
}

export interface Health extends ContractVersion {
  ready: boolean;
  instance: InstanceIdentity;
  freshness: Freshness;
}

export interface InstanceIdentity {
  name: string;
  environment: Environment;
}

export interface Freshness {
  observedAt: string;
  definitionsLoadedAt: string;
  journalUpdatedAt: string | null;
}

export interface Instance extends ContractVersion {
  name: string;
  environment: Environment;
  ready: boolean;
  status: InstanceStatus;
  concurrency: Concurrency;
  counts: InventoryCounts;
  warnings: ValidationWarning[];
}

export interface Concurrency {
  activeRuns: number;
  maxConcurrentRuns: number;
}

export interface InventoryCounts {
  gaggles: number;
  goobers: number;
  workflows: number;
  activeRuns: number;
}

export interface ValidationWarning {
  code: ValidationWarningCode;
  severity: ValidationSeverity;
  scope: string;
  explanation: string;
}

export interface RepoRef {
  provider: Provider;
  owner: string;
  name: string;
  branch?: string;
  connectionRef?: string;
}

export interface BacklogRef {
  provider: Provider;
  project: string;
  labels?: string[];
  query?: string;
  connectionRef?: string;
}

export interface Gaggle {
  name: string;
  displayName: string;
  status: DefinitionStatus;
  project: RepoRef;
  backlog: BacklogRef;
  gooberCount: number;
  workflowCount: number;
  activeRunCount: number;
  warnings: ValidationWarning[];
}

export interface GagglePage {
  items: Gaggle[];
  page: PageInfo;
}

export interface WorkflowReference {
  gaggle: string;
  name: string;
}

export interface GooberReference {
  gaggle: string;
  name: string;
}

export interface StageOwnership {
  workflow: WorkflowReference;
  stage: string;
  kind: GraphNodeKind;
}

export interface Goober {
  name: string;
  displayName: string;
  role: string;
  status: DefinitionStatus;
  harness: Harness;
  skills: string[];
  capabilities: string[];
  workflows: WorkflowReference[];
  stages: StageOwnership[];
  warnings: ValidationWarning[];
}

export interface GooberPage {
  items: Goober[];
  page: PageInfo;
}

export type WorkflowTriggerType = "manual" | "backlog-item" | "schedule" | "signal" | "webhook";

export interface WorkflowTrigger {
  type: WorkflowTriggerType;
  selector?: Record<string, string>;
  schedule?: string;
  signal?: string;
  events?: string[];
}

export interface ReadinessConditions {
  maxConcurrentRuns?: number;
  maxRunsPerHour?: number;
  maxRunsPerDay?: number;
  maxChainDepth?: number;
  maxOpenPRs?: number;
}

export interface WorkflowDefinition {
  version: number;
  digest: string;
}

export interface WorkflowConcurrency {
  activeRuns: number;
  maxConcurrentRuns: number;
}

export interface WorkflowSummary {
  identity: WorkflowReference;
  displayName: string;
  purpose: string;
  triggers: WorkflowTrigger[];
  readiness: ReadinessConditions;
  concurrency: WorkflowConcurrency;
  owners: GooberReference[];
  stageCount: number;
  definition: WorkflowDefinition;
  warnings: ValidationWarning[];
}

export interface WorkflowPage {
  items: WorkflowSummary[];
  page: PageInfo;
}

export interface WorkflowGraph {
  name: string;
  version: number;
  digest: string;
  start: string;
  nodes: WorkflowGraphNode[];
  edges: WorkflowGraphEdge[];
}

export interface WorkflowGraphNode {
  id: string;
  kind: GraphNodeKind;
  owner?: string;
  evaluator?: EvaluatorKind;
}

export interface WorkflowGraphEdge {
  source: string;
  target: string;
  outcome?: string;
  terminal?: GraphTerminal;
}

export interface StageDefinition {
  name: string;
  kind: GraphNodeKind;
  goal: string;
  owner: GooberReference | null;
  evaluator: EvaluatorKind | "";
  capabilities: string[];
}

export interface WorkflowDetail extends WorkflowSummary {
  graph: WorkflowGraph;
  stages: StageDefinition[];
}

export interface RunTrigger {
  kind: RunTriggerKind;
  ref?: string;
}

export interface RunListOptions {
  gaggle?: string;
  workflow?: string;
  phase?: RunPhase;
  trigger?: RunTriggerKind;
  limit?: number;
  cursor?: string;
}

export interface RunList {
  runs: RunSummary[];
  nextCursor?: string;
}

export interface RunSummary {
  id: string;
  workflow: string;
  workflowVersion: number;
  workflowDigest?: string;
  gaggle: string;
  trigger: RunTrigger;
  phase: RunPhase;
  terminal: boolean;
  currentStage?: string;
  startedAt: string;
  finishedAt?: string;
  durationMillis: number;
  lastActivityAt: string;
  lastSeq: number;
  repassCount: number;
  retryCount: number;
  policyRetryCount: number;
  infraRetryCount: number;
}

export interface RunDetail extends RunSummary {
  graph?: WorkflowGraph;
  graphStatus: "pinned" | "unavailable";
  escalation?: EscalationCause;
}

export interface EscalationCause {
  selector: EscalationSelector;
  selectedBranch?: string;
  repassCount: number;
  retryCount: number;
  terminalReason?: string;
  causalEventSeq?: number;
}

export interface EscalationSelector {
  kind: string;
  name: string;
}

export type KnownRunEventType =
  | "run.started"
  | "run.finished"
  | "stage.started"
  | "stage.finished"
  | "gate.started"
  | "gate.evaluated"
  | "artifact.recorded"
  | "span.recorded"
  | "input.snapshot"
  | "ref.touched"
  | "error"
  | "redaction"
  | "repaired"
  | "runner.annotation"
  | "trigger.fired"
  | "tick.skipped"
  | "claim.acquired"
  | "claim.released"
  | "config.reloaded"
  | "config.reload.rejected";

export type RunEventType = KnownRunEventType | (string & Record<never, never>);

export interface EventList {
  runId: string;
  events: RunEvent[];
}

export interface RunEvent {
  schema: string;
  seq: number;
  type: RunEventType;
  branch: number;
  time: string;
  knownSchema: boolean;
  stage?: string;
  attempt?: number;
  attemptClass?: AttemptClass;
  gate?: string;
  verdict?: string;
  target?: string;
  escalated?: boolean;
  status?: RunPhase | StageAttemptStatus;
  outputs?: Record<string, JsonValue>;
  artifacts?: ArtifactMetadata[];
  artifact?: ArtifactMetadata;
  name?: string;
  externalRef?: ExternalRef;
  error?: ErrorDetail;
  redaction?: RedactionInfo;
  runner?: Record<string, JsonValue>;
  workflow?: string;
  runId?: string;
  reason?: string;
  raw?: JsonValue;
}

export interface ExternalRef {
  provider: string;
  kind: string;
  id: string;
  url?: string;
}

export interface ErrorDetail {
  code: string;
  message?: string;
}

export interface RedactionInfo {
  target: string;
  oldDigest: string;
  newDigest: string;
  reason?: string;
}

export interface ArtifactMetadata {
  name?: string;
  digest: string;
  size: number;
  mediaType: string;
  stage?: string;
  attempt?: number;
  attemptClass?: AttemptClass;
  recordedSeq?: number;
}

export interface AttemptList {
  runId: string;
  stage: string;
  attempts: StageAttempt[];
}

export interface StageAttempt {
  number: number;
  class: AttemptClass;
  status: StageAttemptStatus | "";
  startedSeq?: number;
  finishedSeq?: number;
  startedAt?: string;
  finishedAt?: string;
  durationMillis: number;
  outputs?: Record<string, JsonValue>;
  artifacts: ArtifactMetadata[];
  error?: ErrorDetail;
}

export interface ArtifactContent {
  digest: string;
  mediaType: string;
  size: number;
  etag: string | null;
  bytes: ArrayBuffer;
}

export interface TelemetryStatsOptions {
  workflow?: string;
  gaggle?: string;
  since?: string;
  until?: string;
}

export interface TelemetryStatsResult {
  runs: TelemetryRunStats[];
  stages: TelemetryStageStats[];
}

export interface TelemetryRunStats {
  workflow: string;
  totalRuns: number;
  completedRuns: number;
  failedRuns: number;
  otherRuns: number;
  successRate?: number;
  avgDurationMs?: number;
  minDurationMs?: number;
  maxDurationMs?: number;
}

export interface TelemetryStageStats {
  stage: string;
  totalAttempts: number;
  succeededAttempts: number;
  failedAttempts: number;
  successRate?: number;
  avgDurationMs?: number;
  minDurationMs?: number;
  maxDurationMs?: number;
}

export interface TelemetryErrorsOptions extends TelemetryStatsOptions {
  errorClass?: string;
  limit?: number;
  cursor?: string;
}

export interface TelemetryErrorsPage {
  items: TelemetryError[];
  nextCursor?: string;
}

export interface TelemetryError {
  runId: string;
  workflow: string;
  stage: string;
  attempt: number;
  code: string;
  errorClass: string;
  message: string;
  occurredAt: string;
}

export interface DaemonClient {
  connectEvents(
    request?: EventStreamRequest,
    options?: RequestOptions,
  ): Promise<DaemonEventStream>;
  getHealth(options?: RequestOptions): Promise<Health>;
  getInstance(options?: RequestOptions): Promise<Instance>;
  listGaggles(request?: PageRequest, options?: RequestOptions): Promise<GagglePage>;
  listGoobers(gaggle: string, request?: PageRequest, options?: RequestOptions): Promise<GooberPage>;
  listWorkflows(gaggle: string, request?: PageRequest, options?: RequestOptions): Promise<WorkflowPage>;
  getWorkflow(gaggle: string, workflow: string, options?: RequestOptions): Promise<WorkflowDetail>;
  listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList>;
  getRun(runId: string, options?: RequestOptions): Promise<RunDetail>;
  listRunEvents(runId: string, options?: RequestOptions): Promise<EventList>;
  listStageAttempts(runId: string, stage: string, options?: RequestOptions): Promise<AttemptList>;
  getArtifact(runId: string, digest: string, options?: RequestOptions): Promise<ArtifactContent>;
  getTelemetryStats(
    request?: TelemetryStatsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryStatsResult>;
  listTelemetryErrors(
    request?: TelemetryErrorsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorsPage>;
}
