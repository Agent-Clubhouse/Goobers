import { fixtureKey, type DaemonFixtures } from "../api/fixtureClient";
import {
  API_VERSION,
  SCHEMA_VERSION,
  type Gaggle,
  type Goober,
  type RunDetail,
  type RunEvent,
  type RunPhase,
  type RunSummary,
  type WorkflowDetail,
  type WorkflowGraph,
  type WorkflowSummary,
} from "../api/types";

const page = (total: number) => ({
  limit: 100,
  total,
  hasMore: false,
  nextCursor: "",
});

const coreGaggle: Gaggle = {
  name: "core",
  displayName: "Core product",
  status: "configured",
  project: { provider: "github", owner: "Agent-Clubhouse", name: "Goobers" },
  backlog: { provider: "github", project: "Agent-Clubhouse/Goobers" },
  gooberCount: 1,
  workflowCount: 1,
  activeRunCount: 1,
  warnings: [],
};

const toolsGaggle: Gaggle = {
  name: "tools",
  displayName: "Developer tools",
  status: "configured",
  project: { provider: "github", owner: "Agent-Clubhouse", name: "Toolbox" },
  backlog: { provider: "github", project: "Agent-Clubhouse/Toolbox" },
  gooberCount: 1,
  workflowCount: 1,
  activeRunCount: 0,
  warnings: [],
};

function workflow(gaggle: string): WorkflowSummary {
  return {
    identity: { gaggle, name: "implementation" },
    displayName: "Implementation",
    purpose: `Implement approved ${gaggle} backlog items.`,
    triggers: [{ type: "backlog-item", selector: { label: "goobers:ready" } }],
    readiness: { maxConcurrentRuns: 2 },
    concurrency: {
      activeRuns: gaggle === "core" ? 1 : 0,
      maxConcurrentRuns: 2,
    },
    owners: [{ gaggle, name: "implementer" }],
    stageCount: 3,
    definition: { version: 7, digest: `sha256:${gaggle}` },
    warnings: [],
  };
}

function goober(gaggle: string): Goober {
  return {
    name: "implementer",
    displayName: `${gaggle === "core" ? "Core" : "Tools"} implementer`,
    role: "Implements claimed backlog items end to end.",
    status: "configured",
    harness: "copilot",
    skills: ["go", "react"],
    capabilities: ["repo:push"],
    workflows: [{ gaggle, name: "implementation" }],
    stages: [
      {
        workflow: { gaggle, name: "implementation" },
        stage: "implement",
        kind: "agentic",
      },
    ],
    warnings: [],
  };
}

function run(
  id: string,
  gaggle: string,
  phase: RunPhase,
  startedAt: string,
  lastSeq: number,
  finishedAt?: string,
  repassCount = 0,
): RunSummary {
  return {
    id,
    workflow: "implementation",
    workflowVersion: 7,
    workflowDigest: `sha256:${gaggle}`,
    gaggle,
    trigger: { kind: "item", ref: id.slice(-3) },
    phase,
    terminal: phase !== "running",
    currentStage: phase === "running" ? "review" : undefined,
    startedAt,
    finishedAt,
    durationMillis: finishedAt ? Date.parse(finishedAt) - Date.parse(startedAt) : 120_000,
    lastActivityAt: finishedAt ?? new Date(Date.parse(startedAt) + 120_000).toISOString(),
    lastSeq,
    repassCount,
    retryCount: 0,
    policyRetryCount: 0,
    infraRetryCount: 0,
  };
}

function graph(gaggle: string): WorkflowGraph {
  return {
    name: "implementation",
    version: 7,
    digest: `sha256:${gaggle}`,
    start: "query",
    nodes: [
      { id: "query", kind: "deterministic" },
      { id: "implement", kind: "agentic", owner: `${gaggle}/implementer` },
      { id: "review", kind: "gate", evaluator: "agentic" },
    ],
    edges: [
      { source: "query", target: "implement" },
      { source: "implement", target: "review" },
      { source: "review", target: "", outcome: "approve", terminal: "complete" },
      { source: "review", target: "implement", outcome: "needs-changes" },
      { source: "review", target: "@escalate", outcome: "fail", terminal: "escalate" },
    ],
  };
}

