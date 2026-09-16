import { expect, test, type Page } from "@playwright/test";
import { renderHtml } from "../../.github/extensions/goobers-portal/render.mjs";

const sources = ["one", "two"].map((name) => ({
  id: `remote:${name}`, kind: "remote", value: `http://${name}`, label: `Instance ${name}`, connected: true,
}));
const run = {
  id: "run-1", workflow: "implementation", gaggle: "team", phase: "running",
  startedAt: "2026-09-14T18:00:00Z",
  currentStage: "implement",
  activeStages: [{ name: "implement", goober: "implementer" }],
  operator: {
    issue: { number: 7, title: "Implement thing", url: "https://github.com/octo/app/issues/7" },
    pullRequest: { id: 42, url: "https://github.com/octo/app/pull/42" },
    pullRequestTitle: "Ship thing",
  },
  graph: {
    start: "query",
    nodes: [
      { id: "query", kind: "deterministic" },
      { id: "implement", kind: "agentic", owner: "team/implementer" },
      { id: "review", kind: "gate", evaluator: "agentic" },
    ],
    edges: [
      { source: "query", target: "implement" },
      { source: "implement", target: "review" },
      { source: "review", target: "", outcome: "pass", terminal: "complete" },
    ],
  },
  events: [
    { type: "stage.finished", stage: "query", status: "succeeded" },
    { type: "stage.started", stage: "implement" },
  ],
  transitions: [{ source: "query", target: "implement" }],
};
const workflowDetail = {
  identity: { gaggle: "team", name: "implementation" },
  stages: [
    {
      name: "query", kind: "deterministic", goal: "Find work.", owner: null, evaluator: "",
      capabilities: ["github:issues:read"], timeoutSeconds: 120,
      requiredCapabilities: ["linux"], onTimeout: "fail",
      rawYaml: "name: query\ngoal: Find work.\n",
    },
    {
      name: "implement", kind: "agentic", goal: "Implement the issue.",
      owner: { gaggle: "team", name: "implementer" }, evaluator: "",
      capabilities: ["repo:push"], timeoutSeconds: 3600,
      retry: { maxAttempts: 2, backoffSeconds: 30 }, policyActions: ["pr:open"],
      requiredCapabilities: ["linux", "git"], onTimeout: "escalate",
      rawYaml: "name: implement\ngoal: Implement the issue.\npolicyActions:\n- pr:open\n",
    },
    {
      name: "review", kind: "gate", goal: "Review the change.", owner: null,
      evaluator: "agentic", capabilities: ["repo:read"],
      branches: { pass: "", "needs-changes": "implement" }, maxRepasses: 3,
      rawYaml: "name: review\nevaluator: agentic\nbranches:\n  pass: \"\"\n",
    },
  ],
};
const instanceWarning = {
  code: "MODEL002", severity: "warning", scope: "Goober/coder",
  explanation: "requested model is unavailable; using the harness default",
};
const workflowWarnings = [
  {
    code: "VER003", severity: "warning", scope: "Workflow/implementation",
    explanation: "expectedOutputs needs a result file",
  },
  {
    code: "VER001", severity: "warning", scope: "Workflow/implementation",
    explanation: "deprecated feature remains supported",
  },
];

const insightStats = {
  gaggles: [{ gaggle: "team", totalRuns: 20, completedRuns: 16, failedRuns: 3, otherRuns: 1, infraFailedRuns: 1, successRate: 0.8421 }],
  runs: [{ gaggle: "team", workflow: "implementation", totalRuns: 20, completedRuns: 16, failedRuns: 3, otherRuns: 1, successRate: 0.8421 }],
  stages: [{
    gaggle: "team", workflow: "implementation", stage: "implement",
    p50DurationMs: 4200, p95DurationMs: 9800, avgDurationMs: 5000, durationSamples: 12,
  }],
  usage: [{
    scope: "instance", totalAttempts: 25, p50Tokens: 1200, p95Tokens: 4300,
    costUSD: 12.34, p50CostUSD: 0.4, p95CostUSD: 1.1, costSamples: 25,
    retryWasteAttempts: 2, retryWasteTokens: 900, retryWasteCostUSD: 0.75,
  }, {
    scope: "gaggle", gaggle: "team", totalAttempts: 20, p50Tokens: 1000, p95Tokens: 3000,
    costUSD: 9.5, p50CostUSD: 0.35, p95CostUSD: 0.9, costSamples: 20,
    retryWasteAttempts: 1, retryWasteTokens: 300, retryWasteCostUSD: 0.25,
  }, {
    scope: "workflow", gaggle: "team", workflow: "implementation", totalAttempts: 18, p50Tokens: 950, p95Tokens: 2800,
    costUSD: 8.75, p50CostUSD: 0.32, p95CostUSD: 0.85, costSamples: 18,
    retryWasteAttempts: 1, retryWasteTokens: 250, retryWasteCostUSD: 0.2,
  }],
  models: [],
  creditAssignment: [{
    gaggle: "team", workflow: "implementation", stage: "implement", kind: "stage",
    failureShare: 0.62, failureRuns: 3, escalationRuns: 1, retryWasteAttempts: 2,
  }],
  causalCredit: [], graphAnalytics: {}, promotionSignals: [], promotionCandidates: [],
  curation: { runs: 4, ready: 2, needsHuman: 1, closed: 1, everRecorded: true },
  readyPool: {
    depth: 3, starved: false, oldestAgeSeconds: 120, averageClaimAgeSeconds: 45,
    inFlightClaimSamples: 1, averageInFlightClaimAgeSeconds: 30,
    bounceRate: 0.1, bounceEverRecorded: true,
    forwardCurationThroughput: 2, implementationDemand: 3, sampleEverRecorded: true,
  },
  trend: [
    { since: "2026-09-13T00:00:00Z", until: "2026-09-13T12:00:00Z", usage: [{ scope: "instance", costUSD: 3, p50Tokens: 900, costSamples: 5 }] },
    { since: "2026-09-13T12:00:00Z", until: "2026-09-14T00:00:00Z", usage: [{ scope: "instance", costUSD: 4, p50Tokens: 1000, costSamples: 6 }] },
  ],
  trendPrevious: { since: "2026-09-12T00:00:00Z", until: "2026-09-13T00:00:00Z", usage: [{ scope: "instance", costUSD: 7 }] },
};

const costSummary = {
  provider: "github",
  scope: "summary",
  since: "2026-09-08T00:00:00Z",
  until: "2026-09-15T00:00:00Z",
  pullRequests: [{
    provider: "github",
    repository: "Agent-Clubhouse/Goobers",
    url: "https://github.com/Agent-Clubhouse/Goobers/pull/5183",
    externalKind: "pr",
    externalId: "5183",
    totalRuns: 2,
    measuredRuns: 2,
    totalAttempts: 3,
    measuredAttempts: 3,
    nativeTotals: [{ unit: "usd", value: 4.2, estimated: false }],
    normalizedTotals: [{ unit: "aiCredits", value: 420, estimated: false }],
    billingModels: ["usd"],
    costBases: ["provider"],
    coverage: { totalRuns: 2, measuredRuns: 2, totalAttempts: 3, measuredAttempts: 3, complete: true, lowerBound: false },
    models: [{
      model: "gpt-test",
      usageAttempts: 3,
      measuredAttempts: 3,
      nativeTotals: [{ unit: "usd", value: 4.2, estimated: false }],
      normalizedTotals: [{ unit: "aiCredits", value: 420, estimated: false }],
      billingModels: ["usd"],
      costBases: ["provider"],
    }],
    runs: [{ runId: "run-1", startedAt: "2026-09-14T18:00:00Z", usageAttempts: 2, measuredAttempts: 2, nativeTotals: [], normalizedTotals: [], billingModels: [], costBases: [], models: [] }],
  }],
  issues: [{
    provider: "github",
    externalKind: "issue",
    externalId: "7",
    totalRuns: 1,
    measuredRuns: 1,
    totalAttempts: 1,
    measuredAttempts: 1,
    nativeTotals: [{ unit: "aiCredits", value: 12, estimated: true }],
    normalizedTotals: [{ unit: "usd", value: 1.5, estimated: true }],
    billingModels: ["premium"],
    costBases: ["estimate"],
    coverage: { totalRuns: 2, measuredRuns: 1, totalAttempts: 4, measuredAttempts: 2, complete: false, lowerBound: true },
    models: [],
    runs: [{ runId: "run-1", startedAt: "2026-09-14T18:00:00Z", usageAttempts: 1, measuredAttempts: 1, nativeTotals: [], normalizedTotals: [], billingModels: [], costBases: [], models: [] }],
  }],
};

