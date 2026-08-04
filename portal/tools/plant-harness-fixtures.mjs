const page = (total) => ({
  limit: 100,
  total,
  hasMore: false,
  nextCursor: "",
});

const currentReadState = {
  epoch: "plant-harness",
  appliedSeq: 100,
  lagSeconds: 1,
  completeness: "complete",
  missing: [],
  degraded: [],
};

const portalConfig = {
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
    docsUrl: null,
    issuesUrl: null,
    chatUrl: null,
    links: [],
  },
  capabilities: { revealRun: true },
};

const gaggles = ["core", "tools"].map((name) => ({
  name,
  displayName: name === "core" ? "Core product" : "Developer tools",
  status: "configured",
  project: { provider: "github", owner: "synthetic", name: `${name}-project` },
  backlog: { provider: "github", project: `synthetic/${name}` },
  gooberCount: 1,
  workflowCount: 1,
  activeRunCount: name === "core" ? 4 : 1,
  warnings: [],
}));

const workflows = Object.fromEntries(
  gaggles.map((gaggle) => [
    gaggle.name,
    {
      items: [
        {
          identity: { gaggle: gaggle.name, name: "implementation" },
          displayName: "Implementation",
          purpose: "Synthetic Plant measurement workflow.",
          triggers: [{ type: "manual" }],
          readiness: { maxConcurrentRuns: 3 },
          concurrency: {
            activeRuns: gaggle.activeRunCount,
            maxConcurrentRuns: 3,
          },
          owners: [{ gaggle: gaggle.name, name: "implementer" }],
          stageCount: 3,
          definition: { version: 1, digest: `sha256:${gaggle.name}-fixture` },
          warnings: [],
        },
      ],
      page: page(1),
    },
  ]),
);

const goobers = Object.fromEntries(
  gaggles.map((gaggle) => [
    gaggle.name,
    {
      items: [
        {
          name: "implementer",
          displayName: `${gaggle.displayName} implementer`,
          role: "Synthetic fixture worker.",
          status: "configured",
          harness: "copilot",
          skills: [],
          capabilities: [],
          workflows: [{ gaggle: gaggle.name, name: "implementation" }],
          stages: [
            {
              workflow: { gaggle: gaggle.name, name: "implementation" },
              stage: "implement",
              kind: "agentic",
            },
            {
              workflow: { gaggle: gaggle.name, name: "implementation" },
              stage: "review",
              kind: "gate",
            },
          ],
          warnings: [],
        },
      ],
      page: page(1),
    },
  ]),
);

function workflowDetail(gaggle) {
  const summary = workflows[gaggle].items[0];
  return {
    ...summary,
    graph: {
      name: "implementation",
      version: 1,
      digest: `sha256:${gaggle}-fixture`,
      start: "query",
      nodes: [
        { id: "query", kind: "deterministic" },
        { id: "implement", kind: "agentic", owner: `${gaggle}/implementer` },
        { id: "review", kind: "gate", owner: `${gaggle}/implementer`, evaluator: "human" },
      ],
      edges: [
        { source: "query", target: "implement" },
        { source: "implement", target: "review" },
        { source: "review", target: "", outcome: "approve", terminal: "complete" },
        { source: "review", target: "implement", outcome: "changes" },
      ],
    },
    stages: [
      {
        name: "query",
        kind: "deterministic",
        goal: "Synthetic query.",
        owner: null,
        evaluator: "",
        capabilities: [],
        rawYaml: "name: query\n",
      },
      {
        name: "implement",
        kind: "agentic",
        goal: "Synthetic implementation.",
        owner: { gaggle, name: "implementer" },
        evaluator: "",
        capabilities: [],
        rawYaml: "name: implement\n",
      },
      {
        name: "review",
        kind: "gate",
        goal: "Synthetic review.",
        owner: { gaggle, name: "implementer" },
        evaluator: "human",
        capabilities: [],
        rawYaml: "name: review\n",
      },
    ],
  };
}