function workflowDetail(gaggle: string): WorkflowDetail {
  return {
    ...workflow(gaggle),
    graph: graph(gaggle),
    stages: [
      {
        name: "query",
        kind: "deterministic",
        goal: "Claim the next approved backlog item.",
        owner: null,
        evaluator: "",
        capabilities: ["github:issues:write"],
      },
      {
        name: "implement",
        kind: "agentic",
        goal: "Implement the claimed item in an isolated worktree.",
        owner: { gaggle, name: "implementer" },
        evaluator: "",
        capabilities: ["repo:push"],
      },
      {
        name: "review",
        kind: "gate",
        goal: "Review the implementation and select its next target.",
        owner: { gaggle, name: "implementer" },
        evaluator: "agentic",
        capabilities: ["repo:read"],
      },
    ],
  };
}

function detail(summary: RunSummary): RunDetail {
  return {
    ...summary,
    graph: graph(summary.gaggle),
    graphStatus: "pinned",
  };
}

function journalEvent(
  summary: RunSummary,
  seq: number,
  type: RunEvent["type"],
  fields: Partial<RunEvent> = {},
): RunEvent {
  return {
    schema: "v1",
    seq,
    type,
    branch: 0,
    time: new Date(Date.parse(summary.startedAt) + seq * 1_000).toISOString(),
    knownSchema: true,
    ...fields,
  };
}

function runEvents(summary: RunSummary): RunEvent[] {
  const events = [
    journalEvent(summary, 1, "run.started", {
      runId: summary.id,
      workflow: summary.workflow,
    }),
    journalEvent(summary, 2, "stage.started", {
      stage: "query",
      attempt: 1,
      attemptClass: "initial",
    }),
    journalEvent(summary, 3, "stage.finished", {
      stage: "query",
      attempt: 1,
      attemptClass: "initial",
      status: "success",
    }),
    journalEvent(summary, 4, "stage.started", {
      stage: "implement",
      attempt: 1,
      attemptClass: "initial",
    }),
  ];

  switch (summary.phase) {
    case "running":
      return [
        ...events,
        journalEvent(summary, 5, "stage.finished", {
          stage: "implement",
          attempt: 1,
          attemptClass: "initial",
          status: "success",
        }),
        journalEvent(summary, 6, "gate.started", {
          gate: "review",
          attempt: 1,
          attemptClass: "initial",
        }),
      ];
    case "completed":
      return [
        ...events,
        journalEvent(summary, 5, "stage.finished", {
          stage: "implement",
          attempt: 1,
          attemptClass: "initial",
          status: "success",
        }),
        journalEvent(summary, 6, "future.recorded", {
          schema: "v2-preview",
          knownSchema: false,
          raw: { future: "preserved but not rendered" },
        }),
        journalEvent(summary, 7, "gate.started", {
          gate: "review",
          attempt: 1,
          attemptClass: "initial",
        }),
        journalEvent(summary, 8, "gate.evaluated", {
          gate: "review",
          attempt: 1,
          attemptClass: "initial",
          verdict: "approve",
          target: "@complete",
        }),
        journalEvent(summary, 9, "run.finished", { status: "completed" }),
      ];
    case "failed":
      return [
        ...events,
        journalEvent(summary, 5, "stage.finished", {
          stage: "implement",
          attempt: 1,
          attemptClass: "initial",
          status: "failure",
        }),
        journalEvent(summary, 6, "run.finished", { status: "failed" }),
      ];
    case "aborted":
      return [
        ...events,
        journalEvent(summary, 5, "run.finished", {
          status: "aborted",
          reason: "Run was aborted by the operator.",
        }),
      ];
    case "escalated":
      return [
        ...events,
        journalEvent(summary, 5, "stage.finished", {
          stage: "implement",
          attempt: 1,
          attemptClass: "initial",
          status: "failure",
        }),
        journalEvent(summary, 6, "gate.started", {
          gate: "review",
          attempt: 1,
          attemptClass: "initial",
        }),
        journalEvent(summary, 7, "gate.evaluated", {
          gate: "review",
          attempt: 1,
          attemptClass: "initial",
          verdict: "needs-changes",
          target: "implement",
        }),
        journalEvent(summary, 8, "stage.started", {
          stage: "implement",
          attempt: 2,
          attemptClass: "policy",
        }),
        journalEvent(summary, 9, "stage.finished", {
          stage: "implement",
          attempt: 2,
          attemptClass: "policy",
          status: "success",
        }),
        journalEvent(summary, 10, "gate.started", {
          gate: "review",
          attempt: 2,
          attemptClass: "policy",
        }),
        journalEvent(summary, 11, "gate.evaluated", {
          gate: "review",
          attempt: 2,
          attemptClass: "policy",
          verdict: "fail",
          target: "@escalate",
        }),
        journalEvent(summary, 12, "run.finished", { status: "escalated" }),
      ];
  }
}

