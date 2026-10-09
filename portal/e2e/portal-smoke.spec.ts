import { expect, test, type Locator, type Page } from "@playwright/test";
import type { Route } from "../src/routing";

const smokeRunId = "01JZE2ESMOKERUN";

interface RouteCase {
  path: string;
  heading: string;
}

// Keyed by `Route["page"]` so the e2e typecheck fails the moment the routing
// union gains a page that has no browser-level smoke coverage here (#4225).
const ROUTES: Record<Route["page"], RouteCase> = {
  overview: { path: "/#/overview", heading: "Active runs" },
  "instance-detail": { path: "/#/instance/recovery", heading: "Recovery metadata" },
  workflows: { path: "/#/workflows", heading: "Workflows" },
  goobers: { path: "/#/goobers", heading: "Goobers" },
  gaggle: { path: "/#/gaggle/core", heading: "Core product" },
  runs: { path: "/#/runs", heading: "Runs" },
  errors: { path: "/#/errors", heading: "Matching errors" },
  insight: { path: "/#/insight", heading: "Insight" },
  cost: { path: "/#/cost", heading: "Cost" },
  "work-items": { path: "/#/work-items", heading: "Work Items" },
  workflow: { path: "/#/workflow/core/implementation", heading: "Implementation" },
  run: { path: `/#/run/${smokeRunId}`, heading: `Run ${smokeRunId}` },
};

const COMPACT_NAV_ROUTES = [
  ["Overview", "/#/overview", "Active runs", "direct"],
  ["Workflows", "/#/workflows", "Workflows", "direct"],
  ["Goobers", "/#/goobers", "Goobers", "more"],
  ["Runs", "/#/runs", "Runs", "direct"],
  ["Insight", "/#/insight", "Insight", "more"],
  ["Cost", "/#/cost", "Cost", "more"],
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

// Distinct from trackConsoleErrors: an uncaught render throw (the #4825
// failure mode) surfaces as a `pageerror` event, which is not necessarily
// also logged via console.error in a production build.
function trackPageErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("pageerror", (error) => {
    errors.push(error.message);
  });
  return errors;
}

async function expectFullyInVisualViewport(locator: Locator, description: string) {
  await expect
    .poll(
      () =>
        locator.evaluate((element) => {
          const bounds = element.getBoundingClientRect();
          const viewport = window.visualViewport;
          const left = viewport?.offsetLeft ?? 0;
          const top = viewport?.offsetTop ?? 0;
          const right = left + (viewport?.width ?? window.innerWidth);
          const bottom = top + (viewport?.height ?? window.innerHeight);
          return (
            bounds.width > 0 &&
            bounds.height > 0 &&
            bounds.left >= left &&
            bounds.top >= top &&
            bounds.right <= right &&
            bounds.bottom <= bottom
          );
        }),
      { message: `${description} should be fully within the visual viewport` },
    )
    .toBe(true);
}

for (const [name, { path, heading }] of Object.entries(ROUTES)) {
  test(`loads the ${name} route from fixture daemon data`, async ({ page }) => {
    const consoleErrors = trackConsoleErrors(page);
    const pageErrors = trackPageErrors(page);
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
    expect(pageErrors).toEqual([]);
  });
}

// #4825: a fresh instance has no promotion-eligible causal confidence
// interval, so graphAnalytics.confidence is "untrusted" and its arrays are
// withheld — this is the common case on a fresh `init --demo` instance, not
// an edge case. The topology must still render, degraded, with zero
// pageerror events instead of a blank page.
test("renders the Workflow-detail topology with withheld graph analytics and zero page errors", async ({
  page,
}) => {
  const consoleErrors = trackConsoleErrors(page);
  const pageErrors = trackPageErrors(page);

  await page.goto("/#/workflow/core/implementation");

  await expect(page.getByRole("heading", { name: "Implementation" })).toBeVisible();
  await expect(page.locator(".workflow-graph-shell")).toBeVisible();
  // Degraded, not decorated: none of the analytics-derived labels a
  // trusted/partial confidence would add should appear.
  await expect(page.getByText("Cycle detected")).toHaveCount(0);
  await expect(page.getByText(/^Blame /)).toHaveCount(0);

  expect(consoleErrors).toEqual([]);
  expect(pageErrors).toEqual([]);
});