function run(id, gaggle, currentStage, startedAt) {
  return {
    id,
    workflow: "implementation",
    workflowVersion: 1,
    workflowDigest: `sha256:${gaggle}-fixture`,
    gaggle,
    trigger: { kind: "manual" },
    phase: "running",
    terminal: false,
    currentStage,
    startedAt,
    durationMillis: 600_000,
    lastActivityAt: "2026-08-03T21:59:00Z",
    lastSeq: 8,
    repassCount: 0,
    retryCount: 0,
    policyRetryCount: 0,
    infraRetryCount: 0,
    noWork: false,
  };
}

const activeRuns = [
  run("01PLANTACTIVE", "core", "implement", "2026-08-03T21:40:00Z"),
  run("01PLANTBLOCKED", "core", "implement", "2026-08-03T21:42:00Z"),
  run("01PLANTSTOPPED", "core", "query", "2026-08-03T21:41:00Z"),
  run("01PLANTUNKNOWN", "core", "review", "2026-08-03T21:43:00Z"),
  run("01PLANTHELD", "tools", "review", "2026-08-03T21:44:00Z"),
];

const recentRuns = [
  {
    ...run("01PLANTRECENT", "tools", undefined, "2026-08-03T20:00:00Z"),
    phase: "failed",
    terminal: true,
    finishedAt: "2026-08-03T20:10:00Z",
    lastActivityAt: "2026-08-03T20:10:00Z",
  },
];

const attempts = {
  "01PLANTACTIVE/implement": {
    runId: "01PLANTACTIVE",
    stage: "implement",
    attempts: [
      {
        id: "active-implement-1",
        visit: 1,
        number: 1,
        class: "initial",
        status: "running",
        startedSeq: 4,
        durationMillis: 600_000,
        artifacts: [],
      },
    ],
  },
  "01PLANTBLOCKED/implement": {
    runId: "01PLANTBLOCKED",
    stage: "implement",
    attempts: [
      {
        id: "blocked-implement-1",
        visit: 1,
        number: 1,
        class: "initial",
        status: "blocked",
        startedSeq: 4,
        finishedSeq: 5,
        durationMillis: 60_000,
        artifacts: [],
      },
    ],
  },
  "01PLANTSTOPPED/query": {
    runId: "01PLANTSTOPPED",
    stage: "query",
    attempts: [
      {
        id: "stopped-query-1",
        visit: 1,
        number: 1,
        class: "initial",
        status: "blocked",
        startedSeq: 2,
        finishedSeq: 3,
        durationMillis: 30_000,
        artifacts: [],
      },
    ],
  },
};

const heldEvents = {
  runId: "01PLANTHELD",
  events: [
    {
      schema: "v1",
      seq: 7,
      type: "gate.started",
      branch: 0,
      time: "2026-08-03T21:50:00Z",
      knownSchema: true,
      gate: "review",
      attempt: 1,
      attemptClass: "initial",
    },
    {
      schema: "v1",
      seq: 8,
      type: "gate.paused",
      branch: 0,
      time: "2026-08-03T21:51:00Z",
      knownSchema: true,
      gate: "review",
      attempt: 1,
      attemptClass: "initial",
    },
  ],
};

const STRESS_WORKFLOW_COUNT = 10;
const STRESS_STAGES = ["ingest", "plan", "build", "verify", "release"];

