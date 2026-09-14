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
  events: [], transitions: [],
};

function snapshot(source: typeof sources[number]) {
  return {
    connected: true, source, mode: "daemon", instance: { name: source.label },
    workflows: [{ identity: { name: "implementation", gaggle: "team" }, triggers: [], concurrency: { activeRuns: 1 } }],
    runs: [run], attention: [],
    fleet: { associated: true, canonicalUri: "https://fleet.example.com/", connectionState: "connected", fleetId: "fleet" },
  };
}

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
      : url.pathname === "/api/runs" ? { connected: true, runs: [run] }
      : {};
    await route.fulfill({ json: body });
  });
  await page.goto("http://canvas.test/");
  await expect(page.getByRole("tab", { name: "Overview", exact: true })).toBeVisible();
  return errors;
}

test("canvas preserves run detail on refresh and returns keyboard focus to runs", async ({ page }) => {
  const errors = await openCanvas(page);
  await page.getByRole("tab", { name: "Runs", exact: true }).click();
  const runButton = page.getByRole("button", { name: "Open Run id", exact: true });
  await runButton.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("tab", { name: "Summary", exact: true })).toBeVisible();
  await page.route("http://canvas.test/api/snapshot?**", (route) =>
    route.fulfill({ json: snapshot({ ...sources[0], label: "Refreshed instance" }) }));
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.locator("#source-context")).toHaveText("Refreshed instance");
  await expect(page.locator("#dashboard")).toBeHidden();
  await page.getByRole("button", { name: "Back to runs" }).click();
  await expect(page.getByRole("tab", { name: "Runs", exact: true })).toHaveAttribute("aria-selected", "true");
  await expect(runButton).toBeFocused();
  await page.getByRole("button", { name: "Workflow", exact: true }).click();
  await expect(page.locator('th[data-sort="workflow"]')).toHaveAttribute("aria-sort", "ascending");
  expect(errors).toEqual([]);
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
  await openCanvas(page);
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
  await expect(page.getByRole("tab", { name: "Workflows", exact: true })).toHaveAttribute("aria-selected", "true");
});

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