for (const [area, path, heading] of COMPACT_NAV_ROUTES) {
  test(`keeps the ${area} primary route within a 320px viewport`, async ({ page }) => {
    await page.setViewportSize({ width: 320, height: 800 });
    await page.goto(path);

    await expect(page.getByRole("heading", { name: heading, exact: true })).toBeVisible();
    const navigation = page.getByRole("navigation", { name: "Mobile primary", exact: true });
    const active = navigation.getByRole("button", { name: "Open navigation menu" });
    await expect(active).toHaveText(area);
    await expect
      .poll(() => page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth))
      .toBeLessThanOrEqual(1);
  });
}

test("keeps Insight and Cost summaries complete across narrow, landscape, zoomed, and desktop layouts", async ({
  page,
}) => {
  const layouts = [
    { name: "320px", width: 320, height: 800 },
    { name: "390px", width: 390, height: 844 },
    { name: "430px", width: 430, height: 844 },
    { name: "landscape", width: 844, height: 390 },
    { name: "200% zoom reflow", width: 195, height: 422 },
    { name: "desktop", width: 1280, height: 800 },
  ];

  for (const layout of layouts) {
    await page.setViewportSize({ width: layout.width, height: layout.height });

    await page.goto("/#/insight");
    const insightHeading = page.getByRole("heading", { name: "Insight", exact: true });
    const outcome = page.locator(".insight-outcome-row-summary");
    await expect(insightHeading, `Insight title at ${layout.name}`).toBeVisible();
    await expect(outcome, `Insight summary at ${layout.name}`).toBeVisible();
    for (const metric of ["success rate", "successful", "failed", "other", "all"]) {
      await expect(
        outcome.getByRole("link", { name: new RegExp(`View .*${metric}`, "i") }),
        `${metric} outcome at ${layout.name}`,
      ).toBeVisible();
    }
    await outcome.locator(".insight-scope-label strong").evaluate((element) => {
      element.textContent = "A very long operational scope name that must wrap without overflow";
    });
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth - innerWidth),
      `Insight document overflow at ${layout.name}`,
    ).toBeLessThanOrEqual(1);
    if (layout.width === 390 && layout.height === 844) {
      expect((await outcome.boundingBox())!.y).toBeLessThan(layout.height);
    }

    await page.goto("/#/cost");
    const costHeading = page.getByRole("heading", { name: "Cost", exact: true });
    const summary = page.locator(".cost-usage-summary").first();
    await expect(costHeading, `Cost title at ${layout.name}`).toBeVisible();
    await expect(summary, `Cost summary at ${layout.name}`).toBeVisible();
    await expect(summary.getByText("Total cost", { exact: true })).toBeVisible();
    await expect(summary.getByText("P50", { exact: true })).toBeVisible();
    await expect(summary.getByText("P95", { exact: true })).toBeVisible();
    const metrics = summary.locator(".cost-usage-spend > div");
    const boxes = await Promise.all((await metrics.all()).map((metric) => metric.boundingBox()));
    expect(boxes).toHaveLength(4);
    if (layout.width >= 360 && layout.width <= 760) {
      const supporting = boxes.slice(1);
      expect(Math.max(...supporting.map((box) => box!.y)) - Math.min(...supporting.map((box) => box!.y)))
        .toBeLessThanOrEqual(1);
      const values = await metrics.locator("dd").all();
      const valueBoxes = await Promise.all(values.slice(1).map((value) => value.boundingBox()));
      expect(Math.max(...valueBoxes.map((box) => box!.y)) - Math.min(...valueBoxes.map((box) => box!.y)))
        .toBeLessThanOrEqual(1);
    } else if (layout.width < 360) {
      expect(Math.abs(boxes[2]!.y - boxes[3]!.y)).toBeLessThanOrEqual(1);
    }
    const comparison = page.getByRole("region", { name: "Attributed costs comparison" });
    await expect(comparison).toBeVisible();
    await expect(comparison).toHaveAttribute("tabindex", "0");
    await expect(
      comparison.getByRole("table", { name: "Attributed costs" }).getByRole("columnheader"),
    ).toHaveCount(4);
    await expect(comparison.getByText("123,456,789.12 AIC").first()).toBeAttached();
    await comparison.locator(".external-cost-models").first().evaluate((element) => {
      element.setAttribute("style", "font-size: 20px");
      element.querySelector("li")!.textContent =
        "claude-sonnet-with-a-long-unbroken-model-identifier: 123,456 AIC · 14/14 attempts";
    });
    await comparison.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
    const modelList = comparison.locator(".external-cost-models").first();
    const modelBox = await modelList.boundingBox();
    const comparisonBox = await comparison.boundingBox();
    expect(modelBox).not.toBeNull();
    expect(comparisonBox).not.toBeNull();
    expect(modelBox!.x + modelBox!.width).toBeLessThanOrEqual(comparisonBox!.x + comparisonBox!.width - 12);
    expect(await modelList.evaluate((element) => element.scrollWidth - element.clientWidth))
      .toBeLessThanOrEqual(1);
    await expect(page.getByText("Scroll sideways to compare every cost column.")).toHaveCount(0);
    if (layout.width <= 430) {
      expect(
        await comparison.evaluate((element) => element.scrollWidth > element.clientWidth),
        `Cost comparison local overflow at ${layout.name}`,
      ).toBe(true);
    }
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth - innerWidth),
      `Cost document overflow at ${layout.name}`,
    ).toBeLessThanOrEqual(1);
    if (layout.width === 390 && layout.height === 844) {
      expect((await summary.boundingBox())!.y).toBeLessThan(layout.height);
    }

  }
});

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
  await expect(page.getByRole("heading", { name: "Active runs" })).toBeVisible();
  await eventsConnected;

  await page.goto("/#/workflows");
  await expect(page.getByRole("heading", { name: "Workflows" })).toBeVisible();
  await expect(page.getByRole("button", { name: /Core product/ })).toBeVisible();
  await expect.poll(() => workflowRunReads).toBeGreaterThanOrEqual(1);
  const invalidation = await page.request.post("/api/v1/test/invalidate");
  expect(invalidation.ok()).toBe(true);
  await expect.poll(() => workflowRunReads).toBeGreaterThanOrEqual(2);
  expect(consoleErrors).toEqual([]);
});