function stressFixtureResponse(url) {
  const path = url.pathname;
  const workflowNames = Array.from(
    { length: STRESS_WORKFLOW_COUNT },
    (_, index) => `line-${String(index + 1).padStart(2, "0")}`,
  );
  const workflowItems = workflowNames.map((name) => ({
    identity: { gaggle: "scale", name },
    // Deliberately duplicated: visible bay identity must distinguish these.
    displayName: "Assembly",
    purpose: "Synthetic 50-run Plant stress workflow.",
    triggers: [{ type: "manual" }],
    readiness: { maxConcurrentRuns: 5 },
    concurrency: { activeRuns: 5, maxConcurrentRuns: 5 },
    owners: STRESS_STAGES.map((stage) => ({
      gaggle: "scale",
      name: `${name}-${stage}`,
    })),
    stageCount: STRESS_STAGES.length,
    definition: { version: 1, digest: `sha256:${name}-stress` },
    warnings: [],
  }));
  const workerItems = workflowNames.flatMap((workflow) =>
    STRESS_STAGES.map((stage) => ({
      name: `${workflow}-${stage}`,
      displayName: `${workflow} ${stage}`,
      role: "Synthetic stress worker.",
      status: "configured",
      harness: "copilot",
      skills: [],
      capabilities: [],
      workflows: [{ gaggle: "scale", name: workflow }],
      stages: [
        {
          workflow: { gaggle: "scale", name: workflow },
          stage,
          kind: stage === "build" ? "agentic" : "deterministic",
        },
      ],
      warnings: [],
    })),
  );
  const runs = workflowNames.flatMap((workflow, workflowIndex) =>
    STRESS_STAGES.map((stage, stageIndex) =>
      run(
        `01STRESS${String(workflowIndex).padStart(2, "0")}${String(stageIndex).padStart(2, "0")}`,
        "scale",
        stage,
        "2026-08-03T21:40:00Z",
      ),
    ).map((item) => ({
      ...item,
      workflow,
      workflowDigest: `sha256:${workflow}-stress`,
    })),
  );

  if (path === "/api/v1/portal/config") {
    return json(portalConfig);
  }
  if (path === "/api/v1/health") {
    return json({
      apiVersion: "v1",
      schemaVersion: "v1",
      ready: true,
      healthy: true,
      instance: { name: "plant-stress", environment: "dev" },
      freshness: {
        observedAt: "2026-08-03T22:00:00Z",
        definitionsLoadedAt: "2026-08-03T21:59:00Z",
        journalUpdatedAt: "2026-08-03T21:59:30Z",
        lastSchedulerTickAt: "2026-08-03T21:59:59Z",
        lastTickAgeMillis: 1_000,
      },
    });
  }
  if (path === "/api/v1/instance") {
    return json({
      apiVersion: "v1",
      schemaVersion: "v1",
      name: "plant-stress",
      environment: "dev",
      ready: true,
      status: "ready",
      concurrency: { activeRuns: 50, maxConcurrentRuns: 50 },
      counts: {
        gaggles: 1,
        goobers: workerItems.length,
        workflows: workflowItems.length,
        activeRuns: runs.length,
      },
      warnings: [],
    });
  }
  if (path === "/api/v1/gaggles") {
    return json({
      items: [
        {
          name: "scale",
          displayName: "Scale fixture",
          status: "configured",
          project: { provider: "github", owner: "synthetic", name: "scale-project" },
          backlog: { provider: "github", project: "synthetic/scale" },
          gooberCount: workerItems.length,
          workflowCount: workflowItems.length,
          activeRunCount: runs.length,
          warnings: [],
        },
      ],
      page: page(1),
    });
  }
  if (path === "/api/v1/gaggles/scale/goobers") {
    return json({ items: workerItems, page: page(workerItems.length) });
  }
  if (path === "/api/v1/gaggles/scale/workflows") {
    return json({ items: workflowItems, page: page(workflowItems.length) });
  }
  if (path === "/api/v1/gaggles/scale/connections") {
    return json({ gaggle: "scale", repositories: [] });
  }
  const workflowMatch = path.match(
    /^\/api\/v1\/gaggles\/scale\/workflows\/([^/]+)$/,
  );
  if (workflowMatch) {
    const name = decodeURIComponent(workflowMatch[1]);
    if (workflowNames.includes(name)) {
      const summary = workflowItems.find((item) => item.identity.name === name);
      return json({
        ...summary,
        graph: {
          name,
          version: 1,
          digest: `sha256:${name}-stress`,
          start: STRESS_STAGES[0],
          nodes: STRESS_STAGES.map((stage) => ({
            id: stage,
            kind: stage === "build" ? "agentic" : "deterministic",
            owner: `scale/${name}-${stage}`,
          })),
          edges: STRESS_STAGES.slice(0, -1).map((stage, index) => ({
            source: stage,
            target: STRESS_STAGES[index + 1],
          })).concat([
            {
              source: STRESS_STAGES.at(-1),
              target: "",
              terminal: "complete",
            },
          ]),
        },
        stages: STRESS_STAGES.map((stage) => ({
          name: stage,
          kind: stage === "build" ? "agentic" : "deterministic",
          goal: `Synthetic ${stage}.`,
          owner: { gaggle: "scale", name: `${name}-${stage}` },
          evaluator: "",
          capabilities: [],
          rawYaml: `name: ${stage}\n`,
        })),
      });
    }
  }
  if (path === "/api/v1/runs") {
    return url.searchParams.get("phase") === "running"
      ? json({ runs })
      : json({
          runs: [],
          workflowActivity: workflowNames.map((workflow) => ({
            gaggle: "scale",
            workflow,
            activeRuns: 5,
          })),
        });
  }
  const attemptsMatch = path.match(
    /^\/api\/v1\/runs\/([^/]+)\/stages\/([^/]+)\/attempts$/,
  );
  if (attemptsMatch) {
    const runId = decodeURIComponent(attemptsMatch[1]);
    const stage = decodeURIComponent(attemptsMatch[2]);
    return json({
      runId,
      stage,
      attempts: [
        {
          id: `${runId}-${stage}-1`,
          visit: 1,
          number: 1,
          class: "initial",
          status: "running",
          startedSeq: 2,
          durationMillis: 600_000,
          artifacts: [],
        },
      ],
    });
  }
  return json(
    {
      error: {
        code: "unexpected_plant_stress_request",
        message: `${url.pathname}${url.search}`,
      },
    },
    500,
  );
}