const workItemPage = {
  items: [{
    provider: "github",
    repository: "Agent-Clubhouse/Goobers",
    kind: "pr",
    externalId: "42",
    url: "https://github.com/Agent-Clubhouse/Goobers/pull/42",
    actionCount: 2,
    lastOperation: "request-review",
    lastActionAt: "2026-09-14T18:00:00Z",
    lastRunId: "run-1",
    gaggle: "team",
    workflow: "implementation",
    runStatus: "running",
  }, {
    provider: "github",
    repository: "Agent-Clubhouse/Goobers",
    kind: "issue",
    externalId: "7",
    url: "https://github.com/Agent-Clubhouse/Goobers/issues/7",
    actionCount: 1,
    lastOperation: "comment",
    lastActionAt: "2026-09-14T17:00:00Z",
    lastRunId: "run-1",
    gaggle: "triage",
    workflow: "curation",
    runStatus: "completed",
  }],
  hasMore: false,
};

const workItemDetail = {
  provider: "github",
  repository: "Agent-Clubhouse/Goobers",
  kind: "issue",
  externalId: "7",
  url: "https://github.com/Agent-Clubhouse/Goobers/issues/7",
  cost: {
    costUSD: 1.25,
    totalRuns: 2,
    measuredRuns: 1,
    totalAttempts: 3,
    measuredAttempts: 2,
    lowerBound: true,
  },
  relatedPullRequests: [{
    provider: "github",
    repository: "Agent-Clubhouse/Goobers",
    kind: "pr",
    externalId: "42",
    url: "https://github.com/Agent-Clubhouse/Goobers/pull/42",
  }],
  actions: [{
    runId: "run-1",
    sequence: 9,
    operation: "comment",
    occurredAt: "2026-09-14T18:00:00Z",
    gaggle: "team",
    workflow: "implementation",
    runStatus: "running",
  }, {
    runId: "run-2",
    sequence: 4,
    operation: "label",
    occurredAt: "2026-09-14T17:00:00Z",
    gaggle: "triage",
    workflow: "curation",
    runStatus: "completed",
  }],
  truncated: false,
};

function snapshot(source: typeof sources[number]) {
  return {
    connected: true, source, mode: "daemon", instance: { name: source.label, warnings: [instanceWarning] },
    workflows: [{
      identity: { name: "implementation", gaggle: "team" },
      triggers: [],
      concurrency: { activeRuns: 1 },
      warnings: workflowWarnings,
    }],
    runs: [run], attention: [],
    fleet: source.id === sources[0].id
      ? { associated: true, canonicalUri: "https://fleet.example.com/", connectionState: "connected", fleetId: "fleet" }
      : { associated: false },
  };
}