test("keeps Overview content first without horizontal overflow across compact layouts", async ({
  page,
}) => {
  const completedRunId = "01JZE2ECOMPLETEDRUNWITHALONGIDENTIFIER";
  for (const viewport of [
    { width: 1280, height: 800 },
    { width: 320, height: 844 },
    { width: 390, height: 844 },
    { width: 430, height: 844 },
    { width: 844, height: 390 },
  ]) {
    await page.setViewportSize(viewport);
    await page.goto("/#/overview");

    const status = page.getByRole("region", {
      name: "Daemon connection and instance counts",
    });
    await expect(status).toContainText("Active runs1");
    await expect(status).toContainText("Gaggles1");

    const outcomes = page.getByRole("region", { name: "Recent outcomes" });
    const active = page.getByRole("region", { name: "Active runs" });
    await expect(active).toContainText("01JZE2ESMOKERUN");
    const activeRow = active.locator(".data-row").filter({ hasText: smokeRunId });
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

    if (viewport.width === 390) {
      await expectFullyInVisualViewport(
        page.getByRole("heading", { level: 1 }),
        "Overview title at 390x844",
      );
      await expectFullyInVisualViewport(activeRow, "active run at 390x844");
    }

    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
    expect(overflow).toBeLessThanOrEqual(1);
  }

  await page.setViewportSize({ width: 1920, height: 1440 });
  await page.goto("/#/overview");
  const session = await page.context().newCDPSession(page);
  await session.send("Emulation.setDeviceMetricsOverride", {
    width: 960,
    height: 720,
    deviceScaleFactor: 2,
    mobile: false,
  });
  await expect
    .poll(() =>
      page.evaluate(() => ({
        width: window.innerWidth,
        height: window.innerHeight,
        devicePixelRatio: window.devicePixelRatio,
      })),
    )
    .toEqual({ width: 960, height: 720, devicePixelRatio: 2 });
  await expectFullyInVisualViewport(
    page.getByRole("heading", { level: 1 }),
    "Overview title at 200% zoom",
  );
  await expectFullyInVisualViewport(
    page
      .getByRole("region", { name: "Active runs" })
      .locator(".data-row")
      .filter({ hasText: smokeRunId }),
    "active run at 200% zoom",
  );
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
});

