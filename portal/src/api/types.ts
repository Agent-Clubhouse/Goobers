import type { ConfigAuthoringErrorCode } from "./contract.generated";

export const API_VERSION = "v1";
export const SCHEMA_VERSION = "v1";

export type JsonScalar = string | number | boolean | null;
export type JsonValue = JsonScalar | JsonValue[] | { [key: string]: JsonValue };

export type Environment = "dev" | "staging" | "prod";
export type Provider = "github" | "ado";
export type InstanceStatus = "starting" | "ready" | "degraded";
export type DefinitionStatus = "configured";
export type Harness = "copilot" | "claude-code";
export type EvaluatorKind = "automated" | "agentic" | "human";
export type GraphNodeKind = "deterministic" | "agentic" | "gate" | "parallel";
export type BranchStatus = "succeeded" | "failed" | "timed-out" | "cancelled" | "no-output";
export type GraphTerminal = "complete" | "abort" | "escalate";
export type RunPhase = "running" | "completed" | "failed" | "aborted" | "escalated";
export type RunTriggerKind = "manual" | "schedule" | "signal" | "item";
export type AttemptClass = "initial" | "policy" | "infra" | "human";
export type StageAttemptStatus = "running" | "success" | "failure" | "blocked" | "no-work";
export type OutcomeFilter = "finished" | "terminal" | "success" | "failure" | "other";
export type StagePopulationFilter =
  | "attempts"
  | "measured"
  | "token-measured"
  | "premium-measured"
  | "cost-measured"
  | "retry-waste";
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

export const CONFIG_AUTHORING_SCHEMA_VERSION = "v1alpha1";

export interface ConfigAuthoringContractVersion {
  apiVersion: typeof API_VERSION;
  schemaVersion: typeof CONFIG_AUTHORING_SCHEMA_VERSION;
}

export type ConfigSourceKind = "local" | "git" | "provider";

export interface ConfigSourceCapabilities {
  read: boolean;
  validate: boolean;
  directWrite: boolean;
  reviewWrite: boolean;
}

export interface ConfigSourceDescriptor {
  id: string;
  displayName: string;
  kind: ConfigSourceKind;
  revision: string;
  capabilities: ConfigSourceCapabilities;
}

export interface ConfigSourcePage extends ConfigAuthoringContractVersion {
  items: ConfigSourceDescriptor[];
}

export type ConfigDocumentKind =
  | "manifest"
  | "instance"
  | "gaggle"
  | "workflow"
  | "goober"
  | "support";

export interface ConfigDefinitionReference {
  kind: ConfigDocumentKind;
  name: string;
  gaggle?: string;
}

export interface ConfigDocumentDescriptor {
  path: string;
  mediaType: string;
  etag: string;
  editable: boolean;
  definition?: ConfigDefinitionReference;
}

export interface ConfigDocumentPage extends ConfigAuthoringContractVersion {
  sourceId: string;
  revision: string;
  items: ConfigDocumentDescriptor[];
}

export interface ConfigDocumentRequest {
  path: string;
}

export interface ConfigDocument extends ConfigAuthoringContractVersion {
  sourceId: string;
  revision: string;
  document: ConfigDocumentDescriptor;
  content: string;
}

export type ConfigDocumentChange =
  | {
      path: string;
      operation: "upsert";
      baseEtag?: string;
      content: string;
    }
  | {
      path: string;
      operation: "delete";
      baseEtag: string;
      content?: never;
    };

export interface ConfigChangeSet {
  baseRevision: string;
  changes: ConfigDocumentChange[];
}

export type ConfigDiagnosticSeverity = "error" | "warning";

export interface ConfigDiagnosticLocation {
  path: string;
  line?: number;
  column?: number;
  endLine?: number;
  endColumn?: number;
}

export interface ConfigDiagnostic {
  code: string;
  severity: ConfigDiagnosticSeverity;
  message: string;
  scope?: string;
  location?: ConfigDiagnosticLocation;
}

export interface ConfigDiff {
  format: string;
  content: string;
  truncated: boolean;
}

export interface ConfigChangePreviewRequest {
  changeSet: ConfigChangeSet;
}