test("configuration warnings dismiss by content and return when content changes", async ({ page }) => {
  const errors = await openCanvas(page);
  await expect(page.getByText(instanceWarning.explanation, { exact: true })).toBeVisible();
  await page.getByRole("button", {
    name: "Dismiss MODEL002 warning for Goober/coder",
  }).click();
  await expect(page.getByText("Warnings dismissed for this portal session.", { exact: true })).toBeVisible();

  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByText("Warnings dismissed for this portal session.", { exact: true })).toBeVisible();

  const changedWarning = { ...instanceWarning, explanation: "the configured model changed" };
  await page.route("http://canvas.test/api/snapshot?**", (route) =>
    route.fulfill({ json: { ...snapshot(sources[0]), instance: { name: "Instance one", warnings: [changedWarning] } } }));
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByText(changedWarning.explanation, { exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

test("workflow warning groups collapse and dismiss together", async ({ page }) => {
  const errors = await openCanvas(page);
  await page.getByRole("tab", { name: "Workflows", exact: true }).click();
  const warningCell = page.locator(".configuration-warning-cell");
  const group = warningCell.locator(".configuration-warning-group");
  await expect(group).toHaveAttribute("open", "");
  await group.locator("summary").click();
  await expect(group).not.toHaveAttribute("open", "");
  await group.locator("summary").click();
  await warningCell.getByRole("button", {
    name: "Dismiss all 2 warnings for Workflow/implementation",
  }).click();
  await expect(warningCell.getByText("Warnings dismissed for this portal session.", { exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(warningCell.getByText("Warnings dismissed for this portal session.", { exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

async function openCanvas(page: Page) {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("http://canvas.test/**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/") {
      await route.fulfill({ contentType: "text/html", body: renderHtml("browser-test") });
      return;
    }
    if (url.pathname === "/api/events") {
      await route.fulfill({ status: 503, body: "Event feed unavailable" });
      return;
    }
    const source = sources.find((entry) => entry.id === url.searchParams.get("source")) ?? sources[0];
    const body = url.pathname === "/api/sources" ? { sources }
      : url.pathname === "/api/selected-source" ? { sourceId: sources[0].id }
      : url.pathname === "/api/snapshot" ? snapshot(source)
      : url.pathname === "/api/run" ? { connected: true, run }
      : url.pathname === "/api/workflow-detail" ? { connected: true, workflow: workflowDetail }
      : url.pathname === "/api/runs" ? { connected: true, runs: [run] }
      : url.pathname === "/api/insight-stats" ? { connected: true, stats: insightStats }
      : url.pathname === "/api/cost-summary" ? {
          connected: true,
          costs: url.searchParams.get("scope") === "pr"
            ? { ...costSummary, scope: "pr", externalId: url.searchParams.get("id") ?? "", issues: [] }
            : url.searchParams.get("scope") === "issue"
              ? { ...costSummary, scope: "issue", externalId: url.searchParams.get("id") ?? "", pullRequests: [] }
              : costSummary,
        }
      : url.pathname === "/api/work-items" ? {
          connected: true,
          workItems: {
            ...workItemPage,
            items: url.searchParams.get("kind")
              ? workItemPage.items.filter((item) => item.kind === url.searchParams.get("kind"))
              : workItemPage.items,
          },
        }
      : url.pathname === "/api/work-item-detail" ? { connected: true, workItem: workItemDetail }
      : {};
    await route.fulfill({ json: body });
  });
  await page.goto("http://canvas.test/");
  await expect(page.getByRole("tab", { name: "Overview", exact: true })).toBeVisible();
  await expect(page.locator("#error")).toBeEmpty();
  return errors;
}

async function selectSource(page: Page, id: string) {
  await page.getByRole("button", { name: "Goobers source" }).click();
  await page.locator(`.source-picker-option[data-id="${id}"] .source-picker-option-select`).click();
}

async function observeRunNowResponses(page: Page) {
  await page.evaluate(() => {
    let settled = 0;
    const originalFetch = window.fetch.bind(window);
    window.fetch = async (...args) => {
      const response = await originalFetch(...args);
      if (new URL(response.url).pathname === "/api/run-workflow-now") {
        const originalJson = response.json.bind(response);
        response.json = async () => {
          try {
            return await originalJson();
          } finally {
            // Wait for the handler's promise continuations, not just the network response.
            window.setTimeout(() => { document.documentElement.dataset.runNowSettled = String(++settled); }, 0);
          }
        };
      }
      return response;
    };
  });
}

test("canvas preserves run detail on refresh and returns keyboard focus to runs", async ({ page }) => {
  const errors = await openCanvas(page);
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  await expect(page.getByRole("link", { name: "Issue #7: Implement thing", exact: true })).toBeVisible();
  await expect(page.getByRole("link", { name: "PR #42: Ship thing", exact: true })).toBeVisible();
  const runButton = page.getByRole("button", { name: "Open Run id", exact: true });
  await runButton.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("tab", { name: "Summary", exact: true })).toBeVisible();
  await expect(page.locator("#run-content .goober-chip").filter({ hasText: "implementer" })).toBeVisible();
  await page.route("http://canvas.test/api/snapshot?**", (route) =>
    route.fulfill({ json: snapshot({ ...sources[0], label: "Refreshed instance" }) }));
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.locator("#source-context")).toHaveText("Refreshed instance");
  await expect(page.locator("#dashboard")).toBeHidden();
  await expect(page).toHaveURL(/[?&]run=run-1(?:&|$)/);
  await page.getByRole("button", { name: "Back to runs" }).click();
  await expect(page.getByRole("tab", { name: "Runs", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(runButton).toBeFocused();
  await page.getByRole("button", { name: "Workflow", exact: true }).click();
  await expect(page.locator('th[data-sort="workflow"]')).toHaveAttribute("aria-sort", "ascending");
  expect(errors).toEqual([]);
});

test("run graph stages open a cached Fields and Raw YAML inspector", async ({ page }) => {
  const errors = await openCanvas(page);
  let detailRequests = 0;
  page.on("request", (request) => {
    if (new URL(request.url()).pathname === "/api/workflow-detail") detailRequests++;
  });
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  await page.getByRole("button", { name: "Open Run id", exact: true }).click();
  await page.getByRole("tab", { name: "Execution", exact: true }).click();

  const implement = page.getByRole("button", { name: "Inspect stage implement", exact: true });
  await implement.focus();
  await page.keyboard.press("Enter");
  const inspector = page.locator("#stage-inspector");
  await expect(inspector.getByRole("heading", { name: "implement", exact: true })).toBeVisible();
  await expect(inspector).toContainText("team/implementer");
  await expect(inspector).toContainText("Policy actions");
  await expect(inspector).toContainText("Required runner capabilities");
  await expect(inspector).toContainText("On timeout");
  await expect(inspector).not.toContainText("Branches");

  await inspector.getByRole("tab", { name: "Raw YAML", exact: true }).click();
  await expect(inspector.locator(".code-block")).toContainText("policyActions:");
  await expect(inspector.getByText("Policy actions", { exact: true })).toBeHidden();

  await page.getByRole("button", { name: "Inspect stage review", exact: true }).click();
  await expect(inspector.getByRole("heading", { name: "review", exact: true })).toBeVisible();
  await expect(inspector.locator(".code-block")).toContainText("evaluator: agentic");
  await inspector.getByRole("tab", { name: "Fields", exact: true }).click();
  await expect(inspector).toContainText("Branches");
  await expect(inspector).toContainText("pass \u2192 (terminal)");
  await expect(inspector).toContainText("Max repasses");
  await expect(inspector).not.toContainText("Policy actions");
  await page.getByRole("tab", { name: "Summary", exact: true }).click();
  await page.getByRole("tab", { name: "Execution", exact: true }).click();
  await expect(inspector.getByRole("tab", { name: "Fields", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(inspector.getByText("Branches", { exact: true })).toBeVisible();
  await expect(page.locator("#stage-inspector-status")).toHaveText("review definition loaded.");
  expect(detailRequests).toBe(1);
  expect(errors).toEqual([]);
});

for (const failure of [
  "Workflow detail requires a running Goobers daemon.",
  "HTTP 503: workflow unavailable",
]) {
  test(`stage inspector renders workflow-detail failure: ${failure}`, async ({ page }) => {
    const errors = await openCanvas(page);
    await page.route("http://canvas.test/api/workflow-detail?**", (route) =>
      route.fulfill({ json: { connected: false, reason: failure } }));
    await page.getByRole("tab", { name: "Runs", exact: true }).click();
    await page.getByRole("button", { name: "Open Run id", exact: true }).click();
    await page.getByRole("tab", { name: "Execution", exact: true }).click();
    await page.getByRole("button", { name: "Inspect stage implement", exact: true }).click();
    await expect(page.locator("#stage-inspector")).toHaveText(`Stage definition unavailable: ${failure}`);
    await expect(page.locator("#stage-inspector-status")).toHaveText(`Stage definition unavailable: ${failure}`);
    await expect(page.locator("#stage-inspector-status")).toHaveAttribute("aria-live", "assertive");
    expect(errors).toEqual([]);
  });
}

test("stage inspector retries after a failed workflow-detail request", async ({ page }) => {
  const errors = await openCanvas(page);
  let requests = 0;
  await page.route("http://canvas.test/api/workflow-detail?**", (route) => {
    requests++;
    return route.fulfill({
      json: requests === 1
        ? { connected: false, reason: "temporary failure" }
        : { connected: true, workflow: workflowDetail },
    });
  });
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  await page.getByRole("button", { name: "Open Run id", exact: true }).click();
  await page.getByRole("tab", { name: "Execution", exact: true }).click();
  const implement = page.getByRole("button", { name: "Inspect stage implement", exact: true });
  await implement.click();
  await expect(page.locator("#stage-inspector")).toHaveText(
    "Stage definition unavailable: temporary failure",
  );
  await implement.click();
  await expect(page.locator("#stage-inspector").getByRole("heading", {
    name: "implement",
    exact: true,
  })).toBeVisible();
  expect(requests).toBe(2);
  expect(errors).toEqual([]);
});

test("run controls and goober chips own their styling and respect reduced motion", async ({ page }) => {
  await openCanvas(page);
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  await expect(page.locator("#runs-table .run-id-control")).toHaveCSS("display", "inline-flex");
  const copy = page.getByRole("button", { name: "Copy run id", exact: true });
  await expect(copy).toHaveCSS("padding", "2px 7px");
  await copy.evaluate((button) => button.classList.add("copied"));
  await expect(copy).toHaveCSS("animation-name", "copy-pop");
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(copy).toHaveCSS("animation-name", "none");
  await page.getByRole("button", { name: "Open Run id", exact: true }).click();
  const chip = page.locator("#run-content .goober-chip").filter({ hasText: "implementer" });
  await expect(chip).toHaveCSS("display", "inline-flex");
  await expect(chip).toHaveCSS("border-radius", "999px");
  await expect(chip.locator(".goober-avatar")).toHaveCSS("line-height", "12px");
  await expect(chip.locator(".goober-label")).toHaveCSS("text-overflow", "ellipsis");
});

test("canvas ignores late run responses after switching sources", async ({ page }) => {
  await openCanvas(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  await page.route("http://canvas.test/api/run?**", async (route) => {
    await blocked;
    await route.fulfill({ json: { connected: true, run } });
  });
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  const request = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/run");
  await page.getByRole("button", { name: "Open Run id", exact: true }).click();
  await request;
  await selectSource(page, sources[1].id);
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/run");
  release();
  await response;
  await expect(page.locator("#run-view")).toBeHidden();
  await expect(page.locator("#run-content")).toBeEmpty();
});

test("run detail Associated work section dedupes the same issue referenced by multiple events", async ({ page }) => {
  await openCanvas(page);
  const dupedRun = {
    ...run,
    events: [
      {
        type: "stage.finished", stage: "query", status: "succeeded",
        externalRef: { provider: "github", kind: "issue", id: "160", url: "https://github.com/octo/app/issues/160" },
      },
      {
        type: "stage.started", stage: "implement",
        externalRef: { provider: "github", kind: "issue", id: "160", url: "https://api.github.com/repos/octo/app/issues/160" },
      },
      {
        type: "stage.finished", stage: "implement", status: "succeeded",
        externalRef: { provider: "github", kind: "issue", id: "160", url: "https://github.com/octo/app/issues/160#event-1" },
      },
    ],
  };
  await page.route("http://canvas.test/api/run?**", async (route) => {
    await route.fulfill({ json: { connected: true, run: dupedRun } });
  });
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  await page.getByRole("button", { name: "Open Run id", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Associated work", exact: true })).toBeVisible();
  await expect(page.locator(".external-refs a")).toHaveCount(1);
  await expect(page.locator(".external-refs a")).toHaveText("github issue #160");
});

test("workflows table shows the schedule frequency alongside the trigger kind", async ({ page }) => {
  await openCanvas(page);
  await page.route("http://canvas.test/api/snapshot?**", async (route) => {
    const scheduled = {
      ...snapshot(sources[0]),
      workflows: [{
        identity: { name: "implementation", gaggle: "team" },
        triggers: [{ type: "schedule", schedule: "@hourly" }],
        concurrency: { activeRuns: 1 },
        warnings: workflowWarnings,
      }],
    };
    await route.fulfill({ json: scheduled });
  });
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await page.getByRole("tab", { name: "Workflows", exact: true }).click();
  await expect(page.getByRole("row", { name: /implementation/ }).getByText("schedule (hourly)")).toBeVisible();
});

test("canvas fits a narrow panel and supports keyboard workflow drilldown", async ({ page }) => {
  await page.setViewportSize({ width: 360, height: 800 });
  await openCanvas(page);
  await expect(page.locator("#freshness")).toBeVisible();
  await expect(page.getByRole("link", { name: /Open Fleet portal/ })).toHaveAttribute("href", "https://fleet.example.com/");
  await page.getByRole("tab", { name: "Workflows", exact: true }).click();
  await page.getByRole("button", { name: "implementation", exact: true }).focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("tab", { name: "Runs", exact: true })).toHaveAttribute("aria-selected", "true");
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.getByText("More filters and saved views", { exact: true }).click();
  await expect(page.getByRole("textbox", { name: "Stage name (daemon sources)" })).toBeVisible();
});

test("workflow run now prompts only when force is required", async ({ page }) => {
  const errors = await openCanvas(page);
  const requests: Array<{ force?: boolean }> = [];
  await page.route("http://canvas.test/api/run-workflow-now", async (route) => {
    const body = route.request().postDataJSON() as { force?: boolean };
    requests.push(body);
    if (!body.force) {
      await route.fulfill({
        json: {
          ok: false,
          code: "trigger_rejected",
          reason: 'localscheduler: run conditions rejected the trigger for "implementation": conditions: budget',
        },
      });
      return;
    }
    await route.fulfill({ json: { ok: true, result: { runId: "forced-run" } } });
  });

  page.once("dialog", async (dialog) => {
    expect(dialog.message()).toContain("--force");
    await dialog.accept();
  });

  await page.getByRole("tab", { name: "Workflows", exact: true }).click();
  await page.getByRole("button", { name: "Run implementation now", exact: true }).click();
  await expect.poll(() => requests.length).toBe(2);
  expect(requests.map((request) => request.force ?? false)).toEqual([false, true]);
  await expect(page.locator("#workflow-run-status")).toHaveText("Triggered implementation (forced-run)");
  await expect(page.getByRole("tab", { name: "Workflows", exact: true })).toHaveAttribute("aria-selected", "true");
  await selectSource(page, sources[1].id);
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  await expect(page.locator("#workflow-run-status")).toBeEmpty();
  expect(errors).toEqual([]);
});

for (const reason of ["budget", "daily-budget", "concurrency"]) {
  test(`workflow run now does not retry ${reason} without approval`, async ({ page }) => {
    const errors = await openCanvas(page);
    const requests: Array<{ force: boolean }> = [];
    const dialogs: string[] = [];
    await page.route("http://canvas.test/api/run-workflow-now", async (route) => {
      requests.push(route.request().postDataJSON());
      await route.fulfill({ json: { ok: false, code: "trigger_rejected", reason: `conditions: ${reason}` } });
    });
    page.on("dialog", async (dialog) => {
      dialogs.push(dialog.message());
      await dialog.dismiss();
    });
    await page.getByRole("tab", { name: "Workflows", exact: true }).click();
    await page.getByRole("button", { name: "Run implementation now", exact: true }).click();
    await expect(page.locator("#error")).toHaveText(`Failed to run implementation: conditions: ${reason}`);
    await expect(page.getByRole("button", { name: "Run implementation now", exact: true })).toBeEnabled();
    expect(requests.map((request) => request.force)).toEqual([false]);
    expect(dialogs).toHaveLength(reason === "concurrency" ? 0 : 1);
    if (dialogs.length) expect(dialogs[0]).toContain("--force");
    await expect(page.locator("#workflow-run-status")).toBeEmpty();
    expect(errors).toEqual([]);
  });
}

for (const outcome of ["success", "rejected", "budget", "json-error"]) {
  test(`workflow run now ignores late ${outcome} after switching sources`, async ({ page }) => {
    const errors = await openCanvas(page);
    const requests: Array<{ source: string; force: boolean }> = [];
    const dialogs: string[] = [];
    let release!: () => void;
    const blocked = new Promise<void>((resolve) => { release = resolve; });
    await page.route("http://canvas.test/api/run-workflow-now", async (route) => {
      requests.push(route.request().postDataJSON());
      await blocked;
      if (outcome === "json-error") {
        await route.fulfill({ contentType: "application/json", body: "invalid json" });
      } else {
        await route.fulfill({
          json: outcome === "success"
            ? { ok: true, result: { runId: "late-run" } }
            : { ok: false, code: "trigger_rejected", reason: `conditions: ${outcome === "budget" ? "budget" : "concurrency"}` },
        });
      }
    });
    page.on("dialog", async (dialog) => {
      dialogs.push(dialog.message());
      await dialog.dismiss();
    });
    await observeRunNowResponses(page);
    await page.getByRole("tab", { name: "Workflows", exact: true }).click();
    await page.getByRole("button", { name: "Run implementation now", exact: true }).click();
    await expect.poll(() => requests.length).toBe(1);
    await selectSource(page, sources[1].id);
    await expect(page.locator("#source-context")).toHaveText("Instance two");
    release();
    await expect(page.locator("html")).toHaveAttribute("data-run-now-settled", "1");
    await expect(page.locator("#workflow-run-status")).toBeEmpty();
    await expect(page.locator("#error")).toBeEmpty();
    await expect(page.getByRole("button", { name: "Run implementation now", exact: true })).toBeEnabled();
    expect(requests.map(({ source, force }) => ({ source, force }))).toEqual([{ source: sources[0].id, force: false }]);
    expect(dialogs).toEqual([]);
    expect(errors).toEqual([]);
  });
}

for (const outcome of ["success", "rejected", "budget", "json-error"]) {
  test(`workflow run now ignores an old ${outcome} after A-B-A with a newer run pending`, async ({ page }) => {
    const errors = await openCanvas(page);
    await observeRunNowResponses(page);
    const releases: Array<() => void> = [];
    const requests: Array<{ source: string; force: boolean }> = [];
    const dialogs: string[] = [];
    page.on("dialog", async (dialog) => {
      dialogs.push(dialog.message());
      await dialog.dismiss();
    });
    await page.route("http://canvas.test/api/run-workflow-now", async (route) => {
      const index = requests.length;
      requests.push(route.request().postDataJSON());
      await new Promise<void>((resolve) => { releases[index] = resolve; });
      if (index === 0 && outcome === "json-error") {
        await route.fulfill({ contentType: "application/json", body: "invalid json" });
      } else {
        await route.fulfill({
          json: index === 1 || outcome === "success"
            ? { ok: true, result: { runId: index === 0 ? "old-run" : "new-run" } }
            : { ok: false, code: "trigger_rejected", reason: `conditions: ${outcome === "budget" ? "budget" : "concurrency"}` },
        });
      }
    });
    await page.getByRole("tab", { name: "Workflows", exact: true }).click();
    await page.getByRole("button", { name: "Run implementation now", exact: true }).click();
    await expect.poll(() => releases.length).toBe(1);
    await selectSource(page, sources[1].id);
    await expect(page.locator("#source-context")).toHaveText("Instance two");
    await selectSource(page, sources[0].id);
    await expect(page.locator("#source-context")).toHaveText("Instance one");
    await page.getByRole("button", { name: "Run implementation now", exact: true }).click();
    await expect.poll(() => releases.length).toBe(2);
    releases[0]();
    await expect(page.locator("html")).toHaveAttribute("data-run-now-settled", "1");
    // Refresh from real renderer state: a stale pending-key deletion must not re-enable the button.
    await page.getByRole("button", { name: "Refresh", exact: true }).click();
    await expect(page.getByRole("button", { name: "Triggering implementation", exact: true })).toBeDisabled();
    await expect(page.locator("#workflow-run-status")).toBeEmpty();
    await expect(page.locator("#error")).toBeEmpty();
    expect(dialogs).toEqual([]);
    releases[1]();
    await expect(page.locator("#workflow-run-status")).toHaveText("Triggered implementation (new-run)");
    await expect(page.getByRole("button", { name: "Run implementation now", exact: true })).toBeEnabled();
    expect(requests.map(({ source, force }) => ({ source, force }))).toEqual([
      { source: sources[0].id, force: false }, { source: sources[0].id, force: false },
    ]);
    expect(errors).toEqual([]);
  });
}

for (const roundTrip of [false, true]) {
  test(`source change during force confirmation${roundTrip ? " and back" : ""} cancels retry`, async ({ page }) => {
    const errors = await openCanvas(page);
    await observeRunNowResponses(page);
    let requests = 0;
    await page.route("http://canvas.test/api/run-workflow-now", async (route) => {
      ++requests;
      await route.fulfill({ json: { ok: false, code: "trigger_rejected", reason: "conditions: budget" } });
    });
    await page.evaluate(({ ids, roundTrip }) => {
      window.confirm = () => {
        const select = document.querySelector<HTMLSelectElement>("#source-select")!;
        // Emulate a source transition before the synchronous dialog returns.
        for (const id of roundTrip ? [ids[1], ids[0]] : [ids[1]]) {
          select.value = id;
          select.dispatchEvent(new Event("change"));
        }
        document.documentElement.dataset.forcePrompt = "true";
        return true;
      };
    }, { ids: sources.map(({ id }) => id), roundTrip });
    await page.getByRole("tab", { name: "Workflows", exact: true }).click();
    await page.getByRole("button", { name: "Run implementation now", exact: true }).click();
    await expect(page.locator("html")).toHaveAttribute("data-force-prompt", "true");
    await expect(page.locator("html")).toHaveAttribute("data-run-now-settled", "1");
    await expect(page.locator("#source-context")).toHaveText(roundTrip ? "Instance one" : "Instance two");
    await expect(page.locator("#workflow-run-status")).toBeEmpty();
    await expect(page.locator("#error")).toBeEmpty();
    await expect(page.getByRole("button", { name: "Run implementation now", exact: true })).toBeEnabled();
    expect(requests).toBe(1);
    expect(errors).toEqual([]);
  });
}

test("run filters use checkbox dropdowns instead of multi-select lists", async ({ page }) => {
  const errors = await openCanvas(page);
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  await expect(page.locator("#filter-workflow")).toBeHidden();
  await expect(page.locator('[data-filter-control="filter-workflow"]')).toBeVisible();
  await page.getByRole("button", { name: "All workflows", exact: true }).click();
  const request = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return url.pathname === "/api/runs" && url.searchParams.get("workflow") === "implementation";
  });
  await page.getByRole("checkbox", { name: "implementation", exact: true }).check();
  await request;
  await expect(page.getByRole("button", { name: "implementation", exact: true })).toBeVisible();
  expect(errors).toEqual([]);
});

test("canvas refreshes snapshots on live events after switching sources", async ({ page }) => {
  const errors = await openCanvas(page);
  let snapshots = 0;
  await page.route("http://canvas.test/api/snapshot?**", (route) =>
    route.fulfill({ json: snapshot({
      ...sources[1],
      label: ++snapshots === 1 ? "Initial snapshot" : "Live event snapshot",
    }) }));
  await page.route("http://canvas.test/api/events?**", (route) =>
    route.fulfill({
      contentType: "text/event-stream",
      body: 'data: {"type":"snapshot.changed"}\n\n',
    }));
  await selectSource(page, sources[1].id);
  await expect(page.locator("#source-context")).toHaveText("Live event snapshot");
  await expect(page.locator("#error")).toBeEmpty();
  expect(errors).toEqual([]);
});

test("canvas coalesces SSE bursts into one in-flight snapshot fetch", async ({ page }) => {
  await page.addInitScript(() => {
    class TestEventSource {
      onmessage: ((event: MessageEvent) => void) | null = null;
      listener = () => this.onmessage?.(new MessageEvent("message", {
        data: '{"type":"snapshot.changed"}',
      }));
      constructor() { window.addEventListener("test-sse-message", this.listener); }
      close() { window.removeEventListener("test-sse-message", this.listener); }
    }
    Object.defineProperty(window, "EventSource", { value: TestEventSource });
  });
  const errors = await openCanvas(page);
  let snapshots = 0;
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  await page.route("http://canvas.test/api/snapshot?**", async (route) => {
    ++snapshots;
    await blocked;
    await route.fulfill({ json: snapshot({ ...sources[0], label: "Burst refreshed" }) });
  });
  const first = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/snapshot");
  await page.evaluate(() => window.dispatchEvent(new Event("test-sse-message")));
  await first;
  await page.evaluate(() => {
    for (let index = 0; index < 10; ++index) window.dispatchEvent(new Event("test-sse-message"));
  });
  expect(snapshots).toBe(1);
  release();
  await expect(page.locator("#source-context")).toHaveText("Burst refreshed");
  expect(snapshots).toBe(1);
  await expect(page.locator("#error")).toBeEmpty();
  expect(errors).toEqual([]);
});

test("connecting a source clears the previous run and closes the source form", async ({ page }) => {
  const errors = await openCanvas(page);
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  await page.getByRole("button", { name: "Open Run id", exact: true }).click();
  await expect(page.getByRole("tab", { name: "Summary", exact: true })).toBeVisible();
  await page.route("http://canvas.test/api/add-source", (route) =>
    route.fulfill({ json: { id: sources[1].id } }));
  await page.getByRole("button", { name: "Goobers source" }).click();
  await page.getByRole("menuitem", { name: "Connect a source", exact: false }).click();
  await page.locator("#remote-url").fill(sources[1].value);
  await page.getByRole("button", { name: "Add remote", exact: true }).click();
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  await expect(page.locator("#connect-source-dialog")).not.toBeVisible();
  await expect(page.locator("#run-view")).toBeHidden();
  await expect(page.locator("#run-content")).toBeEmpty();
  await expect(page.locator("#error")).toBeEmpty();
  await expect(page).not.toHaveURL(/[?&]run=/);
  expect(errors).toEqual([]);
});

test("canvas derives Fleet link and associated work links from selected source data", async ({ page }) => {
  const errors = await openCanvas(page);
  const link = page.getByRole("link", { name: /Open Fleet portal/ });
  await expect(link).toHaveAttribute("href", "https://fleet.example.com/");
  await expect(link).toHaveAttribute("target", "_blank");
  await expect(link).toHaveAttribute("rel", "noopener noreferrer");
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  await expect(page.locator("#freshness")).toHaveAttribute("data-freshness", /live|reconnecting|stale/);
  await expect(page.getByRole("link", { name: "Issue #7: Implement thing" })).toHaveAttribute("href", "https://github.com/octo/app/issues/7");
  await expect(page.getByRole("link", { name: "PR #42: Ship thing" })).toHaveAttribute("href", "https://github.com/octo/app/pull/42");
  const copyButton = page.getByRole("button", { name: "Copy run id" });
  await expect(copyButton).toHaveAttribute("data-copy-run-id", "run-1");
  await expect(copyButton).toHaveText("📋");
  await page.evaluate(() => {
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: async () => undefined },
    });
  });
  await copyButton.click();
  await expect(copyButton).toHaveText("✅");
  await expect(copyButton).toHaveClass(/copied/);
  await page.getByRole("button", { name: "Open Run id", exact: true }).click();
  await expect(page.locator(".goober-chip", { hasText: "implementer" })).toBeVisible();
  expect(errors).toEqual([]);
});

test("canvas hides Fleet association when switching to an unassociated source", async ({ page }) => {
  const errors = await openCanvas(page);
  const link = page.getByRole("link", { name: /Open Fleet portal/ });
  await expect(link).toBeVisible();
  await selectSource(page, sources[1].id);
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  await expect(link).toHaveCount(0);
  await expect(page.locator("#fleet-panel")).toBeHidden();
  await selectSource(page, sources[0].id);
  await expect(link).toHaveAttribute("href", "https://fleet.example.com/");
  expect(errors).toEqual([]);
});

test("source picker supports keyboard navigation and restores focus on close", async ({ page }) => {
  const errors = await openCanvas(page);
  const trigger = page.getByRole("button", { name: "Goobers source" });

  // Opening moves focus to the currently-selected item, not just the first item.
  await trigger.focus();
  await trigger.press("Enter");
  const firstOption = page.locator(`.source-picker-option[data-id="${sources[0].id}"] .source-picker-option-select`);
  await expect(firstOption).toBeFocused();
  await expect(firstOption).toHaveAttribute("aria-checked", "true");

  // Arrow keys roam across menu items, including the trailing connect button.
  await page.keyboard.press("ArrowDown");
  const secondOption = page.locator(`.source-picker-option[data-id="${sources[1].id}"] .source-picker-option-select`);
  await expect(secondOption).toBeFocused();
  await page.keyboard.press("End");
  await expect(page.getByRole("menuitem", { name: "Connect a source", exact: false })).toBeFocused();
  await page.keyboard.press("Home");
  await expect(firstOption).toBeFocused();

  // Escape closes the menu and restores focus to the trigger.
  await page.keyboard.press("Escape");
  await expect(page.locator("#source-picker-menu")).toBeHidden();
  await expect(trigger).toBeFocused();

  // Selecting a source also restores focus to the trigger.
  await trigger.click();
  await secondOption.click();
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  await expect(page.locator("#source-picker-menu")).toBeHidden();
  await expect(trigger).toBeFocused();
  expect(errors).toEqual([]);
});

test("Work Items tab filters the bounded list and opens action, cost, and related-work detail", async ({ page }) => {
  const errors = await openCanvas(page);
  const listRequests: URL[] = [];
  const detailRequests: URL[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/work-items") listRequests.push(url);
    if (url.pathname === "/api/work-item-detail") detailRequests.push(url);
  });

  await page.getByRole("tab", { name: "Work Items", exact: true }).click();
  await expect(page.getByRole("button", {
    name: "Open PR #42 in Agent-Clubhouse/Goobers",
    exact: true,
  })).toBeVisible();
  await expect(page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  })).toBeVisible();
  expect(listRequests).toHaveLength(1);
  expect(listRequests[0].searchParams.get("limit")).toBe("200");

  await page.getByRole("combobox", { name: "Filter work items by gaggle" }).selectOption("triage");
  await expect(page.getByRole("button", {
    name: "Open PR #42 in Agent-Clubhouse/Goobers",
    exact: true,
  })).toHaveCount(0);
  await page.getByRole("searchbox", { name: "Search work items" }).fill("Goobers#7");
  await expect(page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  })).toBeVisible();
  expect(listRequests).toHaveLength(1);

  await page.getByRole("button", { name: "Issues", exact: true }).click();
  await expect.poll(() => listRequests.length).toBe(2);
  expect(listRequests[1].searchParams.get("kind")).toBe("issue");
  await page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  }).click();

  const detail = page.locator("#work-item-content");
  await expect(detail.getByRole("heading", { name: "Agent-Clubhouse/Goobers#7", exact: true })).toBeVisible();
  await expect(detail).toContainText("$1.25");
  await expect(detail).toContainText("Lower bound; some usage is unmeasured.");
  await expect(detail.getByRole("link", { name: "Open issue", exact: false })).toHaveAttribute(
    "href",
    "https://github.com/Agent-Clubhouse/Goobers/issues/7",
  );
  await expect(detail.getByRole("link", {
    name: "Open related PR Agent-Clubhouse/Goobers#42",
  })).toHaveAttribute("href", "https://github.com/Agent-Clubhouse/Goobers/pull/42");
  await expect(detail.getByRole("table", {
    name: "Action history for Agent-Clubhouse/Goobers#7",
  })).toBeVisible();

  await detail.getByRole("combobox", { name: "Filter actions by type" }).selectOption("label");
  await expect(detail.locator(".work-item-action-row")).toHaveCount(1);
  await expect(detail.locator(".work-item-action-row")).toContainText("Label");
  await detail.getByRole("button", { name: "View run", exact: true }).click();
  await expect(page.getByRole("button", { name: "Back to Work Items", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Back to Work Items", exact: true }).click();
  await expect(detail.getByRole("heading", { name: "Agent-Clubhouse/Goobers#7", exact: true })).toBeVisible();
  await expect(detail.getByRole("combobox", { name: "Filter actions by type" })).toHaveValue("label");
  await detail.getByRole("button", { name: "Back to Work Items" }).click();
  await expect(page.getByRole("searchbox", { name: "Search work items" })).toHaveValue("Goobers#7");

  await page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  }).click();
  await expect(detail.getByRole("heading", { name: "Agent-Clubhouse/Goobers#7", exact: true })).toBeVisible();
  expect(detailRequests).toHaveLength(1);
  expect(Object.fromEntries(detailRequests[0].searchParams.entries())).toEqual({
    source: sources[0].id,
    provider: "github",
    repository: "Agent-Clubhouse/Goobers",
    kind: "issue",
    id: "7",
  });
  expect(errors).toEqual([]);
});

test("Work Items discards late detail responses and resets filters when the source changes", async ({ page }) => {
  const errors = await openCanvas(page);
  let detailRequests = 0;
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  await page.route("http://canvas.test/api/work-item-detail?**", async (route) => {
    detailRequests++;
    if (detailRequests === 1) {
      await blocked;
    }
    await route.fulfill({ json: { connected: true, workItem: workItemDetail } });
  });

  await page.getByRole("tab", { name: "Work Items", exact: true }).click();
  await page.getByRole("searchbox", { name: "Search work items" }).fill("Goobers#7");
  await page.getByRole("combobox", { name: "Filter work items by gaggle" }).selectOption("triage");
  await page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  }).click();
  await expect(page.locator("#work-item-status")).toHaveText("Loading\u2026");

  await selectSource(page, sources[1].id);
  release();
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  await expect(page.getByRole("searchbox", { name: "Search work items" })).toHaveValue("");
  await expect(page.getByRole("combobox", { name: "Filter work items by gaggle" })).toHaveValue("");
  await expect(page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  })).toBeVisible();

  await page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  }).click();
  await expect(page.locator("#work-item-content").getByRole("heading", {
    name: "Agent-Clubhouse/Goobers#7",
    exact: true,
  })).toBeVisible();
  expect(detailRequests).toBe(2);
  expect(errors).toEqual([]);
});