export function plantFixtureResponse(requestUrl, state = { eventRequests: 0 }) {
  const url = new URL(requestUrl);
  const path = url.pathname;
  if (path === "/__plant-harness/refresh") {
    state.refreshEnabled = true;
    return json({ ready: true });
  }
  if (path === "/api/v1/events") {
    state.eventRequests += 1;
    const refreshSnapshot =
      state.refreshEnabled === true && state.refreshEventSent !== true;
    if (refreshSnapshot) {
      state.refreshEventSent = true;
    }
    const event =
      state.eventRequests === 1 || refreshSnapshot
        ? `id: plant-harness:${state.eventRequests}\nevent: snapshot\ndata: {"cursor":"plant-harness:${state.eventRequests}","models":["instance","run","workflow"]}\n\n`
        : `event: heartbeat\ndata: {"cursor":"plant-harness:${state.eventRequests}"}\n\n`;
    return {
      status: 200,
      headers: [
        { name: "Content-Type", value: "text/event-stream" },
        { name: "Cache-Control", value: "no-cache" },
      ],
      body: event,
    };
  }
  if (state.variant === "stress") {
    return stressFixtureResponse(url);
  }
  if (path === "/api/v1/portal/config") {
    return json(portalConfig);
  }
  if (path === "/api/v1/health") {
    return json({
      apiVersion: "v1",
      schemaVersion: "v1",
      ready: true,
      healthy: true,
      instance: { name: "plant-harness", environment: "dev" },
      freshness: {
        observedAt: "2026-08-03T22:00:00Z",
        definitionsLoadedAt: "2026-08-03T21:59:00Z",
        journalUpdatedAt: "2026-08-03T21:59:30Z",
        lastSchedulerTickAt: "2026-08-03T21:59:59Z",
        lastTickAgeMillis: 1_000,
      },
    });
  }
  if (path === "/api/v1/instance") {
    return json({
      apiVersion: "v1",
      schemaVersion: "v1",
      name: "plant-harness",
      environment: "dev",
      ready: true,
      status: "ready",
      concurrency: { activeRuns: 5, maxConcurrentRuns: 6 },
      counts: { gaggles: 2, goobers: 2, workflows: 2, activeRuns: 5 },
      warnings: [],
    });
  }
  if (path === "/api/v1/gaggles") {
    return json({ items: gaggles, page: page(gaggles.length) });
  }

  const gaggleMatch = path.match(/^\/api\/v1\/gaggles\/([^/]+)\/(goobers|workflows|connections)$/);
  if (gaggleMatch) {
    const gaggle = decodeURIComponent(gaggleMatch[1]);
    const collection = gaggleMatch[2];
    if (collection === "goobers" && goobers[gaggle]) {
      return json(goobers[gaggle]);
    }
    if (collection === "workflows" && workflows[gaggle]) {
      return json(workflows[gaggle]);
    }
    if (collection === "connections") {
      return json({ gaggle, repositories: [] });
    }
  }

  const workflowMatch = path.match(
    /^\/api\/v1\/gaggles\/([^/]+)\/workflows\/([^/]+)$/,
  );
  if (workflowMatch && decodeURIComponent(workflowMatch[2]) === "implementation") {
    const gaggle = decodeURIComponent(workflowMatch[1]);
    if (workflows[gaggle]) {
      return json(workflowDetail(gaggle));
    }
  }

  if (path === "/api/v1/runs") {
    if (url.searchParams.get("phase") === "running") {
      if (state.variant === "commons") {
        return json({ runs: [] });
      }
      state.activeRunRequests = (state.activeRunRequests ?? 0) + 1;
      const refreshedRun =
        state.refreshEnabled === true
          ? [
              run(
                "01PLANTREFRESH",
                "tools",
                "query",
                "2026-08-03T21:45:00Z",
              ),
            ]
          : [];
      return json({ runs: [...activeRuns, ...refreshedRun] });
    }
    return json({
      runs: recentRuns,
      workflowActivity: [
        { gaggle: "core", workflow: "implementation", activeRuns: 4 },
        { gaggle: "tools", workflow: "implementation", activeRuns: 1 },
      ],
    });
  }

  const attemptsMatch = path.match(
    /^\/api\/v1\/runs\/([^/]+)\/stages\/([^/]+)\/attempts$/,
  );
  if (attemptsMatch) {
    const key = `${decodeURIComponent(attemptsMatch[1])}/${decodeURIComponent(attemptsMatch[2])}`;
    if (key === "01PLANTREFRESH/query") {
      return json({
        runId: "01PLANTREFRESH",
        stage: "query",
        attempts: [
          {
            id: "refresh-query-1",
            visit: 1,
            number: 1,
            class: "initial",
            status: "running",
            startedSeq: 2,
            durationMillis: 60_000,
            artifacts: [],
          },
        ],
      });
    }
    if (attempts[key]) {
      return json(attempts[key]);
    }
  }

  if (path === "/api/v1/runs/01PLANTHELD/events") {
    return json(heldEvents);
  }

  if (path === "/api/v1/runs/01PLANTUNKNOWN/events") {
    return json({ runId: "01PLANTUNKNOWN", events: [] });
  }

  return json(
    {
      error: {
        code: "unexpected_plant_harness_request",
        message: `${url.pathname}${url.search}`,
      },
    },
    500,
  );
}

function json(value, status = 200) {
  const body =
    status >= 200 &&
    status < 300 &&
    value &&
    typeof value === "object" &&
    !Array.isArray(value)
      ? { ...value, readState: value.readState ?? currentReadState }
      : value;
  return {
    status,
    headers: [{ name: "Content-Type", value: "application/json" }],
    body: JSON.stringify(body),
  };
}