test("keeps optional Overview diagnostics accessible and exposes capacity warnings", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route("**/api/v1/instance", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    delete body.recoveryInventory;
    await route.fulfill({ response, json: body });
  });
  await page.goto("/#/overview");

  const diagnostics = page.getByText("Diagnostics and capacity", { exact: true });
  await expect(diagnostics).toBeVisible();
  await diagnostics.click();
  await expect(page.getByRole("status", { name: "Retention sweep running" })).toBeVisible();
  await expect(page.getByText("Recovery inventory", { exact: true })).toHaveCount(0);

  await page.unroute("**/api/v1/instance");
  await page.route("**/api/v1/instance", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    await route.fulfill({
      response,
      json: {
        ...body,
        recoveryInventory: {
          state: "warning",
          used: 9,
          limit: 10,
          unreadable: 0,
          overflow: 0,
          highWaterPercent: 80,
          inventoryRoot: "C:\\fixture\\recovery",
          policySource: "instance-config",
          observedAt: "2026-08-17T08:01:59Z",
        },
      },
    });
  });
  await page.reload();

  const warning = page.getByRole("alert", { name: "Recovery inventory warning" });
  await expect(warning).toBeVisible();
  await expect(
    warning.getByRole("link", { name: "Recovery capacity and operator actions" }),
  ).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth)).toBeLessThanOrEqual(1);
});

test("preserves attention selection and dismissals across navigation and Back", async ({ page }) => {
  await page.route("**/api/v1/runs*", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname === "/api/v1/runs" && url.searchParams.get("phase") === "failed") {
      await route.fulfill({
        json: {
          runs: [{
            id: "01JZE2EATTENTION",
            workflow: "implementation",
            workflowVersion: 7,
            workflowDigest: "sha256:core",
            gaggle: "core",
            trigger: { kind: "item", ref: "6645" },
            phase: "failed",
            terminal: true,
            startedAt: "2026-08-17T07:00:00Z",
            finishedAt: "2026-08-17T07:10:00Z",
            durationMillis: 600_000,
            lastActivityAt: "2026-08-17T07:10:00Z",
            stale: false,
            lastSeq: 8,
            repassCount: 0,
            retryCount: 0,
            policyRetryCount: 0,
            infraRetryCount: 0,
            noWork: false,
            terminalReason: "review failed",
          }],
        },
      });
      return;
    }
    await route.continue();
  });

  await page.goto("/#/overview");
  const selection = page.getByRole("checkbox", {
    name: /Select all 1 runs in core \/ implementation/,
  });
  await selection.check();
  await page.goto("/#/runs");
  await page.goBack();
  await expect(selection).toBeChecked();

  await page.getByRole("button", { name: /Dismiss all runs in core \/ implementation/ }).click();
  await expect(page.getByRole("button", { name: "Show dismissed (1)" })).toBeVisible();
  await page.goto("/#/runs");
  await page.goBack();
  await expect(page.getByRole("button", { name: "Show dismissed (1)" })).toBeVisible();
});