export interface ConfigChangePreview extends ConfigAuthoringContractVersion {
  sourceId: string;
  baseRevision: string;
  previewId: string;
  eligible: boolean;
  diagnostics: ConfigDiagnostic[];
  diff: ConfigDiff;
}

export type ConfigWriteStrategy = "direct" | "review";

export interface ConfigWriteRequest {
  previewId: string;
  changeSet: ConfigChangeSet;
  strategy: ConfigWriteStrategy;
  summary?: string;
}

export interface ConfigReviewReference {
  id: string;
  url: string;
  branch?: string;
  commit?: string;
}

export interface ConfigWriteOutcome extends ConfigAuthoringContractVersion {
  sourceId: string;
  baseRevision: string;
  revision?: string;
  strategy: ConfigWriteStrategy;
  changedDocuments: string[];
  review?: ConfigReviewReference;
  sourceApplied?: string;
}

export interface ConfigAuthoringApiError {
  code: ConfigAuthoringErrorCode;
  message: string;
}

export interface ConfigAuthoringErrorEnvelope {
  error: ConfigAuthoringApiError;
}

export interface Health extends ContractVersion {
  ready: boolean;
  healthy: boolean;
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
  lastSchedulerTickAt: string | null;
  lastTickAgeMillis: number | null;
}