test("Work Items tab renders loading, list error, detail error, and empty states", async ({ page }) => {
  const errors = await openCanvas(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  await page.route("http://canvas.test/api/work-items?**", async (route) => {
    await blocked;
    await route.fulfill({ json: { connected: false, reason: "Work items require a running Goobers daemon." } });
  });
  await page.getByRole("tab", { name: "Work Items", exact: true }).click();
  await expect(page.locator("#work-item-status")).toHaveText("Loading\u2026");
  release();
  await expect(page.locator("#work-item-status")).toHaveText("Work items require a running Goobers daemon.");
  await expect(page.locator("#work-item-content")).toBeEmpty();

  await page.unroute("http://canvas.test/api/work-items?**");
  await page.route("http://canvas.test/api/work-items?**", (route) =>
    route.fulfill({ json: { connected: true, workItems: { items: [], hasMore: false } } }));
  await page.getByRole("button", { name: "Pull requests", exact: true }).click();
  await expect(page.locator("#work-item-status")).toBeEmpty();
  await expect(page.locator("#work-item-content")).toContainText(
    "No confirmed provider actions match this filter.",
  );

  await page.unroute("http://canvas.test/api/work-items?**");
  await page.route("http://canvas.test/api/work-items?**", (route) =>
    route.fulfill({ json: { connected: true, workItems: workItemPage } }));
  await page.getByRole("button", { name: "Issues", exact: true }).click();
  await expect(page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  })).toBeVisible();
  await page.route("http://canvas.test/api/work-item-detail?**", (route) =>
    route.fulfill({ json: { connected: false, reason: "work item detail unavailable" } }));
  await page.getByRole("button", {
    name: "Open issue #7 in Agent-Clubhouse/Goobers",
    exact: true,
  }).click();
  await expect(page.locator("#work-item-status")).toHaveText("work item detail unavailable");
  await expect(page.getByRole("button", { name: "Back to Work Items" })).toBeVisible();
  expect(errors).toEqual([]);
});