for (const viewport of [
  { width: 320, height: 800 },
  { width: 390, height: 844 },
  { width: 430, height: 932 },
  { width: 667, height: 375 },
]) {
  test(`keeps run details usable at ${viewport.width}x${viewport.height}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await page.goto(`/#/run/${smokeRunId}`);

    await expect(page.getByRole("heading", { name: `Run ${smokeRunId}` })).toBeVisible();
    const back = page.getByRole("button", { name: "Back to runs" });
    const backBox = await back.boundingBox();
    expect(backBox?.width).toBeGreaterThanOrEqual(44);
    expect(backBox?.height).toBeGreaterThanOrEqual(44);
    const tabs = page.getByRole("tablist", { name: "Run detail views" });
    for (const name of ["Overview", "Artifacts", "Diagnostics", "Journal"]) {
      const tab = tabs.getByRole("tab", { name });
      await expect(tab).toBeVisible();
      const box = await tab.boundingBox();
      expect(box?.height).toBeGreaterThanOrEqual(44);
    }
    await expect
      .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
      .toBeLessThanOrEqual(1);

    await tabs.getByRole("tab", { name: "Journal" }).click();
    const event = page.getByRole("button", { name: /^Select sequence 3:/ });
    await event.click();
    const dialog = page.getByRole("dialog", { name: "Event detail" });
    await expect(dialog).toBeFocused();
    await expect(dialog).toContainText("Sequence 3");
    const close = dialog.getByRole("button", { name: "Close event detail" });
    const closeBox = await close.boundingBox();
    expect(closeBox?.width).toBeGreaterThanOrEqual(44);
    expect(closeBox?.height).toBeGreaterThanOrEqual(44);
    if (viewport.width <= 480) {
      const box = await dialog.boundingBox();
      expect(box?.width).toBe(viewport.width);
      expect(box?.height).toBe(viewport.height);
    }
    await close.click();
    await expect(event).toBeFocused();
  });
}

test("drills from an error occurrence and restores its phone focus and scroll", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/errors");

  const origin = page.getByRole("link", {
    name: `Open latest run ${smokeRunId} for error fixture.error`,
  });
  await origin.focus();
  await page.locator(".portal-main").evaluate((element) => {
    const spacer = document.createElement("div");
    spacer.style.height = "600px";
    element.append(spacer);
    element.scrollTop = 120;
  });
  const scrollTop = await page.locator(".portal-main").evaluate((element) => element.scrollTop);
  await origin.click();
  await expect(page.getByRole("heading", { name: `Run ${smokeRunId}` })).toBeVisible();
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
  await page.getByRole("button", { name: "Back to runs" }).click();
  await expect(page.getByRole("heading", { name: "Matching errors" })).toBeVisible();
  await expect(origin).toBeFocused();
  await expect
    .poll(() => page.locator(".portal-main").evaluate((element) => element.scrollTop))
    .toBe(scrollTop);
});

test("contains event sheet keyboard focus and restores its invoking event", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/#/run/${smokeRunId}`);
  await page.getByRole("tab", { name: "Journal" }).click();
  const event = page.getByRole("button", { name: /^Select sequence 3:/ });
  await event.click();

  const dialog = page.getByRole("dialog", { name: "Event detail" });
  const controls = dialog.locator(
    "button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled)",
  );
  const first = controls.first();
  const last = controls.last();
  await expect(dialog).toBeFocused();

  await page.keyboard.press("Shift+Tab");
  await expect(last).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(first).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(last).toBeFocused();
  await page.keyboard.press("Escape");

  await expect(dialog).toHaveCount(0);
  await expect(event).toBeFocused();
});

