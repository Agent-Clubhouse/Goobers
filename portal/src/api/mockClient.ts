import type {
  ApprovalDecision,
  GateApprovalRequest,
  GoobersPortalApi,
  InstanceSnapshot,
  RunSummary,
  RunTrace,
} from "./types";

const iso = (offsetMinutes = 0) => new Date(Date.UTC(2026, 5, 28, 19, 0 + offsetMinutes, 0)).toISOString();

export const emptyInstanceSnapshot: InstanceSnapshot = {
  instance: {
    id: "local-dev",
    name: "Goobers local instance",
    environment: "dev",
    health: "ready",
    bootedAt: iso(),
  },
  gaggles: [],
};

export const sampleInstanceSnapshot: InstanceSnapshot = {
  instance: emptyInstanceSnapshot.instance,
  gaggles: [
    {
      id: "engineering",
      name: "Engineering gaggle",
      description: "Coding workforce for product backlog delivery.",
      health: "ready",
      goobers: [
        {
          id: "coder-1",
          name: "Coder Goober",
          role: "Implementation agent",
          status: "ready",
          scale: 2,
          skills: ["typescript", "go", "tests"],
          activeRunIds: ["run-101"],
        },
        {
          id: "qa-1",
          name: "QA Goober",
          role: "Quality gate reviewer",
          status: "ready",
          scale: 1,
          skills: ["acceptance", "regression"],
          activeRunIds: [],
        },
      ],
      workflows: [
        {
          id: "default-implementation",
          name: "Default implementation workflow",
          trigger: "backlog",
          status: "enabled",
          steps: [
            { id: "plan", name: "Plan work", kind: "task", gooberId: "coder-1" },
            { id: "implement", name: "Implement change", kind: "task", gooberId: "coder-1" },
            { id: "qa-human", name: "QA approval", kind: "gate", gateKind: "human" },
          ],
        },
      ],
    },
  ],
};

export const sampleRuns: RunSummary[] = [
  {
    id: "run-101",
    gaggleId: "engineering",
    workflowId: "default-implementation",
    title: "Implement portal walking skeleton",
    status: "paused",
    startedAt: iso(2),
    currentStep: "QA approval",
    spanCount: 4,
  },
  {
    id: "run-100",
    gaggleId: "engineering",
    workflowId: "default-implementation",
    title: "Add provider abstraction",
    status: "succeeded",
    startedAt: iso(-120),
    endedAt: iso(-62),
    currentStep: "Complete",
    spanCount: 6,
  },
];

export const sampleTraces: RunTrace[] = [
  {
    run: sampleRuns[0],
    spans: [
      {
        id: "span-scheduler",
        name: "Scheduler claimed backlog item",
        kind: "scheduler",
        status: "succeeded",
        startedAt: iso(2),
        endedAt: iso(3),
        attributes: { gaggleId: "engineering", workflowId: "default-implementation" },
        events: [{ name: "lease.acquired", timestamp: iso(2) }],
      },
      {
        id: "span-plan",
        parentSpanId: "span-scheduler",
        name: "Coder planned change",
        kind: "goober",
        status: "succeeded",
        startedAt: iso(4),
        endedAt: iso(13),
        attributes: { gooberId: "coder-1", taskId: "plan" },
        events: [{ name: "plan.completed", timestamp: iso(13) }],
      },
      {
        id: "span-implement",
        parentSpanId: "span-scheduler",
        name: "Coder implemented change",
        kind: "goober",
        status: "succeeded",
        startedAt: iso(14),
        endedAt: iso(48),
        attributes: { gooberId: "coder-1", taskId: "implement" },
        events: [{ name: "tests.completed", timestamp: iso(47), attributes: { passed: true } }],
      },
      {
        id: "span-human-gate",
        parentSpanId: "span-scheduler",
        name: "Waiting for QA approval",
        kind: "gate",
        status: "paused",
        startedAt: iso(49),
        attributes: { gateKind: "human", gateName: "QA approval" },
        events: [{ name: "approval.requested", timestamp: iso(50) }],
      },
    ],
  },
];

export const sampleGateApprovals: GateApprovalRequest[] = [
  {
    id: "gate-qa-run-101",
    runId: "run-101",
    gaggleId: "engineering",
    workflowId: "default-implementation",
    gateName: "QA approval",
    status: "pending",
    requestedBy: "default-implementation",
    requestedAt: iso(50),
    summary: "QA review required before this run can continue.",
  },
];

export interface MockApiOptions {
  snapshot?: InstanceSnapshot;
  runs?: RunSummary[];
  traces?: RunTrace[];
  gateApprovals?: GateApprovalRequest[];
}

export function createMockGoobersApi(options: MockApiOptions = {}): GoobersPortalApi {
  let gateApprovals = [...(options.gateApprovals ?? [])];
  const runs = options.runs ?? [];
  const traces = options.traces ?? [];

  return {
    async getInstanceSnapshot() {
      return options.snapshot ?? emptyInstanceSnapshot;
    },
    async listRuns() {
      return runs;
    },
    async getRunTrace(runId) {
      return traces.find((trace) => trace.run.id === runId);
    },
    async listGateApprovals() {
      return gateApprovals;
    },
    async decideGateApproval(requestId, decision: ApprovalDecision) {
      const request = gateApprovals.find((approval) => approval.id === requestId);
      if (!request) {
        throw new Error(`Gate approval request not found: ${requestId}`);
      }

      const updated: GateApprovalRequest = {
        ...request,
        status: decision.decision === "approve" ? "approved" : "rejected",
        approver: "current-user",
        decidedAt: new Date().toISOString(),
        decisionComment: decision.comment,
      };
      gateApprovals = gateApprovals.map((approval) => (approval.id === requestId ? updated : approval));
      return updated;
    },
  };
}

export function createSampleGoobersApi(): GoobersPortalApi {
  return createMockGoobersApi({
    snapshot: sampleInstanceSnapshot,
    runs: sampleRuns,
    traces: sampleTraces,
    gateApprovals: sampleGateApprovals,
  });
}