test("Insights tab loads aggregate telemetry for the instance scope by default", async ({ page }) => {
  const errors = await openCanvas(page);
  const requests: URL[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/insight-stats") requests.push(url);
  });
  await page.getByRole("tab", { name: "Insights", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "Insight scope" })).toHaveValue("instance");
  await expect(page.getByRole("combobox", { name: "Time window" })).toHaveValue("24h");
  const content = page.locator("#insight-content");
  await expect(content).toContainText("Success and failure");
  await expect(content).toContainText("Tokens and retry waste");
  await expect(content).toContainText("Cost over time");
  await expect(content).toContainText("Ready-pool health");
  await expect(content).toContainText("Highest-contributing nodes");
  await expect(page.locator("#insight-status")).toBeEmpty();
  expect(requests).toHaveLength(1);
  expect(requests[0].searchParams.get("gaggle")).toBeNull();
  expect(requests[0].searchParams.get("workflow")).toBeNull();
  expect(requests[0].searchParams.get("since")).not.toBeNull();
  expect(requests[0].searchParams.get("trendBuckets")).toBe("16");
  expect(errors).toEqual([]);
});

test("Insights scope switch refetches with the matching gaggle/workflow params", async ({ page }) => {
  const errors = await openCanvas(page);
  const requests: URL[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/insight-stats") requests.push(url);
  });
  await page.getByRole("tab", { name: "Insights", exact: true }).click();
  const scopeSelect = page.getByRole("combobox", { name: "Insight scope" });
  await scopeSelect.selectOption("gaggle:team");
  await expect(page.locator("#insight-content tr.insight-outcome-summary")).toContainText("team");
  await scopeSelect.selectOption("workflow:team|implementation");
  await expect(page.locator("#insight-content tr.insight-outcome-summary")).toContainText("team / implementation");
  expect(requests).toHaveLength(3);
  expect(requests[1].searchParams.get("gaggle")).toBe("team");
  expect(requests[1].searchParams.get("workflow")).toBeNull();
  expect(requests[2].searchParams.get("gaggle")).toBe("team");
  expect(requests[2].searchParams.get("workflow")).toBe("implementation");
  expect(errors).toEqual([]);
});