test("uses Runs as the safe Back fallback for a directly opened run deep link", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/#/run/${smokeRunId}`);

  await page.getByRole("button", { name: "Back to runs" }).click();

  await expect(page.getByRole("heading", { name: "Runs", exact: true })).toBeVisible();
  await expect(page).toHaveURL(/#\/runs$/);
});

test("restores an unfocused pointer-activated run row and list scroll after browser back", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/runs?status=all");

  const row = page.getByRole("link", { name: `Open run ${smokeRunId}` });
  await page.locator(".portal-main").evaluate((element) => {
    const spacer = document.createElement("div");
    spacer.dataset.testScrollSpacer = "true";
    spacer.style.height = "600px";
    element.append(spacer);
    element.scrollTop = 180;
  });
  const scrollTop = await page.locator(".portal-main").evaluate((element) => element.scrollTop);
  expect(scrollTop).toBeGreaterThan(0);
  await row.evaluate((element) => {
    element.addEventListener("mousedown", (event) => event.preventDefault(), { once: true });
  });
  await row.click();
  await expect(page.getByRole("heading", { name: `Run ${smokeRunId}` })).toBeVisible();
  await expect
    .poll(() => page.locator(".portal-main").evaluate((element) => element.scrollTop))
    .toBe(0);

  await page.goBack();
  await expect(page.getByRole("heading", { name: "Runs", exact: true })).toBeVisible();
  await expect(row).toBeFocused();
  await expect
    .poll(() => page.locator(".portal-main").evaluate((element) => element.scrollTop))
    .toBe(scrollTop);
});

test("keeps the run event sheet usable at 200 percent page zoom", async ({ page }) => {
  await page.setViewportSize({ width: 640, height: 800 });
  await page.goto(`/#/run/${smokeRunId}`);
  await page.getByRole("tab", { name: "Journal" }).click();
  await page.getByRole("button", { name: /^Select sequence 3:/ }).click();
  const dialog = page.getByRole("dialog", { name: "Event detail" });
  await expect(dialog).toBeVisible();
  await page.evaluate(() => {
    document.documentElement.style.zoom = "2";
  });

  await expect(dialog.getByRole("button", { name: "Close event detail" })).toBeVisible();
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
});

for (const viewport of [
  { width: 320, height: 800 },
  { width: 390, height: 844 },
  { width: 430, height: 932 },
  { width: 667, height: 375 },
]) {
  test(`keeps instance detail routes usable at ${viewport.width}x${viewport.height}`, async ({
    page,
  }) => {
    await page.setViewportSize(viewport);
    for (const [path, heading] of [
      ["recovery", "Recovery metadata"],
      ["retention", "Telemetry retention"],
      ["warnings", "Configuration warnings"],
    ] as const) {
      await page.goto(`/#/instance/${path}`);
      await expect(page.getByRole("heading", { name: heading }).first()).toBeVisible();
      const back = page.getByRole("button", { name: "Back to overview" });
      const box = await back.boundingBox();
      expect(box?.width).toBeGreaterThanOrEqual(44);
      expect(box?.height).toBeGreaterThanOrEqual(44);
      await expect
        .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
        .toBeLessThanOrEqual(1);
    }
  });
}

test("shows loading and empty states on instance detail routes", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route("**/api/v1/instance", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    delete body.recoveryInventory;
    await new Promise((resolve) => setTimeout(resolve, 400));
    await route.fulfill({ response, json: body });
  });

  const navigation = page.goto("/#/instance/recovery");
  await expect(page.getByRole("heading", { name: "Loading recovery metadata" })).toBeVisible();
  await navigation;
  await expect(page.getByRole("heading", { name: "No recovery metadata reported" })).toBeVisible();
});

