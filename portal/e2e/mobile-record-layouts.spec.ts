import { expect, test, type Locator, type Page } from "@playwright/test";

const compactViewports = [
  { width: 320, height: 800 },
  { width: 390, height: 844 },
  { width: 430, height: 932 },
  { width: 844, height: 390 },
];

async function expectNoDocumentOverflow(page: Page) {
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
}

async function expectTouchTarget(locator: Locator) {
  const box = await locator.boundingBox();
  expect(box).not.toBeNull();
  expect(box!.width).toBeGreaterThanOrEqual(44);
  expect(box!.height).toBeGreaterThanOrEqual(44);
}

for (const viewport of compactViewports) {
  test(`keeps workflow and goober records usable at ${viewport.width}x${viewport.height}`, async ({
    page,
  }) => {
    await page.setViewportSize(viewport);
    await page.goto("/#/workflows");

    await expect(page.getByRole("heading", { name: "Workflows", exact: true })).toBeVisible();
    const workflowGroup = page.getByRole("button", { name: /Core product/ });
    await expect(workflowGroup).toHaveAttribute("aria-expanded", "true");
    const workflow = page.getByRole("region", {
      name: "Core product workflow definitions",
    });
    await expect(workflow.getByText("Implementation", { exact: true })).toBeVisible();
    await expect(workflow.getByTitle("core/implementation")).toBeVisible();
    await expect(workflow.getByRole("link", { name: "Details" })).toBeVisible();
    await expectTouchTarget(workflow.getByRole("link", { name: "Details" }));
    const scopeActions = page.locator(".scope-pivot-link:visible");
    expect(await scopeActions.count()).toBeGreaterThan(0);
    for (const action of await scopeActions.all()) {
      await expectTouchTarget(action);
    }
    const workflowBox = await workflow.getByText("Implementation", { exact: true }).boundingBox();
    expect(workflowBox).not.toBeNull();
    expect(workflowBox!.y).toBeLessThan(viewport.height);
    await expectNoDocumentOverflow(page);

    await workflow.getByRole("link", { name: "Details" }).click();
    await expect(page).toHaveURL(/#\/workflow\/core\/implementation$/);
    await page.goBack();
    await expect(page).toHaveURL(/#\/workflows$/);

    await page.goto("/#/goobers");
    await expect(page.getByRole("heading", { name: "Goobers", exact: true })).toBeVisible();
    const gooberGroup = page.getByRole("button", { name: /Core product/ });
    await expect(gooberGroup).toHaveAttribute("aria-expanded", "true");
    const goober = page.getByRole("button", { name: /Core implementer/ });
    await expect(goober).toContainText("configured");
    await expect(goober.getByTitle("core/implementer")).toBeVisible();
    await expectTouchTarget(goober);
    const gooberBox = await goober.boundingBox();
    expect(gooberBox).not.toBeNull();
    expect(gooberBox!.y).toBeLessThan(viewport.height);
    await expectNoDocumentOverflow(page);
  });
}

test("keeps long records bounded at 200% zoom and with a large inventory", async ({ page }) => {
  await page.setViewportSize({ width: 640, height: 900 });
  await page.route("**/api/v1/gaggles/core/workflows*", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    const source = body.items[0];
    const items = Array.from({ length: 80 }, (_, index) => ({
      ...source,
      identity: {
        gaggle: "core",
        name: `workflow-${index}-with-an-extraordinarily-long-identifier-that-must-wrap`,
      },
      displayName: `Workflow ${index} with an extraordinarily long display name`,
      purpose: "A long purpose that remains available without widening the document.",
    }));
    await route.fulfill({ response, json: { ...body, items, page: { ...body.page, total: 80 } } });
  });

  await page.goto("/#/workflows");
  await page.evaluate(() => {
    document.body.style.zoom = "200%";
    document.body.style.width = "50%";
  });

  await expect(page.getByText("Workflow 0 with an extraordinarily long display name")).toBeVisible();
  await expect(
    page.getByTitle("core/workflow-0-with-an-extraordinarily-long-identifier-that-must-wrap"),
  ).toBeVisible();
  await expectNoDocumentOverflow(page);

  await page.evaluate(() => {
    document.body.style.zoom = "";
    document.body.style.width = "";
  });
  await expect(page.getByText("Workflow 79 with an extraordinarily long display name")).toBeAttached();
  await expectNoDocumentOverflow(page);
});

test("preserves the desktop record disclosures and detail navigation", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/#/workflows");

  const workflowGroup = page.getByRole("button", { name: /Core product/ });
  await expect(workflowGroup).toHaveAttribute("aria-expanded", "false");
  await workflowGroup.click();
  await expect(
    page.getByRole("region", { name: "Core product workflow definitions" }),
  ).toContainText("Implementation");

  await page.goto("/#/goobers");
  const gooberGroup = page.getByRole("button", { name: /Core product/ });
  await expect(gooberGroup).toHaveAttribute("aria-expanded", "false");
  await gooberGroup.click();
  await expect(page.getByRole("button", { name: /Core implementer/ })).toBeVisible();
  await expectNoDocumentOverflow(page);
});