test("Insights window switch refetches with an updated time range and omits since/trend for all time", async ({ page }) => {
  const errors = await openCanvas(page);
  const requests: URL[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/insight-stats") requests.push(url);
  });
  await page.getByRole("tab", { name: "Insights", exact: true }).click();
  const windowSelect = page.getByRole("combobox", { name: "Time window" });
  await windowSelect.selectOption("7d");
  await expect.poll(() => requests.length).toBe(2);
  await windowSelect.selectOption("all");
  await expect.poll(() => requests.length).toBe(3);
  await expect(page.locator("#insight-content")).toContainText("choose 24h, 7d, or 30d");
  expect(requests[1].searchParams.get("since")).not.toBeNull();
  expect(requests[2].searchParams.get("since")).toBeNull();
  expect(requests[2].searchParams.get("trendBuckets")).toBeNull();
  expect(errors).toEqual([]);
});

test("Insights tab renders loading, error, and empty states", async ({ page }) => {
  const errors = await openCanvas(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  await page.route("http://canvas.test/api/insight-stats?**", async (route) => {
    await blocked;
    await route.fulfill({ json: { connected: false, reason: "Telemetry stats require a running Goobers daemon." } });
  });
  await page.getByRole("tab", { name: "Insights", exact: true }).click();
  await expect(page.locator("#insight-status")).toHaveText("Loading\u2026");
  release();
  await expect(page.locator("#insight-status")).toHaveText("Telemetry stats require a running Goobers daemon.");
  await expect(page.locator("#insight-content")).toBeEmpty();

  await page.route("http://canvas.test/api/insight-stats?**", (route) =>
    route.fulfill({
      json: {
        connected: true,
        stats: {
          gaggles: [], runs: [], stages: [], usage: [], models: [], creditAssignment: [],
          causalCredit: [], graphAnalytics: {}, promotionSignals: [], promotionCandidates: [],
          curation: {}, readyPool: {}, trend: [], trendPrevious: {},
        },
      },
    }));
  await page.getByRole("combobox", { name: "Time window" }).selectOption("30d");
  await expect(page.locator("#insight-status")).toBeEmpty();
  await expect(page.locator("#insight-content")).toContainText("No telemetry in this window");
  expect(errors).toEqual([]);
});

test("Cost tab renders selected-scope usage, instance rollup, trend, and attributed work items", async ({ page }) => {
  const errors = await openCanvas(page);
  const insightRequests: URL[] = [];
  const costRequests: URL[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/insight-stats") insightRequests.push(url);
    if (url.pathname === "/api/cost-summary") costRequests.push(url);
  });
  await page.getByRole("tab", { name: "Cost", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "Cost scope" })).toHaveValue("instance");
  await expect(page.getByRole("combobox", { name: "Cost time window" })).toHaveValue("7d");
  const content = page.locator("#cost-content");
  await expect(content).toContainText("Cost summary");
  await expect(content).toContainText("$12.34");
  await expect(content).toContainText("Cost trend");
  await expect(content).toContainText("Cost by gaggle");
  await expect(content).toContainText("Cost by pull request and issue");
  await expect(content).toContainText("PR #5183");
  await expect(content).toContainText("Issue #7");
  await expect(page.locator("#cost-status")).toBeEmpty();
  expect(insightRequests).toHaveLength(1);
  expect(costRequests).toHaveLength(1);
  expect(costRequests[0].searchParams.get("scope")).toBe("summary");
  expect(costRequests[0].searchParams.get("since")).not.toBeNull();
  expect(errors).toEqual([]);
});

