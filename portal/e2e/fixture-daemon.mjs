import { createReadStream, existsSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { extname, join, normalize, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const port = 4173;
const distRoot = resolve(fileURLToPath(new URL(".", import.meta.url)), "../../cmd/goobers/portal-dist");
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
      support: { docsUrl: null, issuesUrl: null, chatUrl: null, links: [] },
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
]);

const contentTypes = {
  ".css": "text/css",
  ".html": "text/html; charset=utf-8",
  ".js": "text/javascript",
  ".png": "image/png",
  ".svg": "image/svg+xml",
};
const eventStreams = new Set();
let eventSequence = 0;

function sendJSON(response, value) {
  response.writeHead(200, { "Content-Type": "application/json" });
  response.end(JSON.stringify(value));
}

function emitInvalidation() {
  eventSequence += 1;
  const cursor = `fixture:${eventSequence}`;
  const event = {
    cursor,
    models: ["instance", "gaggle", "workflow", "goober", "run"],
    runIds: [run.id],
    workflows: [identity],
  };
  for (const stream of eventStreams) {
    stream.write(`id: ${cursor}\nevent: invalidate\ndata: ${JSON.stringify(event)}\n\n`);
  }
}

function serveEvents(response) {
  response.writeHead(200, {
    "Cache-Control": "no-cache",
    Connection: "keep-alive",
    "Content-Type": "text/event-stream",
  });
  response.flushHeaders();
  eventStreams.add(response);
  response.on("close", () => eventStreams.delete(response));
}

function serveStatic(pathname, response) {
  const relative = pathname === "/" ? "index.html" : normalize(decodeURIComponent(pathname)).slice(1);
  const file = resolve(join(distRoot, relative));
  if (!file.startsWith(`${distRoot}/`) || !existsSync(file) || !statSync(file).isFile()) {
    response.writeHead(404);
    response.end("not found");
    return;
  }
  response.writeHead(200, {
    "Content-Type": contentTypes[extname(file)] ?? "application/octet-stream",
  });
  createReadStream(file).pipe(response);
}

createServer((request, response) => {
  const url = new URL(request.url ?? "/", `http://${request.headers.host}`);
  if (url.pathname === "/api/v1/events") {
    serveEvents(response);
    return;
  }
  if (url.pathname === "/api/v1/test/invalidate") {
    if (request.method !== "POST") {
      response.writeHead(405);
      response.end("method not allowed");
      return;
    }
    if (eventStreams.size === 0) {
      response.writeHead(409);
      response.end("no event stream connected");
      return;
    }
    emitInvalidation();
    sendJSON(response, { delivered: eventStreams.size });
    return;
  }
  if (url.pathname === "/api/v1/runs") {
    const phase = url.searchParams.get("phase");
    sendJSON(response, {
      runs: phase && phase !== "running" ? [] : [run],
      ...(url.searchParams.get("latestPerWorkflow") === "true"
        ? { workflowActivity: [{ ...identity, activeRuns: 1 }] }
        : {}),
    });
    return;
  }
  const fixture = responses.get(url.pathname);
  if (fixture) {
    sendJSON(response, fixture);
    return;
  }
  serveStatic(url.pathname, response);
}).listen(port, "127.0.0.1", () => {
  console.log(`Fixture daemon listening on http://127.0.0.1:${port}`);
});
