export type HealthState = "ready" | "degraded" | "offline";
export type RunStatus = "queued" | "running" | "paused" | "succeeded" | "failed" | "cancelled";
export type GateDecisionStatus = "pending" | "approved" | "rejected";

export interface InstanceSummary {
  id: string;
  name: string;
  environment: "dev" | "test" | "prod";
  health: HealthState;
  bootedAt: string;
}

export interface WorkflowStep {
  id: string;
  name: string;
  kind: "task" | "gate";
  gooberId?: string;
  gateKind?: "automated" | "agentic" | "human";
}

export interface WorkflowDefinition {
  id: string;
  name: string;
  trigger: "backlog" | "schedule" | "signal" | "manual";
  status: "enabled" | "paused";
  steps: WorkflowStep[];
}

export interface GooberDefinition {
  id: string;
  name: string;
  role: string;
  status: HealthState;
  scale: number;
  skills: string[];
  activeRunIds: string[];
}

export interface GaggleSummary {
  id: string;
  name: string;
  description: string;
  health: HealthState;
  goobers: GooberDefinition[];
  workflows: WorkflowDefinition[];
}

export interface InstanceSnapshot {
  instance: InstanceSummary;
  gaggles: GaggleSummary[];
}

export interface RunSummary {
  id: string;
  gaggleId: string;
  workflowId: string;
  title: string;
  status: RunStatus;
  startedAt: string;
  endedAt?: string;
  currentStep: string;
  spanCount: number;
}

export interface TraceEvent {
  name: string;
  timestamp: string;
  attributes?: Record<string, string | number | boolean>;
}

export interface TraceSpan {
  id: string;
  parentSpanId?: string;
  name: string;
  kind: "scheduler" | "task" | "gate" | "goober" | "system";
  status: RunStatus;
  startedAt: string;
  endedAt?: string;
  attributes: Record<string, string | number | boolean>;
  events: TraceEvent[];
}

export interface RunTrace {
  run: RunSummary;
  spans: TraceSpan[];
}

export interface GateApprovalRequest {
  id: string;
  runId: string;
  gaggleId: string;
  workflowId: string;
  gateName: string;
  status: GateDecisionStatus;
  requestedBy: string;
  requestedAt: string;
  summary: string;
  approver?: string;
  decidedAt?: string;
  decisionComment?: string;
}

export interface ApprovalDecision {
  decision: "approve" | "reject";
  comment?: string;
}

/**
 * Portal-to-backend contract for the walking skeleton.
 * Initial implementations can satisfy this with static data; the real backend will
 * bind these shapes to Goober-run telemetry and live instance state.
 */
export interface GoobersPortalApi {
  getInstanceSnapshot(): Promise<InstanceSnapshot>;
  listRuns(): Promise<RunSummary[]>;
  getRunTrace(runId: string): Promise<RunTrace | undefined>;
  listGateApprovals(): Promise<GateApprovalRequest[]>;
  decideGateApproval(requestId: string, decision: ApprovalDecision): Promise<GateApprovalRequest>;
}