export interface Instance extends ContractVersion {
  name: string;
  environment: Environment;
  ready: boolean;
  status: InstanceStatus;
  concurrency: Concurrency;
  counts: InventoryCounts;
  warnings: ValidationWarning[];
  memoryHighWater?: number;
  memoryGateEnabled: boolean;
  fsyncDisabled: boolean;
  fleetEnrolled: boolean;
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

export type RepositoryAccessMode = "read-write" | "read-only";

export interface RepositoryIdentity {
  provider: Provider;
  owner: string;
  project?: string;
  name: string;
}

export interface RepositoryConnection {
  repository: RepositoryIdentity;
  accessMode: RepositoryAccessMode;
}

export interface GaggleConnections {
  gaggle: string;
  repositories: RepositoryConnection[];
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
  desiredConcurrentRuns?: number;
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
  desiredRuns?: number;
  maxConcurrentRuns: number;
  admissionBlocked?: boolean;
  blockingCondition?: string;
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
  /** The declared parallel branch this edge belongs to; empty on ordinary sequential edges. */
  branch?: string;
}

export interface RetryPolicy {
  maxAttempts: number;
  backoffSeconds?: number;
}

export interface StageDefinition {
  name: string;
  kind: GraphNodeKind;
  goal: string;
  owner: GooberReference | null;
  evaluator: EvaluatorKind | "";
  capabilities: string[];
  timeoutSeconds?: number;
  retry?: RetryPolicy | null;
  policyActions?: string[];
  onTimeout?: string;
  requiredCapabilities?: string[];
  branches?: Record<string, string>;
  maxRepasses?: number;
  /** The stage's Task/Gate config as actually loaded, marshaled back to YAML — ground truth for values like timeout (#2185). */
  rawYaml: string;
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
  stage?: string;
  outcome?: OutcomeFilter;
  population?: StagePopulationFilter;
  phase?: RunPhase;
  trigger?: RunTriggerKind;
  since?: string;
  until?: string;
  limit?: number;
  cursor?: string;
  latestPerWorkflow?: boolean;
  /**
   * Filters and orders by the run's last journal activity instead of its
   * start (#1777). `since`/`until` bound last activity on this axis, which is
   * what makes "runs active in the last N hours" expressible. Requires the
   * read model — an instance without one refuses rather than silently
   * ordering by start.
   */
  orderByActivity?: boolean;
  /** Includes routine no-work schedule ticks (#2188); omitted/false hides them. */
  showNoWork?: boolean;
}

export interface RunList {
  runs: RunSummary[];
  workflowActivity?: WorkflowRunActivity[];
  nextCursor?: string;
}

export interface WorkflowRunActivity {
  gaggle: string;
  workflow: string;
  activeRuns: number;
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
  /** Running run whose activity and daemon heartbeat both exceed runner.livenessTimeout. */
  stale: boolean;
  lastSeq: number;
  repassCount: number;
  retryCount: number;
  policyRetryCount: number;
  infraRetryCount: number;
  /** True for a completed run that touched exactly one stage and that stage's terminal status was no-work (#2188). */
  noWork: boolean;
  /** Projected cause of a non-completed terminal run — failed and aborted as well as escalated (#4246). */
  terminalReason?: string;
  operator?: OperatorRunSummary;
}

export interface OperatorRunSummary {
  issue?: { number: string; title?: string };
  currentStage?: string;
  lastHeartbeatAt?: string;
  heartbeatAgeMillis?: number;
  liveness: string;
  trajectory: string;
  pullRequest?: { provider: string; kind: string; id: string; url?: string };
  prOpenerStage?: string;
  claim: {
    leaseStatus: string;
    expiresAt?: string;
    providerMarker: string;
  };
  latestError?: { code: string; message?: string };
  review?: { verdict: string; rationale?: string };
  nextTransition?: string;
  /** Things impeding the RUN itself. Never a read-side capability gap (#3346). */
  potentialBlockers: string[];
  /** What the read invocation could not establish (missing credential, unreachable provider) — a limit on the reader, not on the run (#3346). */
  diagnosticsLimitations?: string[];
}

export interface RunDetail extends RunSummary {
  graph?: WorkflowGraph;
  graphStatus: "pinned" | "unavailable";
  escalation?: EscalationCause;
  /** The same cause projection as escalation, present for every non-completed terminal phase (#4246). */
  terminalCause?: EscalationCause;
  /** The business decision a completed run reached, distinct from phase (the execution axis). */
  outcome?: RunOutcome;
  /** The run's exact executed workflow-graph transition history — never inferred from "both endpoint nodes were visited". */
  transitions?: RunTransition[];
  transitionsStatus: "projected" | "unavailable";
}

/** One executed transition in a run's workflow graph (source -> target), including terminal and repass edges. */
export interface RunTransition {
  branch: number;
  occurrence: number;
  seq: number;
  source: string;
  target?: string;
  verdict?: string;
  terminal?: boolean;
  status?: string;
  repass?: boolean;
}

/** Present only when phase is "completed"; all-empty when no gate decided the completion. */
export interface RunOutcome {
  gate?: string;
  verdict?: string;
  target?: string;
  causalEventSeq?: number;
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
  | "run.resumed"
  | "run.finished"
  | "stage.started"
  | "stage.heartbeat"
  | "stage.finished"
  | "stage.rerun.requested"
  | "gate.started"
  | "gate.paused"
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
  | "workflow.starved"
  | "provider.quota.reset"
  | "poll.shed"
  | "claim.acquired"
  | "claim.released"
  | "claim.force_released"
  | "claim_lock_slow"
  | "claims_lock_timeout"
  | "config.reloaded"
  | "config.reload.rejected"
  | "daemon.started"
  | "daemon.clean_shutdown"
  | "daemon.dirty_restart"
  | "parallel.started"
  | "parallel.finished"
  | "branch.started"
  | "branch.finished";

export type RunEventType = KnownRunEventType | (string & Record<never, never>);

export type RunEventCategory =
  | "transition"
  | "decision"
  | "result"
  | "evidence"
  | "liveness"
  | "bookkeeping"
  | "unknown";

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
  category?: RunEventCategory;
  replayChapter?: boolean;
  stage?: string;
  attempt?: number;
  attemptClass?: AttemptClass;
  gate?: string;
  verdict?: string;
  target?: string;
  complete?: boolean;
  escalated?: boolean;
  status?: RunPhase | StageAttemptStatus;
  actor?: string;
  action?: string;
  decision?: string;
  rationale?: string;
  instructionAddendum?: string;
  workflowVersion?: number;
  workflowDigest?: string;
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
  /** The parallel state this event concerns; set on parallel.started/finished and branch.started/finished. */
  parallel?: string;
  /** The declared branch name; set on branch.started/branch.finished. */
  branchName?: string;
  /** The branch's terminal status; set on branch.finished. */
  branchStatus?: BranchStatus;
  /** The branch completeness record; set on parallel.finished, one entry per declared branch. */
  completeness?: BranchOutcome[];
  raw?: JsonValue;
}

/** One entry in a parallel's completeness record (one per declared branch). */
export interface BranchOutcome {
  branch: number;
  name: string;
  status: BranchStatus;
  artifacts: number;
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

/**
 * Placement provenance journaled under runner.* for one stage attempt:
 * where it physically executed, as far as the executing substrate knew.
 * Every field except runner is optional — a local attempt has no pod and
 * never queued.
 */
export interface AttemptPlacement {
  /** Runners-inventory entry name; "self" for the daemon's own host. */
  runner: string;
  /** Cluster node the attempt ran on — only ever a real node, never a hostname. */
  node?: string;
  /** The executing process's own hostname; inside a pod this is the pod name. */
  host?: string;
  /** GOOS of the executing substrate. */
  os?: string;
  /** Container image reference the attempt ran under. */
  image?: string;
  /** Pod identity for containerized attempts. */
  pod?: string;
  /** When the attempt entered the dispatch fabric. */
  queuedAt?: string;
  /** When the attempt's pod began executing. */
  podStartedAt?: string;
}

export interface StageAttempt {
  id: string;
  visit: number;
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
  /** Requested/selected model (e.g. "auto"), when the telemetry rollup has indexed it. */
  model?: string;
  /** runner.* placement provenance; absent for journals recorded before it existed. */
  placement?: AttemptPlacement;
}

export interface ArtifactContent {
  digest: string;
  mediaType: string;
  size: number;
  etag: string | null;
  bytes: ArrayBuffer;
}

export interface TranscriptContent {
  seq: number;
  stage: string;
  name: string;
  size: number;
  bytes: ArrayBuffer;
}

export interface TelemetryStatsOptions {
  workflow?: string;
  gaggle?: string;
  since?: string;
  until?: string;
  trendSince?: string;
  trendUntil?: string;
  trendBuckets?: number;
  trendPreviousSince?: string;
  trendPreviousUntil?: string;
}

export interface TelemetryTrendBucket {
  since: string;
  until: string;
  usage: TelemetryUsageStats[];
}

export interface TelemetryStatsResult {
  gaggles: TelemetryGaggleStats[];
  runs: TelemetryRunStats[];
  stages: TelemetryStageStats[];
  usage: TelemetryUsageStats[];
  models: TelemetryModelStats[];
  creditAssignment: NodeCredit[];
  causalCredit: CausalNodeCredit[] | null;
  graphAnalytics?: GraphAnalytics;
  promotionSignals?: PromotionSignal[];
  promotionCandidates?: PromotionSignal[];
  curation: TelemetryCurationStats;
  readyPool: TelemetryReadyPool;
  trend?: TelemetryTrendBucket[];
  trendPrevious?: TelemetryTrendBucket;
}

export interface GraphAnalytics {
  centrality: CentralityScore[];
  criticalPath: CriticalPath;
  cycles: string[][];
  confidence: "bounded" | "partial" | "untrusted" | string;
  caveat?: string;
}

export interface CentralityScore {
  node: string;
  score: number;
}

export interface CriticalPath {
  nodes: string[];
  weight: number;
}

export interface NodeCredit {
  gaggle: string;
  workflow: string;
  kind: "stage" | "gate";
  stage: string;
  identity?: string;
  routedRuns: number;
  failureRuns: number;
  failureShare: number;
  escalationRuns: number;
  retryWasteAttempts: number;
  effect?: number;
  lower?: number;
  upper?: number;
  identification: string;
  caveat?: string;
}

export interface CausalNodeCredit {
  node: string;
  effect: number;
  lower: number;
  upper: number;
  identification:
    | "randomized"
    | "observational-difference-in-differences"
    | "unidentifiable";
  caveat: string;
  treatedBefore: number;
  treatedAfter: number;
  controlBefore: number;
  controlAfter: number;
  intervalAvailable: boolean;
  promotionEligible: boolean;
  promotionSource: string;
}

export interface PromotionSignal {
  node: string;
  value: number;
  lower?: number;
  upper?: number;
  source: string;
  caveat: string;
  promotionEligible: boolean;
}

export interface TelemetryCurationStats {
  everRecorded: boolean;
  runs: number;
  reportedRuns: number;
  ready: number;
  needsHuman: number;
  closed: number;
  deduped: number;
  split: number;
  stale: number;
  reconciled: number;
  milestoned: number;
  bounced: number;
}

export interface TelemetryReadyPool {
  sampleEverRecorded: boolean;
  observedAt?: string;
  depth?: number;
  averageAgeSeconds?: number;
  oldestAgeSeconds?: number;
  starved?: boolean;
  claimAgeSamples: number;
  averageClaimAgeSeconds?: number;
  bounceEverRecorded: boolean;
  bounceRate?: number;
  inFlightClaimSamples: number;
  averageInFlightClaimAgeSeconds: number;
  oldestInFlightClaimAgeSeconds: number;
  forwardCurationThroughput: number;
  implementationDemand: number;
}

export interface TelemetryGaggleStats {
  gaggle: string;
  totalRuns: number;
  completedRuns: number;
  failedRuns: number;
  // How many of failedRuns terminated on an infrastructure fault rather than a
  // verdict about the work, and are therefore excluded from successRate's
  // denominator (#3361/#3364).
  infraFailedRuns: number;
  otherRuns: number;
  successRate?: number;
  avgDurationMs?: number;
  minDurationMs?: number;
  maxDurationMs?: number;
}

export interface TelemetryRunStats {
  gaggle: string;
  workflow: string;
  totalRuns: number;
  completedRuns: number;
  failedRuns: number;
  otherRuns: number;
  successRate?: number;
  avgDurationMs?: number;
  minDurationMs?: number;
  maxDurationMs?: number;
  // How many of failedRuns terminated on an infrastructure fault (credential
  // materialization, git, network, lock contention) rather than a verdict
  // about the work, and are therefore excluded from successRate's denominator
  // (#3361/#3364).
  infraFailedRuns: number;
  // How many of totalRuns hung and were later aborted (the watchdog's
  // max-duration expiry), excluded from avg/min/maxDurationMs — disclosed
  // rather than silently dropped (#2534, #1439).
  stuckAbortedRuns: number;
}

export interface TelemetryStageStats {
  gaggle: string;
  workflow: string;
  stage: string;
  branch?: number;
  totalAttempts: number;
  succeededAttempts: number;
  failedAttempts: number;
  successRate?: number;
  avgDurationMs?: number;
  minDurationMs?: number;
  maxDurationMs?: number;
  durationSamples: number;
  p50DurationMs?: number;
  p95DurationMs?: number;
  tokenSamples: number;
  p50Tokens?: number;
  p95Tokens?: number;
  costSamples: number;
  p50CostUSD?: number;
  p95CostUSD?: number;
  retryWasteAttempts: number;
  retryWasteDurationMs?: number;
  retryWasteTokens?: number;
  retryWasteCostUSD?: number;
  // How many of totalAttempts belong to a run that hung and was later
  // aborted (the watchdog's max-duration expiry), excluded from
  // avg/min/maxDurationMs and from p50/p95DurationMs — disclosed rather than
  // silently dropped (#2534, #1439).
  stuckAbortedAttempts: number;
}

export interface TelemetryUsageStats {
  scope: "instance" | "gaggle" | "workflow" | "stage";
  gaggle?: string;
  workflow?: string;
  stage?: string;
  branch?: number;
  totalAttempts: number;
  tokenSamples: number;
  p50Tokens?: number;
  p95Tokens?: number;
  premiumRequestSamples: number;
  p50CopilotPremiumRequests?: number;
  p95CopilotPremiumRequests?: number;
  costSamples: number;
  costUSD?: number;
  p50CostUSD?: number;
  p95CostUSD?: number;
  retryWasteAttempts: number;
  retryWasteTokens?: number;
  retryWasteCostUSD?: number;
}

export interface TelemetryModelStats {
  model: string;
  usageSamples: number;
  inputTokenSamples: number;
  inputTokens?: number;
  outputTokenSamples: number;
  outputTokens?: number;
  premiumRequestSamples: number;
  copilotPremiumRequests?: number;
  costSamples: number;
  costUSD?: number;
}

export interface TelemetryErrorSignaturesOptions extends TelemetryStatsOptions {
  stage?: string;
  limit?: number;
}

export interface TelemetryErrorSignaturesResult {
  items: TelemetryErrorSignature[];
}

export interface TelemetryErrorSignature {
  code: string;
  errorClass: string;
  count: number;
  lastSeen: string;
  exampleRunId?: string;
  exampleStage?: string;
  exampleAttempt?: number;
}

export interface TelemetryErrorsOptions extends TelemetryStatsOptions {
  stage?: string;
  code?: string;
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

export interface PortalBrand {
  name: string;
  tagline: string;
  scopeMark: string;
  logoUrl: string | null;
  faviconUrl: string | null;
}

export interface PortalTheme {
  accentLight: string | null;
  accentDark: string | null;
  accentSoftLight: string | null;
  accentSoftDark: string | null;
  accentInkLight: string | null;
  accentInkDark: string | null;
}

export interface PortalSupportLink {
  label: string;
  url: string;
}

export interface PortalSupport {
  docsUrl: string | null;
  issuesUrl: string | null;
  chatUrl: string | null;
  links: PortalSupportLink[];
}

export interface PortalConfig {
  brand: PortalBrand;
  theme: PortalTheme;
  support: PortalSupport;
  capabilities: {
    revealRun: boolean;
    workflowEnable: boolean;
  };
}

export interface DaemonClient {
  connectEvents(
    request?: EventStreamRequest,
    options?: RequestOptions,
  ): Promise<DaemonEventStream>;
  getHealth(options?: RequestOptions): Promise<Health>;
  getInstance(options?: RequestOptions): Promise<Instance>;
  getPortalConfig(options?: RequestOptions): Promise<PortalConfig>;
  listGaggles(request?: PageRequest, options?: RequestOptions): Promise<GagglePage>;
  listGoobers(gaggle: string, request?: PageRequest, options?: RequestOptions): Promise<GooberPage>;
  listWorkflows(gaggle: string, request?: PageRequest, options?: RequestOptions): Promise<WorkflowPage>;
  getGaggleConnections(gaggle: string, options?: RequestOptions): Promise<GaggleConnections>;
  getWorkflow(gaggle: string, workflow: string, options?: RequestOptions): Promise<WorkflowDetail>;
  listRuns(request?: RunListOptions, options?: RequestOptions): Promise<RunList>;
  getRun(runId: string, options?: RequestOptions): Promise<RunDetail>;
  revealRun(runId: string, options?: RequestOptions): Promise<void>;
  listRunEvents(runId: string, options?: RequestOptions): Promise<EventList>;
  listStageAttempts(runId: string, stage: string, options?: RequestOptions): Promise<AttemptList>;
  getArtifact(runId: string, digest: string, options?: RequestOptions): Promise<ArtifactContent>;
  getTranscript(runId: string, seq: number, options?: RequestOptions): Promise<TranscriptContent>;
  getTelemetryStats(
    request?: TelemetryStatsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryStatsResult>;
  getTelemetryErrorSignatures(
    request?: TelemetryErrorSignaturesOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorSignaturesResult>;
  listTelemetryErrors(
    request?: TelemetryErrorsOptions,
    options?: RequestOptions,
  ): Promise<TelemetryErrorsPage>;
}

/**
 * How current the data in a response is (#1927, design §7.2).
 *
 * Distinct from connection state. "The socket is open" and "the data is fresh"
 * are different facts, and conflating them is why an operator cannot tell slow
 * from broken (#1928).
 *
 * `lagSeconds` is an honest upper bound, not a measurement: the journal append
 * and the intake write are in different files and cannot be atomic, so the
 * server reports `max(oldest pending watermark age, time since the last
 * completed repair sweep)`.
 */
export interface ReadState {
  epoch: string;
  appliedSeq: number;
  sourceApplied?: { runId: string; journalSeq: number };
  observedAt: string;
  lagSeconds: number;
  pendingIntake: number;
  oldestPendingSourceAge: number;
  intakeWriteFailures: number;
  lastSweepCompletedAt?: string;
  minChangeSeq: number;
  completeness: "complete" | "partial";
  missing?: { name: string; reason: string; expectedBy: string }[];
  degraded: string[];
}

/** Every read response carries one, when served from the read model. */
export interface WithReadState {
  readState?: ReadState;
}