for (const failure of [
  { name: "permission", status: 403, heading: "Access denied" },
  { name: "unavailable", status: 503, heading: "Couldn't load Goobers data" },
] as const) {
  test(`shows an explicit ${failure.name} state on detail routes`, async ({ page }) => {
    await page.route("**/api/v1/instance", (route) =>
      route.fulfill({
        body: JSON.stringify({ code: "fixture_error", message: failure.name }),
        contentType: "application/json",
        status: failure.status,
      }),
    );
    await page.goto("/#/instance/recovery");
    await expect(page.getByRole("heading", { name: failure.heading })).toBeVisible();
  });
}

test("shows an older-daemon state on detail routes", async ({ page }) => {
  await page.route("**/api/v1/instance", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    body.apiVersion = "v0";
    await route.fulfill({ response, json: body });
  });
  await page.goto("/#/instance/retention");
  await expect(page.getByRole("heading", { name: "Daemon update required" })).toBeVisible();
});

test("keeps diagnostics and supported action failures explicit on a phone", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`/#/run/${smokeRunId}?tab=diagnostics`);
  await expect(page.locator("aside.run-inspector")).toBeVisible();
  await page.getByRole("button", { name: "Reveal run files" }).click();
  await expect(page.getByRole("alert")).toContainText(/not found|could not|failed|malformed/i);
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
});

test("restores the originating instance summary focus and scroll", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/overview");
  const origin = page.getByRole("link", { name: "View recovery metadata" });
  await origin.scrollIntoViewIfNeeded();
  await origin.focus();
  const scrollTop = await page.locator(".portal-main").evaluate((element) => element.scrollTop);
  expect(scrollTop).toBeGreaterThan(0);

  await origin.evaluate((link) => (link as HTMLAnchorElement).click());
  await expect(
    page.getByRole("heading", { name: "Recovery metadata", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Back to overview" }).click();

  await expect(page.getByRole("heading", { name: /healthy|attention/i }).first()).toBeVisible();
  await expect(origin).toBeFocused();
  await expect
    .poll(() => page.locator(".portal-main").evaluate((element) => element.scrollTop))
    .toBe(scrollTop);
});

test("keeps overview instance detail entry links at least 44 by 44 pixels", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/overview");

  const links = page.locator('a.instance-warning-link[href^="#/instance/"]');
  await expect(links).toHaveCount(3);
  for (const link of await links.all()) {
    const box = await link.boundingBox();
    expect(box?.width).toBeGreaterThanOrEqual(44);
    expect(box?.height).toBeGreaterThanOrEqual(44);
  }
});

test("does not mark unrelated history after a canceled detail-link click", async ({ page }) => {
  await page.goto("/#/overview");
  const origin = page.getByRole("link", { name: "View recovery metadata" });
  await origin.evaluate((link) => {
    link.addEventListener("click", (event) => event.preventDefault(), { once: true });
    link.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true }));
  });
  await expect(page).toHaveURL(/#\/overview$/);

  await page.evaluate(() => {
    window.location.hash = "#/instance/retention";
  });
  await expect(page.getByRole("heading", { name: "Telemetry retention", level: 1 })).toBeVisible();
  expect(await page.evaluate(() => window.history.state?.portalOrigin)).not.toBe(true);

  await page.goBack();
  await expect(page).toHaveURL(/#\/overview$/);
  await page.goForward();
  await expect(page.getByRole("heading", { name: "Telemetry retention", level: 1 })).toBeVisible();
});

