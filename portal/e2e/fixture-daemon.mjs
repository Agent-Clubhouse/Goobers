import { createReadStream, existsSync, readFileSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { extname, isAbsolute, join, normalize, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

const port = 4173;
const distRoot = resolve(fileURLToPath(new URL(".", import.meta.url)), "../../internal/portalassets/dist");
const page = { limit: 100, total: 1, hasMore: false, nextCursor: "" };
const identity = { gaggle: "core", name: "implementation" };
const workflow = {
  identity,
  displayName: "Implementation",
  purpose: "Implement approved backlog items.",
  triggers: [{ type: "backlog-item", selector: { label: "goobers:ready" } }],
  readiness: { maxConcurrentRuns: 2 },
  concurrency: { activeRuns: 1, maxConcurrentRuns: 2 },
  owners: [{ gaggle: "core", name: "implementer" }],
  stageCount: 3,
  definition: { version: 7, digest: "sha256:core" },
  warnings: [],
};
const workflowGraph = {
  name: "implementation",
  version: 7,
  digest: "sha256:core",
  start: "query",
  nodes: [
    { id: "query", kind: "deterministic" },
    { id: "implement", kind: "agentic", owner: "core/implementer" },
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
const workflowDetail = {
  ...workflow,
  graph: workflowGraph,
  stages: [
    {
      name: "query",
      kind: "deterministic",
      goal: "Claim the next approved backlog item.",
      owner: null,
      evaluator: "",
      capabilities: ["github:issues:write"],
      timeoutSeconds: 120,
      rawYaml:
        "name: query\ntype: deterministic\ngoal: Claim the next approved backlog item.\ncapabilities:\n- github:issues:write\ntimeoutSeconds: 120\n",
    },
    {
      name: "implement",
      kind: "agentic",
      goal: "Implement the claimed item in an isolated worktree.",
      owner: { gaggle: "core", name: "implementer" },
      evaluator: "",
      capabilities: ["repo:push"],
      timeoutSeconds: 3600,
      retry: { maxAttempts: 2, backoffSeconds: 30 },
      policyActions: ["pr:open"],
      rawYaml:
        "name: implement\ntype: agentic\ngoober: implementer\ngoal: Implement the claimed item in an isolated worktree.\ncapabilities:\n- repo:push\npolicyActions:\n- pr:open\nretry:\n  maxAttempts: 2\n  backoffSeconds: 30\ntimeoutSeconds: 3600\n",
    },
    {
      name: "review",
      kind: "gate",
      goal: "Review the implementation and select its next target.",
      owner: { gaggle: "core", name: "implementer" },
      evaluator: "agentic",
      capabilities: ["repo:read"],
      branches: { pass: "", "needs-changes": "implement" },
      rawYaml:
        "name: review\nevaluator: agentic\nagentic:\n  goober: implementer\nbranches:\n  pass: \"\"\n  needs-changes: implement\n",
    },
  ],
};
const run = {
  id: "01JZE2ESMOKERUN",
  workflow: "implementation",
  workflowVersion: 7,
  workflowDigest: "sha256:core",
  gaggle: "core",
  trigger: { kind: "item", ref: "2033" },
  phase: "running",
  terminal: false,
  currentStage: "implement",
  startedAt: "2026-08-17T08:00:00Z",
  durationMillis: 120_000,
  lastActivityAt: "2026-08-17T08:02:00Z",
  stale: false,
  lastSeq: 4,
  repassCount: 0,
  retryCount: 0,
  policyRetryCount: 0,
  infraRetryCount: 0,
  noWork: false,
};

const completedRun = {
  ...run,
  id: "01JZE2ECOMPLETEDRUNWITHALONGIDENTIFIER",
  trigger: {
    kind: "item",
    ref: "refs/heads/users/jeffstei/a-very-long-portal-layout-verification-branch",
  },
  phase: "completed",
  terminal: true,
  currentStage: undefined,
  startedAt: "2026-08-17T06:00:00Z",
  finishedAt: "2026-08-17T06:03:00Z",
  durationMillis: 180_000,
  lastActivityAt: "2026-08-17T06:03:00Z",
  lastSeq: 9,
};

const runDetail = {
  ...run,
  graph: workflowGraph,
  graphStatus: "pinned",
  transitionsStatus: "projected",
  transitions: [
    { branch: 0, occurrence: 1, seq: 2, source: "query", target: "implement", status: "success" },
  ],
};

function journalEvent(seq, type, fields) {
  return {
    schema: "goobers.run.v1",
    seq,
    type,
    branch: 0,
    time: new Date(Date.parse(run.startedAt) + seq * 1_000).toISOString(),
    knownSchema: true,
    ...fields,
  };
}
const runEvents = {
  runId: run.id,
  events: [
    journalEvent(1, "run.started", { runId: run.id, workflow: run.workflow }),
    journalEvent(2, "stage.started", { stage: "query", attempt: 1, attemptClass: "initial" }),
    journalEvent(3, "stage.finished", {
      stage: "query",
      attempt: 1,
      attemptClass: "initial",
      status: "success",
    }),
    journalEvent(4, "stage.started", { stage: "implement", attempt: 1, attemptClass: "initial" }),
  ],
};
const stageAttempts = (stage, status) => ({
  runId: run.id,
  stage,
  attempts: [
    {
      id: `sta-${stage}-1`,
      visit: 1,
      number: 1,
      class: "initial",
      status,
      startedSeq: 2,
      durationMillis: 1_500,
      artifacts: [],
    },
  ],
});

const telemetryStats = {
  gaggles: [],
  runs: [],
  stages: [],
  usage: [],
  models: [],
  creditAssignment: [],
  causalCredit: null,
  curation: {
    everRecorded: false,
    runs: 0,
    reportedRuns: 0,
    ready: 0,
    needsHuman: 0,
    closed: 0,
    deduped: 0,
    split: 0,
    stale: 0,
    reconciled: 0,
    milestoned: 0,
    bounced: 0,
  },
  readyPool: {
    sampleEverRecorded: false,
    claimAgeSamples: 0,
    bounceEverRecorded: false,
    inFlightClaimSamples: 0,
    averageInFlightClaimAgeSeconds: 0,
    oldestInFlightClaimAgeSeconds: 0,
    forwardCurationThroughput: 0,
    implementationDemand: 0,
  },
};

const responses = new Map([
  [
    "/api/v1/health",
    {
      apiVersion: "v1",
      schemaVersion: "v1",
      ready: true,
      healthy: true,
      instance: { name: "e2e-fixture", environment: "test" },
      freshness: {
        observedAt: "2026-08-17T08:02:00Z",
        definitionsLoadedAt: "2026-08-17T08:00:00Z",
        journalUpdatedAt: "2026-08-17T08:02:00Z",
        lastSchedulerTickAt: "2026-08-17T08:01:59Z",
        lastTickAgeMillis: 1_000,
      },
    },
  ],
  [
    "/api/v1/instance",
    {
      apiVersion: "v1",
      schemaVersion: "v1",
      name: "e2e-fixture",
      environment: "test",
      ready: true,
      status: "ready",
      concurrency: { activeRuns: 1, maxConcurrentRuns: 2 },
      counts: { gaggles: 1, goobers: 1, workflows: 1, activeRuns: 1 },
      maintenance: {
        kind: "retention-sweep",
        state: "running",
        trigger: "periodic",
        startedAt: "2026-09-09T18:10:00Z",
        lastProgressAt: "2026-09-09T19:15:00Z",
        currentPhase: "projection-retention-with-a-long-but-coherent-phase-name",
        candidates: 23,
        removed: 12,
        failures: 0,
        lastResult: "running",
      },
      warnings: [],
    },
  ],
  [
    "/api/v1/portal/config",
    {
      brand: {
        name: "goobers",
        tagline: "local operations",
        scopeMark: "G",
        logoUrl: null,
        faviconUrl: null,
      },
      theme: {
        accentLight: null,
        accentDark: null,
        accentSoftLight: null,
        accentSoftDark: null,
        accentInkLight: null,
        accentInkDark: null,
      },
      support: {
        docsUrl: "https://example.test/docs",
        issuesUrl: "https://example.test/support",
        chatUrl: null,
        links: [],
      },
      capabilities: { revealRun: true },
    },
  ],
  [
    "/api/v1/gaggles",
    {
      items: [
        {
          name: "core",
          displayName: "Core product",
          status: "configured",
          project: { provider: "github", owner: "Agent-Clubhouse", name: "Goobers" },
          backlog: { provider: "github", project: "Agent-Clubhouse/Goobers" },
          gooberCount: 1,
          workflowCount: 1,
          activeRunCount: 1,
          warnings: [],
        },
      ],
      page,
    },
  ],
  [
    "/api/v1/gaggles/core/goobers",
    {
      items: [
        {
          name: "implementer",
          displayName: "Core implementer",
          role: "Implements claimed backlog items end to end.",
          status: "configured",
          harness: "copilot",
          skills: ["go", "react"],
          capabilities: ["repo:push"],
          workflows: [identity],
          stages: [{ workflow: identity, stage: "implement", kind: "agentic" }],
          warnings: [],
        },
      ],
      page,
    },
  ],
  ["/api/v1/gaggles/core/workflows", { items: [workflow], page }],
  ["/api/v1/gaggles/core/workflows/implementation", workflowDetail],
  ["/api/v1/gaggles/core/workflows/implementation/queue-eligibility", {
    gaggle: "core", workflow: "implementation", asOf: "2026-09-08T00:01:00Z",
    status: "observed", sourceRunId: "queue-observation",
    report: {
      version: 1, repositoryKey: "github|||Agent-Clubhouse|Goobers|", gaggle: "core",
      workflow: "implementation", runId: "queue-observation", observedAt: "2026-09-08T00:00:00Z",
      completeSnapshot: false, matchingItems: 2, omittedItems: 1,
      items: [{
        number: 42, eligible: false, reason: "escalated, human action required",
        nextStep: "Resolve the escalation and request a human retry.",
        claim: {
          state: "unclaimed", providerClaimLabel: true, comparison: "provider-label-without-live-local-lease",
          nextStep: "Check other instances before reconciling the label.",
        },
      }],
    },
  }],
  [
    "/api/v1/gaggles/core/connections",
    {
      gaggle: "core",
      repositories: [
        {
          repository: { provider: "github", owner: "Agent-Clubhouse", name: "Goobers" },
          accessMode: "read-write",
        },
      ],
    },
  ],
  [
    "/api/v1/telemetry/errors",
    {
      items: [
        {
          runId: run.id,
          workflow: run.workflow,
          stage: "implement",
          attempt: 1,
          code: "fixture.error",
          errorClass: "test",
          message: "Fixture daemon smoke error.",
          occurredAt: "2026-08-17T08:01:00Z",
        },
      ],
    },
  ],
  [
    "/api/v1/telemetry/costs",
    {
      scope: "summary",
      since: "2026-08-10T08:00:00Z",
      until: "2026-08-17T08:00:00Z",
      pullRequests: [],
      issues: [],
    },
  ],
  [
    "/api/v1/work-items",
    {
      items: [
        {
          provider: "github",
          repository: "Agent-Clubhouse/Goobers",
          kind: "pr",
          externalId: "4800",
          url: "https://github.com/Agent-Clubhouse/Goobers/pull/4800",
          actionCount: 2,
          lastOperation: "comment",
          lastActionAt: "2026-09-10T08:02:00Z",
          lastRunId: run.id,
          gaggle: "core",
          workflow: "implementation",
          runStatus: "running",
        },
      ],
      hasMore: false,
    },
  ],
  ["/api/v1/telemetry/stats", telemetryStats],
  ["/api/v1/telemetry/error-signatures", { items: [] }],
  [`/api/v1/runs/${run.id}`, runDetail],
  [`/api/v1/runs/${run.id}/events`, runEvents],
  [`/api/v1/runs/${run.id}/stages/query/attempts`, stageAttempts("query", "success")],
  [`/api/v1/runs/${run.id}/stages/implement/attempts`, stageAttempts("implement", "running")],
  [`/api/v1/runs/${run.id}/stages/review/attempts`, { runId: run.id, stage: "review", attempts: [] }],
]);

const contentTypes = {
  ".css": "text/css",
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript",
  ".png": "image/png",
  ".svg": "image/svg+xml",
};
const eventStreams = new Map();
let eventSequence = 0;
const constrainedAdmissions = new Map();

function admissionState(key) {
  if (!constrainedAdmissions.has(key)) {
    constrainedAdmissions.set(key, { active: 0, peak: 0, requests: 0 });
  }
  return constrainedAdmissions.get(key);
}

function sendJSON(response, value) {
  response.writeHead(200, { "Content-Type": "application/json" });
  response.end(JSON.stringify(value));
}

function sendError(response, status, code, message, headers = {}) {
  response.writeHead(status, { "Content-Type": "application/json", ...headers });
  response.end(JSON.stringify({ error: { code, message } }));
}

function emitInvalidation(key) {
  eventSequence += 1;
  const cursor = `fixture:${eventSequence}`;
  const event = {
    cursor,
    models: ["instance", "gaggle", "workflow", "goober", "run"],
    runIds: [run.id],
    workflows: [identity],
  };
  for (const [stream, streamKey] of eventStreams) {
    if (streamKey !== key) continue;
    stream.write(`id: ${cursor}\nevent: invalidate\ndata: ${JSON.stringify(event)}\n\n`);
  }
}

function serveEvents(response, key) {
  response.writeHead(200, {
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
    "Content-Type": "text/event-stream",
  });
  response.flushHeaders();
  eventStreams.set(response, key);
  response.on("close", () => eventStreams.delete(response));
}

function serveStatic(pathname, mode, response) {
  const requestPath =
    pathname === "/" ? "index.html" : normalize(decodeURIComponent(pathname)).slice(1);
  const file = resolve(join(distRoot, requestPath));
  const pathFromRoot = relative(distRoot, file);
  const escapesRoot = pathFromRoot === ".." || pathFromRoot.startsWith(`..${sep}`) || isAbsolute(pathFromRoot);
  if (escapesRoot || !existsSync(file) || !statSync(file).isFile()) {
    response.writeHead(404);
    response.end("not found");
    return;
  }
  response.writeHead(200, {
    "Content-Type": contentTypes[extname(file)] ?? "application/octet-stream",
  });
  if (requestPath === "index.html" && mode === "getting-started") {
    response.end(
      readFileSync(file, "utf8")
        .replace('content="daemon"', 'content="getting-started"')
        .replace("<title>Goobers · local operations</title>", "<title>Getting Started | Goobers</title>"),
    );
    return;
  }
  createReadStream(file).pipe(response);
}

createServer((request, response) => {
  const url = new URL(request.url ?? "/", `http://${request.headers.host}`);
  // Keep constrained browser sessions independent of sibling tests' counters
  // and SSE invalidations, including concurrent repeat-each invocations.
  const admissionKey = request.headers["x-test-constrained-admission"] ?? "";
  if (url.pathname === "/api/v1/test/admission") {
    const constrainedAdmission = admissionState(admissionKey);
    if (request.method === "POST") {
      constrainedAdmission.active = 0;
      constrainedAdmission.peak = 0;
      constrainedAdmission.requests = 0;
      sendJSON(response, { reset: true });
      return;
    }
    sendJSON(response, constrainedAdmission);
    return;
  }
  if (url.pathname === "/api/v1/events") {
    serveEvents(response, admissionKey);
    return;
  }
  if (url.pathname === "/api/v1/test/invalidate") {
    if (request.method !== "POST") {
      response.writeHead(405);
      response.end("method not allowed");
      return;
    }
    const connected = [...eventStreams.values()].filter((key) => key === admissionKey).length;
    if (connected === 0) {
      response.writeHead(409);
      response.end("no event stream connected");
      return;
    }
    emitInvalidation(admissionKey);
    sendJSON(response, { delivered: connected });
    return;
  }
  if (url.pathname === "/api/v1/runs") {
    const phase = url.searchParams.get("phase");
    sendJSON(response, {
      runs: phase === "completed" ? [completedRun] : phase && phase !== "running" ? [] : [run],
      ...(url.searchParams.get("latestPerWorkflow") === "true"
        ? { workflowActivity: [{ ...identity, activeRuns: 1 }] }
        : {}),
    });
    return;
  }
  const fixture = responses.get(url.pathname);
  if (fixture) {
    if (!admissionKey) {
      sendJSON(response, fixture);
      return;
    }
    const constrainedAdmission = admissionState(admissionKey);
    constrainedAdmission.requests += 1;
    if (constrainedAdmission.active >= 1) {
      sendError(
        response,
        503,
        "class_saturated",
        "too many concurrent bounded requests; retry shortly",
        { "Retry-After": "0" },
      );
      return;
    }
    constrainedAdmission.active += 1;
    constrainedAdmission.peak = Math.max(
      constrainedAdmission.peak,
      constrainedAdmission.active,
    );
    setTimeout(() => {
      constrainedAdmission.active -= 1;
      sendJSON(response, fixture);
    }, 40);
    return;
  }
  serveStatic(url.pathname, url.searchParams.get("mode"), response);
}).listen(port, "127.0.0.1", () => {
  console.log(`Fixture daemon listening on http://127.0.0.1:${port}`);
});