test("Cost scope and window changes refetch and hide instance-only rollup outside instance scope", async ({ page }) => {
  const errors = await openCanvas(page);
  const insightRequests: URL[] = [];
  const costRequests: URL[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/insight-stats") insightRequests.push(url);
    if (url.pathname === "/api/cost-summary") costRequests.push(url);
  });
  await page.getByRole("tab", { name: "Cost", exact: true }).click();
  const scopeSelect = page.getByRole("combobox", { name: "Cost scope" });
  await scopeSelect.selectOption("gaggle:team");
  await expect(page.locator("#cost-content")).toContainText("$9.50");
  await expect(page.locator("#cost-content")).not.toContainText("Cost by gaggle");
  await expect(page.locator("#cost-content")).toContainText("Attribution is instance-wide");
  await expect(page.locator("#cost-content")).toContainText("PR #5183");
  await page.getByRole("combobox", { name: "Cost time window" }).selectOption("all");
  await expect.poll(() => costRequests.length).toBe(3);
  await expect(page.locator("#cost-content")).toContainText("choose 24h, 7d, or 30d");
  await expect(page.locator("#cost-content")).toContainText("all-time attribution is capped at 90 days");
  expect(insightRequests.at(-1)?.searchParams.get("since")).toBeNull();
  expect(costRequests.at(-1)?.searchParams.get("since")).not.toBeNull();
  expect(costRequests.at(-1)?.searchParams.get("scope")).toBe("summary");
  expect(errors).toEqual([]);
});