test("keeps instance details usable on desktop and at 200 percent zoom", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/#/instance/retention");
  await expect(page.getByRole("heading", { name: "Telemetry retention", level: 1 })).toBeVisible();
  await page.evaluate(() => {
    document.documentElement.style.zoom = "2";
  });
  await expect(page.getByRole("button", { name: "Back to overview" })).toBeVisible();
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
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

test("bounds route reads when daemon admission is constrained", async ({ page }, testInfo) => {
  const headers = {
    "x-test-constrained-admission": `${testInfo.testId}:${testInfo.repeatEachIndex}:${testInfo.workerIndex}`,
  };
  await page.route("**/api/v1/**", async (route) => {
    if (new URL(route.request().url()).pathname.startsWith("/api/v1/test/")) {
      await route.continue();
      return;
    }
    await route.continue({
      headers: {
        ...route.request().headers(),
        ...headers,
      },
    });
  });
  const enabled = await page.request.post("/api/v1/test/admission", { headers });
  expect(enabled.ok()).toBe(true);

  await page.goto("/#/overview");
  await page.getByRole("button", { name: "Workflows" }).click();
  await page.getByRole("button", { name: "Runs" }).click();
  await page.getByRole("button", { name: "Insight" }).click();
  await page.getByRole("button", { name: "Cost" }).click();
  await expect(page.getByRole("heading", { name: "Cost", exact: true })).toBeVisible();

  const result = await page.request.get("/api/v1/test/admission", { headers });
  const stats = (await result.json()) as { peak: number; requests: number };
  expect(stats.peak).toBeLessThanOrEqual(1);
  expect(stats.requests).toBeLessThanOrEqual(30);
});

test("keeps the shared shell deliberate and accessible at 320px", async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 320, height: 800 });
  await page.goto("/#/overview");
  await expect(page.getByRole("heading", { name: "Active runs" })).toBeVisible();

  const primary = page.getByRole("navigation", { name: "Mobile primary", exact: true });
  const more = primary.getByRole("button", { name: "Open navigation menu" });
  await expect(more).toBeVisible();
  await expect(more).toHaveText("Overview");
  await more.click();
  const dialog = page.getByRole("dialog", { name: "Goobers" });
  for (const name of ["Overview", "Workflows", "Runs", "Goobers", "Work Items", "Insight", "Cost"]) {
    await expect(dialog.getByRole("button", { name })).toBeVisible();
  }
  await expect(page.getByRole("navigation", { name: "Gaggles" })).toBeVisible();
  const support = page.getByRole("navigation", { name: "Support" });
  await expect(support).toBeVisible();
  await expect(support.getByRole("link", { name: "Docs" })).toBeVisible();

  await expect(dialog.getByRole("button", { name: "Use dark theme" })).toHaveCount(0);
  const close = dialog.getByRole("button", { name: "Close portal menu" });
  for (const control of [more, close]) {
    const box = await control.boundingBox();
    expect(box, "representative shell control should have geometry").not.toBeNull();
    expect(box!.width).toBeGreaterThanOrEqual(24);
    expect(box!.height).toBeGreaterThanOrEqual(24);
  }

  await dialog.getByRole("button", { name: "Cost" }).click();
  await expect(page.getByRole("heading", { name: "Cost", exact: true })).toBeVisible();
  await expect(more).toHaveText("Cost");

  for (const control of [
    page.getByLabel("Scope"),
    page.getByLabel("Time window"),
  ]) {
    const box = await control.boundingBox();
    expect(box, "representative page control should have geometry").not.toBeNull();
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
  await expect(page).toHaveURL(/#\/cost\?gaggle=core&workflow=implementation$/);
  await expect(page.getByRole("heading", { name: "Cost", exact: true })).toBeVisible();
  await expect(page.getByLabel("Scope")).toHaveText("Workflow · core / implementation");
});

test("shows one coherent polling fallback status with diagnostics out of primary copy", async ({
  page,
}) => {
  await page.route("**/api/v1/events*", async (route) => route.abort("failed"));
  await page.goto("/#/overview");

  const status = page.getByText("Data current via polling", { exact: true });
  await expect(status).toBeVisible({ timeout: 10_000 });
  await expect(status).not.toContainText("stream-error");
  await expect(status).not.toHaveAttribute("title", /.+/);

  const detailsButton = page.getByRole("button", { name: /Show live update details/ });
  await detailsButton.focus();
  const details = page.locator("#live-updates-tooltip");
  await expect(details).toBeVisible();
  await expect(details).toContainText("stream-error");
  await expect(details).toContainText("/api/v1/events");
  await expect(page.locator('[data-state="polling-fallback"]')).toHaveCount(1);
});