export function populatedDaemonFixtures(): DaemonFixtures {
  const coreWorkflow = workflow("core");
  const toolsWorkflow = workflow("tools");
  const runSummaries = [
    run(
      "01JZ455ESCALATE",
      "core",
      "completed",
      "2026-07-18T02:00:00Z",
      9,
      "2026-07-18T03:00:00Z",
    ),
    run(
      "01JZ402DASHBOARD",
      "core",
      "escalated",
      "2026-07-18T04:30:00Z",
      12,
      "2026-07-18T05:00:00Z",
      1,
    ),
    run(
      "01JZ441DAEMONAPI",
      "core",
      "running",
      "2026-07-18T06:00:00Z",
      6,
    ),
    run(
      "01JZ400FAILED",
      "core",
      "failed",
      "2026-07-18T03:30:00Z",
      6,
      "2026-07-18T04:00:00Z",
    ),
    run(
      "01JZ300ABORTED",
      "tools",
      "aborted",
      "2026-07-18T01:30:00Z",
      5,
      "2026-07-18T02:00:00Z",
    ),
  ];
  return {
    health: {
      apiVersion: API_VERSION,
      schemaVersion: SCHEMA_VERSION,
      ready: true,
      instance: { name: "local-dev", environment: "dev" },
      freshness: {
        observedAt: "2026-07-18T20:00:00Z",
        definitionsLoadedAt: "2026-07-18T19:59:00Z",
        journalUpdatedAt: "2026-07-18T19:58:00Z",
      },
    },
    instance: {
      apiVersion: API_VERSION,
      schemaVersion: SCHEMA_VERSION,
      name: "local-dev",
      environment: "dev",
      ready: true,
      status: "ready",
      concurrency: { activeRuns: 1, maxConcurrentRuns: 4 },
      counts: { gaggles: 2, goobers: 2, workflows: 2, activeRuns: 1 },
      warnings: [
        {
          code: "VER001",
          severity: "warning",
          scope: "tools/implementation",
          explanation: "Workflow definition uses a preview field.",
        },
      ],
    },
    gaggles: { items: [coreGaggle, toolsGaggle], page: page(2) },
    goobers: {
      core: { items: [goober("core")], page: page(1) },
      tools: { items: [goober("tools")], page: page(1) },
    },
    workflows: {
      core: { items: [coreWorkflow], page: page(1) },
      tools: { items: [toolsWorkflow], page: page(1) },
    },
    workflowDetails: {
      [fixtureKey("core", "implementation")]: workflowDetail("core"),
      [fixtureKey("tools", "implementation")]: workflowDetail("tools"),
    },
    runs: { runs: runSummaries },
    runDetails: Object.fromEntries(
      runSummaries.map((summary) => [summary.id, detail(summary)]),
    ),
    runEvents: Object.fromEntries(
      runSummaries.map((summary) => [
        summary.id,
        { runId: summary.id, events: runEvents(summary) },
      ]),
    ),
    telemetryStats: {
      gaggles: [
        {
          gaggle: "core",
          totalRuns: 4,
          completedRuns: 1,
          failedRuns: 1,
          otherRuns: 2,
          successRate: 0.5,
          avgDurationMs: 2_700_000,
          minDurationMs: 1_800_000,
          maxDurationMs: 3_600_000,
        },
        {
          gaggle: "tools",
          totalRuns: 1,
          completedRuns: 0,
          failedRuns: 0,
          otherRuns: 1,
          avgDurationMs: 1_800_000,
          minDurationMs: 1_800_000,
          maxDurationMs: 1_800_000,
        },
      ],
      runs: [
        {
          gaggle: "core",
          workflow: "implementation",
          totalRuns: 4,
          completedRuns: 1,
          failedRuns: 1,
          otherRuns: 2,
          successRate: 0.5,
          avgDurationMs: 2_700_000,
          minDurationMs: 1_800_000,
          maxDurationMs: 3_600_000,
        },
        {
          gaggle: "tools",
          workflow: "implementation",
          totalRuns: 1,
          completedRuns: 0,
          failedRuns: 0,
          otherRuns: 1,
          avgDurationMs: 1_800_000,
          minDurationMs: 1_800_000,
          maxDurationMs: 1_800_000,
        },
      ],
      stages: [
        {
          gaggle: "core",
          workflow: "implementation",
          stage: "implement",
          totalAttempts: 5,
          succeededAttempts: 3,
          failedAttempts: 2,
          successRate: 0.6,
          avgDurationMs: 1_260_000,
          minDurationMs: 480_000,
          maxDurationMs: 2_100_000,
          durationSamples: 5,
          p50DurationMs: 1_080_000,
          p95DurationMs: 2_100_000,
          tokenSamples: 4,
          p50Tokens: 24_000,
          p95Tokens: 48_000,
          costSamples: 4,
          p50CostUSD: 1.25,
          p95CostUSD: 2.5,
          retryWasteAttempts: 1,
          retryWasteDurationMs: 480_000,
          retryWasteTokens: 12_000,
          retryWasteCostUSD: 0.75,
        },
        {
          gaggle: "core",
          workflow: "implementation",
          stage: "review",
          totalAttempts: 4,
          succeededAttempts: 3,
          failedAttempts: 1,
          successRate: 0.75,
          avgDurationMs: 180_000,
          minDurationMs: 90_000,
          maxDurationMs: 360_000,
          durationSamples: 4,
          p50DurationMs: 150_000,
          p95DurationMs: 360_000,
          tokenSamples: 4,
          p50Tokens: 8_000,
          p95Tokens: 15_000,
          costSamples: 4,
          p50CostUSD: 0.4,
          p95CostUSD: 0.8,
          retryWasteAttempts: 0,
        },
        {
          gaggle: "tools",
          workflow: "implementation",
          stage: "implement",
          totalAttempts: 1,
          succeededAttempts: 0,
          failedAttempts: 0,
          durationSamples: 0,
          tokenSamples: 0,
          costSamples: 0,
          retryWasteAttempts: 0,
        },
      ],
    },
    telemetryErrors: { items: [] },
  };
}

