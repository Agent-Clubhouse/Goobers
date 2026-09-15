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
      : {};
    await route.fulfill({ json: body });
  });
  await page.goto("http://canvas.test/");
  await expect(page.getByRole("tab", { name: "Overview", exact: true })).toBeVisible();
  await expect(page.locator("#error")).toBeEmpty();
  return errors;
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
  await page.getByRole("combobox", { name: "Goobers source" }).selectOption(sources[1].id);
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/run");
  release();
  await response;
  await expect(page.locator("#run-view")).toBeHidden();
  await expect(page.locator("#run-content")).toBeEmpty();
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
  await page.getByRole("combobox", { name: "Goobers source" }).selectOption(sources[1].id);
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
    await page.getByRole("combobox", { name: "Goobers source" }).selectOption(sources[1].id);
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
    await page.getByRole("combobox", { name: "Goobers source" }).selectOption(sources[1].id);
    await expect(page.locator("#source-context")).toHaveText("Instance two");
    await page.getByRole("combobox", { name: "Goobers source" }).selectOption(sources[0].id);
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
  await page.getByRole("combobox", { name: "Goobers source" }).selectOption(sources[1].id);
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
  await page.locator("#add-source-details summary").click();
  await page.locator("#remote-url").fill(sources[1].value);
  await page.getByRole("button", { name: "Add remote", exact: true }).click();
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  await expect(page.locator("#add-source-details")).not.toHaveAttribute("open", "");
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
  await page.getByRole("combobox", { name: "Goobers source" }).selectOption(sources[1].id);
  await expect(page.locator("#source-context")).toHaveText("Instance two");
  await expect(link).toHaveCount(0);
  await expect(page.locator("#fleet-panel")).toBeHidden();
  await page.getByRole("combobox", { name: "Goobers source" }).selectOption(sources[0].id);
  await expect(link).toHaveAttribute("href", "https://fleet.example.com/");
  expect(errors).toEqual([]);
});
