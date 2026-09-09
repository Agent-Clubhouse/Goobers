import { expect, test, type Page } from "@playwright/test";
import type { Route } from "../src/routing";

const smokeRunId = "01JZE2ESMOKERUN";

interface RouteCase {
  path: string;
  heading: string;
}

// Keyed by `Route["page"]` so the e2e typecheck fails the moment the routing
// union gains a page that has no browser-level smoke coverage here (#4225).
const ROUTES: Record<Route["page"], RouteCase> = {
  overview: { path: "/#/overview", heading: "No runs need attention." },
  workflows: { path: "/#/workflows", heading: "Workflows" },
  goobers: { path: "/#/goobers", heading: "Goobers" },
  gaggle: { path: "/#/gaggle/core", heading: "Core product" },
  runs: { path: "/#/runs", heading: "Runs" },
  errors: { path: "/#/errors", heading: "Matching errors" },
  insight: { path: "/#/insight", heading: "Insight" },
  cost: { path: "/#/cost", heading: "Cost" },
  workflow: { path: "/#/workflow/core/implementation", heading: "Implementation" },
  run: { path: `/#/run/${smokeRunId}`, heading: `Run ${smokeRunId}` },
};

function trackConsoleErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") {
      errors.push(message.text());
    }
  });
  return errors;
}

for (const [name, { path, heading }] of Object.entries(ROUTES)) {
  test(`loads the ${name} route from fixture daemon data`, async ({ page }) => {
    const consoleErrors = trackConsoleErrors(page);
    await page.goto(path);

    await expect(page.getByRole("heading", { name: heading, exact: true })).toBeVisible();
    if (name === "workflow") {
      const queue = page.getByRole("region", { name: "PR queue eligibility" });
      await expect(queue.getByRole("table", { name: "Per-PR eligibility" })).toBeVisible();
      await expect(queue).toContainText("#42");
      await expect(queue).toContainText("Historical selection evidence—not permission to claim or merge.");
      await expect(queue).toContainText("Partial provider snapshot");
      await expect(queue).toContainText("2 matching PRs; 1 omitted");
      await expect(queue).toContainText("Provider claimed label: present");
      await expect(queue).toContainText("Check other instances before reconciling the label.");
    }
    expect(consoleErrors).toEqual([]);
  });
}

test("loads Overview and Workflows and processes an SSE invalidation", async ({ page }) => {
  const consoleErrors = trackConsoleErrors(page);
  let workflowRunReads = 0;
  const eventsConnected = page.waitForResponse(
    (response) => new URL(response.url()).pathname === "/api/v1/events",
  );
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (url.pathname === "/api/v1/runs" && url.searchParams.get("latestPerWorkflow") === "true") {
      workflowRunReads += 1;
    }
  });

  await page.goto("/#/overview");
  await expect(page.getByRole("heading", { name: "No runs need attention." })).toBeVisible();
  await eventsConnected;

  await page.goto("/#/workflows");
  await expect(page.getByRole("heading", { name: "Workflows" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Core product" })).toBeVisible();
  await expect.poll(() => workflowRunReads).toBeGreaterThanOrEqual(1);
  const invalidation = await page.request.post("/api/v1/test/invalidate");
  expect(invalidation.ok()).toBe(true);
  await expect.poll(() => workflowRunReads).toBeGreaterThanOrEqual(2);
  expect(consoleErrors).toEqual([]);
});

test("keeps Overview status and recent outcomes compact at desktop and narrow widths", async ({
  page,
}) => {
  const completedRunId = "01JZE2ECOMPLETEDRUNWITHALONGIDENTIFIER";
  for (const viewport of [
    { width: 1280, height: 800 },
    { width: 390, height: 844 },
  ]) {
    await page.setViewportSize(viewport);
    await page.goto("/#/overview");

    const status = page.getByRole("region", {
      name: "Daemon connection and instance counts",
    });
    await expect(status).toContainText("Retention sweep running");
    await expect(status).toContainText("Workflows1");
    await expect(status).toContainText("Active runs1");
    await expect(status).toContainText("Gaggles1");

    const outcomes = page.getByRole("region", { name: "Recent outcomes" });
    const outcomeRow = outcomes.locator(".data-row").filter({ hasText: completedRunId });
    await expect(outcomeRow).toBeVisible();
    await expect(outcomeRow.locator(`a[aria-label="Open run ${completedRunId}"]`)).toHaveAttribute(
      "href",
      `#/run/${completedRunId}`,
    );
    await expect(outcomes.getByTitle(completedRunId)).toBeVisible();
    await expect(
      outcomes.getByTitle(
        "Implementation · item refs/heads/users/jeffstei/a-very-long-portal-layout-verification-branch",
      ),
    ).toBeVisible();

    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
    expect(overflow).toBeLessThanOrEqual(1);
  }
});

test("loads the Gaggle page from fixture daemon data", async ({ page }) => {
  const consoleErrors = trackConsoleErrors(page);
  await page.goto("/#/gaggle/core");

  await expect(page.getByRole("heading", { name: "Core product" })).toBeVisible();
  await expect(page.getByText("Core implementer")).toBeVisible();
  await expect(page.getByRole("region", { name: "Core product active runs" })).toContainText(
    "01JZE2ESMOKERUN",
  );
  expect(consoleErrors).toEqual([]);
});

test("loads the Errors page from fixture daemon data", async ({ page }) => {
  const consoleErrors = trackConsoleErrors(page);
  await page.goto("/#/errors");

  await expect(page.getByRole("heading", { name: "Matching errors" })).toBeVisible();
  await expect(page.getByRole("region", { name: "Matching error history" })).toContainText(
    "fixture.error",
  );
  expect(consoleErrors).toEqual([]);
});

test("bounds route reads when daemon admission is constrained", async ({ page }) => {
  await page.route("**/api/v1/**", async (route) => {
    if (new URL(route.request().url()).pathname.startsWith("/api/v1/test/")) {
      await route.continue();
      return;
    }
    await route.continue({
      headers: {
        ...route.request().headers(),
        "x-test-constrained-admission": "1",
      },
    });
  });
  const enabled = await page.request.post("/api/v1/test/admission");
  expect(enabled.ok()).toBe(true);

  await page.goto("/#/overview");
  await page.getByRole("button", { name: "Workflows" }).click();
  await page.getByRole("button", { name: "Runs" }).click();
  await page.getByRole("button", { name: "Insight" }).click();
  await page.getByRole("button", { name: "Cost" }).click();
  await expect(page.getByRole("heading", { name: "Cost", exact: true })).toBeVisible();

  const result = await page.request.get("/api/v1/test/admission");
  const stats = (await result.json()) as { peak: number; requests: number };
  expect(stats.peak).toBeLessThanOrEqual(1);
  expect(stats.requests).toBeLessThanOrEqual(30);
});