// Builds a journal far larger than any Overview page, for asserting that the
// Overview and Runs page bound what they fetch and render (DASH-12/14/15). Runs
// are core/implementation with distinct StartedAt values so the server-order
// (newest first, id tie-break) is deterministic.
export function largeJournalFixtures(
  counts: Partial<Record<RunPhase, number>> = {},
): DaemonFixtures {
  const base = populatedDaemonFixtures();
  const distribution: Record<RunPhase, number> = {
    completed: counts.completed ?? 60,
    running: counts.running ?? 3,
    failed: counts.failed ?? 2,
    escalated: counts.escalated ?? 2,
    aborted: counts.aborted ?? 1,
  };
  const start = Date.parse("2026-07-18T00:00:00Z");
  const runs: RunSummary[] = [];
  let index = 0;
  for (const phase of Object.keys(distribution) as RunPhase[]) {
    for (let n = 0; n < distribution[phase]; n += 1) {
      const startedAt = new Date(start + index * 60_000).toISOString();
      const finishedAt =
        phase === "running" ? undefined : new Date(start + index * 60_000 + 30_000).toISOString();
      runs.push(
        run(
          `01JZTEST${String(index).padStart(9, "0")}`,
          "core",
          phase,
          startedAt,
          5,
          finishedAt,
        ),
      );
      index += 1;
    }
  }
  return { ...base, runs: { runs } };
}

export function emptyDaemonFixtures(): DaemonFixtures {
  const fixtures = populatedDaemonFixtures();
  return {
    ...fixtures,
    instance: {
      ...fixtures.instance,
      concurrency: { activeRuns: 0, maxConcurrentRuns: 4 },
      counts: { gaggles: 0, goobers: 0, workflows: 0, activeRuns: 0 },
      warnings: [],
    },
    gaggles: { items: [], page: page(0) },
    goobers: {},
    workflows: {},
    workflowDetails: {},
    runs: { runs: [] },
    runDetails: {},
    runEvents: {},
    telemetryStats: { gaggles: [], runs: [], stages: [] },
  };
}