test("Cost tab looks up pull request and issue costs by provider and id", async ({ page }) => {
  const errors = await openCanvas(page);
  const costRequests: URL[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/cost-summary") costRequests.push(url);
  });
  await page.getByRole("tab", { name: "Cost", exact: true }).click();
  await page.getByRole("textbox", { name: "External cost ID" }).fill("5183");
  await page.getByRole("button", { name: "Lookup" }).click();
  await expect(page.locator("#cost-content")).toContainText("Lookup result");
  await expect.poll(() => costRequests.some((url) =>
    url.searchParams.get("scope") === "pr" &&
    url.searchParams.get("provider") === "github" &&
    url.searchParams.get("id") === "5183",
  )).toBe(true);

  await page.getByRole("combobox", { name: "External cost type" }).selectOption("issue");
  await page.getByRole("textbox", { name: "External cost ID" }).fill("7");
  await page.getByRole("button", { name: "Lookup" }).click();
  await expect.poll(() => costRequests.some((url) =>
    url.searchParams.get("scope") === "issue" &&
    url.searchParams.get("provider") === "github" &&
    url.searchParams.get("id") === "7",
  )).toBe(true);
  expect(errors).toEqual([]);
});

test("Cost tab renders loading, error, and empty states", async ({ page }) => {
  const errors = await openCanvas(page);
  let release!: () => void;
  const blocked = new Promise<void>((resolve) => { release = resolve; });
  await page.route("http://canvas.test/api/cost-summary?**", async (route) => {
    await blocked;
    await route.fulfill({ json: { connected: false, reason: "Cost telemetry requires a running Goobers daemon." } });
  });
  await page.getByRole("tab", { name: "Cost", exact: true }).click();
  await expect(page.locator("#cost-status")).toHaveText("Loading\u2026");
  release();
  await expect(page.locator("#cost-status")).toHaveText("Attributed costs unavailable: Cost telemetry requires a running Goobers daemon.");
  await expect(page.locator("#cost-content")).toContainText("Cost summary");
  await expect(page.locator("#cost-content")).toContainText("Attributed pull request and issue costs could not be loaded");

  await page.route("http://canvas.test/api/cost-summary?**", (route) =>
    route.fulfill({ json: { connected: true, costs: { pullRequests: [], issues: [] } } }));
  await page.getByRole("combobox", { name: "Cost time window" }).selectOption("30d");
  await expect(page.locator("#cost-status")).toBeEmpty();
  await expect(page.locator("#cost-content")).toContainText("No pull request or issue cost was attributed");
  expect(errors).toEqual([]);
});

test("Cost tab keeps attributed costs visible when selected-scope stats fail", async ({ page }) => {
  const errors = await openCanvas(page);
  await page.route("http://canvas.test/api/insight-stats?**", (route) =>
    route.fulfill({ status: 503, contentType: "text/plain", body: "Telemetry stats budget exhausted." }));
  await page.getByRole("tab", { name: "Cost", exact: true }).click();
  await expect(page.locator("#cost-status")).toContainText("Selected-scope cost telemetry unavailable:");
  await expect(page.locator("#cost-content")).toContainText("Selected-scope usage, trend, and instance rollup are unavailable");
  await expect(page.locator("#cost-content")).toContainText("PR #5183");
  await expect(page.locator("#cost-content")).toContainText("Issue #7");
  expect(errors).toEqual([]);
});

test("Cost tab still runs explicit lookup when summary attribution fails", async ({ page }) => {
  const errors = await openCanvas(page);
  await page.route("http://canvas.test/api/cost-summary?**", (route) => {
    const url = new URL(route.request().url());
    if (url.searchParams.get("scope") === "summary") {
      return route.fulfill({ status: 503, contentType: "text/plain", body: "summary unavailable" });
    }
    return route.fulfill({
      json: {
        connected: true,
        costs: { ...costSummary, scope: "pr", externalId: url.searchParams.get("id") ?? "", issues: [] },
      },
    });
  });
  await page.getByRole("tab", { name: "Cost", exact: true }).click();
  await page.locator("#cost-lookup-id").fill("5183");
  await page.getByRole("button", { name: "Lookup", exact: true }).click();
  await expect(page.locator("#cost-status")).toContainText("Attributed costs unavailable:");
  await expect(page.locator("#cost-content")).toContainText("Attributed pull request and issue costs could not be loaded");
  await expect(page.locator("#cost-content")).toContainText("Lookup result");
  await expect(page.locator("#cost-content")).toContainText("PR #5183");
  expect(errors).toEqual([]);
});
