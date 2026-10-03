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
});
