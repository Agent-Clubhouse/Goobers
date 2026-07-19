import { fixtureKey, type DaemonFixtures } from "../api/fixtureClient";
import {
  API_VERSION,
  SCHEMA_VERSION,
  type Gaggle,
  type Goober,
  type RunPhase,
  type RunSummary,
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
  finishedAt?: string,
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
    lastSeq: 9,
    repassCount: phase === "escalated" ? 3 : 0,
    retryCount: 0,
    policyRetryCount: 0,
    infraRetryCount: 0,
  };
}

export function populatedDaemonFixtures(): DaemonFixtures {
  const coreWorkflow = workflow("core");
  const toolsWorkflow = workflow("tools");
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
      [fixtureKey("core", "implementation")]: {
        ...coreWorkflow,
        graph: {
          name: "implementation",
          version: 7,
          digest: "sha256:core",
          start: "implement",
          nodes: [],
          edges: [],
        },
        stages: [],
      },
    },
    runs: {
      runs: [
        run(
          "01JZ455ESCALATE",
          "core",
          "completed",
          "2026-07-18T02:00:00Z",
          "2026-07-18T03:00:00Z",
        ),
        run(
          "01JZ402DASHBOARD",
          "core",
          "escalated",
          "2026-07-18T04:30:00Z",
          "2026-07-18T05:00:00Z",
        ),
        run(
          "01JZ441DAEMONAPI",
          "core",
          "running",
          "2026-07-18T06:00:00Z",
        ),
        run(
          "01JZ400FAILED",
          "core",
          "failed",
          "2026-07-18T03:30:00Z",
          "2026-07-18T04:00:00Z",
        ),
        run(
          "01JZ300ABORTED",
          "tools",
          "aborted",
          "2026-07-18T01:30:00Z",
          "2026-07-18T02:00:00Z",
        ),
      ],
    },
    telemetryStats: { runs: [], stages: [] },
    telemetryErrors: { items: [] },
  };
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
  };
}