test("reveals records when a mounted desktop page changes to a compact viewport", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/#/workflows");

  const workflowGroup = page.getByRole("button", { name: /Core product/ });
  await expect(workflowGroup).toHaveAttribute("aria-expanded", "false");

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(workflowGroup).toHaveAttribute("aria-expanded", "true");
  await expect(page.getByText("Implementation", { exact: true })).toBeVisible();

  await workflowGroup.click();
  await expect(workflowGroup).toHaveAttribute("aria-expanded", "false");
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(workflowGroup).toHaveAttribute("aria-expanded", "false");
const routes = [
  {
    path: "/#/runs",
    heading: "Runs",
    record: (page: Page) => page.getByRole("link", { name: /^Open run / }).first(),
  },
  {
    path: "/#/work-items",
    heading: "Work Items",
    record: (page: Page) => page.getByRole("button", { name: /^Open PR / }).first(),
  },
] as const;

const viewports = [
  { name: "320px phone", width: 320, height: 844 },
  { name: "390px phone", width: 390, height: 844 },
  { name: "430px phone", width: 430, height: 932 },
  { name: "phone landscape", width: 844, height: 390 },
  { name: "200% zoom equivalent", width: 390, height: 422 },
] as const;

for (const viewport of viewports) {
  test(`keeps Runs and Work Items usable at ${viewport.name}`, async ({ page }) => {
    await page.setViewportSize(viewport);

    for (const route of routes) {
      await page.goto(route.path);

      await expect(page.getByRole("heading", { name: route.heading, exact: true })).toBeVisible();
      const record = route.record(page);
      await expect(record).toBeVisible();
      const box = await record.boundingBox();
      expect(box, `${route.heading} record should have geometry`).not.toBeNull();
      if (viewport.height >= 844) {
        expect(box!.y).toBeLessThan(viewport.height);
      }
      await expect
        .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
        .toBeLessThanOrEqual(1);
    }
  });
}

test("shows semantic mobile context and preserves deep links and browser Back", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });

  await page.goto("/#/runs");
  const run = page.getByRole("link", { name: /^Open run / }).first();
  await expect(run.locator(".status-badge")).toBeVisible();
  await expect(run.locator(".run-current-stage")).toBeVisible();
  await run.click();
  await expect(page).toHaveURL(/#\/run\//);
  await page.goBack();
  await expect(page.getByRole("heading", { name: "Runs", exact: true })).toBeVisible();

  await page.goto("/#/work-items");
  const workItem = page.getByRole("button", { name: /^Open PR / }).first();
  await expect(workItem.locator(".work-item-status")).toContainText("Running");
  await expect(workItem.locator(".work-item-mobile-context")).toContainText(
    "core / implementation",
  );
  await workItem.click();
  await expect(page).toHaveURL(/#\/work-items\/github\/Agent-Clubhouse\/Goobers\/pr\/4800$/);
  await page.goBack();
  await expect(page.getByRole("heading", { name: "Work Items", exact: true })).toBeVisible();
});

test("renders a bounded large Work Items dataset without losing the first record", async ({
  page,
}) => {
  const items = Array.from({ length: 200 }, (_, index) => ({
    provider: "github",
    repository: `organization/repository-with-a-long-name-${index}`,
    kind: "issue",
    externalId: `${10_000 + index}`,
    actionCount: index + 1,
    lastOperation: "comment",
    lastActionAt: "2026-09-10T08:02:00Z",
    lastRunId: `run-${index}`,
    gaggle: "core",
    workflow: "implementation",
    runStatus: index === 0 ? "running" : "completed",
  }));
  await page.route("**/api/v1/work-items?**", (route) =>
    route.fulfill({ json: { items, hasMore: true } }));
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/work-items");

  const first = page.getByRole("button", {
    name: "Open issue #10000 in organization/repository-with-a-long-name-0",
  });
  await expect(first).toBeVisible();
  await expect(first.locator(".work-item-status")).toContainText("Running");
  await expect(page.getByText("Showing the 200 most recently actioned work items.")).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
});

test("retains desktop record columns", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });

  await page.goto("/#/runs");
  await expect(page.locator(".all-runs-grid.data-table-header")).toBeVisible();
  await expect(page.locator(".all-runs-grid.data-table-header")).toContainText("Duration");

  await page.goto("/#/work-items");
  await expect(page.locator(".work-item-grid.data-table-header")).toBeVisible();
  await expect(page.locator(".work-item-grid.data-table-header")).toContainText("Actions");
  await expect(page.locator(".work-item-mobile-context").first()).toBeHidden();
});
