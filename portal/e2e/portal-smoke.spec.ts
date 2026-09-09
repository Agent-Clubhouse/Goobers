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

const PRIMARY_ROUTES = [
  ["Overview", "/#/overview", "No runs need attention."],
  ["Workflows", "/#/workflows", "Workflows"],
  ["Goobers", "/#/goobers", "Goobers"],
  ["Runs", "/#/runs", "Runs"],
  ["Insight", "/#/insight", "Insight"],
  ["Cost", "/#/cost", "Cost"],
] as const;

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

for (const [area, path, heading] of PRIMARY_ROUTES) {
  test(`keeps the ${area} primary route within a 320px viewport`, async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 800 });
    await page.goto(path);

    await expect(page.getByRole("heading", { name: heading, exact: true })).toBeVisible();
    await expect(
      page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: area }),
    ).toHaveAttribute("aria-current", "page");
    await expect
      .poll(() => page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth))
      .toBeLessThanOrEqual(1);
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
    await expect(page.getByRole("region", { name: "Active runs" })).toContainText(
      "01JZE2ESMOKERUN",
    );
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

test("keeps the shared shell deliberate and accessible at 320px", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 320, height: 800 });
  await page.goto("/#/overview");
  await expect(page.getByRole("heading", { name: "No runs need attention." })).toBeVisible();

  const primary = page.getByRole("navigation", { name: "Primary" });
  for (const name of ["Overview", "Workflows", "Goobers", "Runs", "Insight", "Cost"]) {
    await expect(primary.getByRole("button", { name })).toBeVisible();
  }
  await expect(primary.getByRole("button", { name: "Overview" })).toHaveAttribute(
    "aria-current",
    "page",
  );

  const more = page.getByRole("button", {
    name: "Show gaggles, status, and support links",
  });
  await more.click();
  await expect(page.getByRole("navigation", { name: "Gaggles" })).toBeVisible();
  await expect(page.getByText("Daemon API", { exact: true })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Support" })).toBeVisible();

  await primary.getByRole("button", { name: "Cost" }).click();
  await expect(page.getByRole("heading", { name: "Cost", exact: true })).toBeVisible();
  await expect(primary.getByRole("button", { name: "Cost" })).toHaveAttribute(
    "aria-current",
    "page",
  );

  for (const control of [
    primary.getByRole("button", { name: "Cost" }),
    more,
    page.getByLabel("Scope"),
    page.getByLabel("Time window"),
    page.getByRole("button", { name: "Use dark theme" }),
  ]) {
    const box = await control.boundingBox();
    expect(box, "representative control should have geometry").not.toBeNull();
    expect(box!.width).toBeGreaterThanOrEqual(24);
    expect(box!.height).toBeGreaterThanOrEqual(24);
  }

  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
  expect(await page.evaluate(() => document.documentElement.scrollHeight)).toBeLessThan(8_000);
  await testInfo.attach("portal-shell-320.png", {
    body: await page.screenshot({ fullPage: true }),
    contentType: "image/png",
  });
});

test("keeps workflow hierarchy separate from scoped workspace pivots", async ({ page }) => {
  await page.goto("/#/workflow/core/implementation");
  const breadcrumbs = page.getByRole("navigation", { name: "Breadcrumb" });
  await expect(breadcrumbs).toContainText("Workflows");
  await expect(breadcrumbs).toContainText("core");
  await expect(breadcrumbs).toContainText("Implementation");
  await expect(breadcrumbs.getByRole("link")).toHaveCount(0);
  await expect(
    page.getByRole("navigation", { name: "Primary" }).getByRole("button", { name: "Workflows" }),
  ).toHaveAttribute(
    "aria-current",
    "page",
  );

  const costPivot = page.getByRole("link", {
    name: "View core / Implementation in Cost",
  });
  await expect(costPivot).toHaveAttribute(
    "href",
    "#/cost?gaggle=core&workflow=implementation",
  );
  await costPivot.click();
  await expect(page.getByRole("heading", { name: "Cost", exact: true })).toBeVisible();
  await expect(page.getByLabel("Cost scope")).toContainText("core / implementation");
});

test("shows one coherent polling fallback status with diagnostics out of primary copy", async ({
  page,
}) => {
  await page.route("**/api/v1/events*", async (route) => route.abort("failed"));
  await page.goto("/#/overview");

  const status = page.getByText("Data current via polling", { exact: true });
  await expect(status).toBeVisible({ timeout: 10_000 });
  await expect(status).not.toContainText("stream-error");
  await expect(status).toHaveAttribute("title", /stream-error.*\/api\/v1\/events/);
  await expect(page.getByRole("button", { name: "Retry live updates" })).toBeVisible();
});
