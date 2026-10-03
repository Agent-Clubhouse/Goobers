import { expect, test } from "@playwright/test";

const mobileViewports = [
  { width: 320, height: 800 },
  { width: 390, height: 844 },
  { width: 430, height: 860 },
  { width: 844, height: 390 },
];

for (const viewport of mobileViewports) {
  test(`keeps content controls compact at ${viewport.width}x${viewport.height}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await page.goto("/#/work-items");

    await expect(page.getByRole("heading", { name: "Work Items" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Filters", exact: true })).toBeVisible();
    await expect(page.getByRole("searchbox", { name: "Search work items" })).toBeVisible();
    const firstRecord = page.locator(".work-items-table .data-row").first();
    await expect(firstRecord).toBeVisible();
    expect((await firstRecord.boundingBox())!.y).toBeLessThan(viewport.height);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
      .toBeLessThanOrEqual(1);

    for (const control of [
      page.getByRole("button", { name: "Filters", exact: true }),
      page.getByRole("searchbox", { name: "Search work items" }),
    ]) {
      const box = await control.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.height).toBeGreaterThanOrEqual(44);
      expect(box!.width).toBeGreaterThanOrEqual(44);
    }
  });
}

test("isolates mobile filter drafts and supports dismissal, chips, reset, and Back", async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/runs?status=all");

  const trigger = page.getByRole("button", { name: "Filters", exact: true });
  await trigger.click();
  const dialog = page.getByRole("dialog", { name: "Filters" });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Cancel filter changes" })).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(dialog.getByRole("button", { name: "Apply filters" })).toBeFocused();
  await dialog.getByLabel("Draft gaggle filter").selectOption("core");
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(page).toHaveURL(/#\/runs\?status=all$/);
  await expect(trigger).toBeFocused();

  await trigger.click();
  await dialog.getByLabel("Draft gaggle filter").selectOption("core");
  await dialog.getByRole("button", { name: "Apply filters" }).click();
  await expect(page).toHaveURL(/#\/runs\?gaggle=core&status=all$/);
  await expect(page.getByRole("button", { name: "Remove Gaggle: core filter" })).toBeVisible();

  await trigger.click();
  await page.goBack();
  await expect(dialog).toBeHidden();
  await expect(trigger).toBeFocused();

  await page.getByRole("button", { name: "Reset filters" }).click();
  await expect(page).toHaveURL(/#\/runs$/);
  await expect(page.getByRole("region", { name: "Run history" })).toBeVisible();

  const session = await page.context().newCDPSession(page);
  await session.send("Emulation.setPageScaleFactor", { pageScaleFactor: 2 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
});

test("shows invalid filters explicitly and preserves the desktop controls", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/#/runs?status=invalid");

  await expect(page.getByRole("alert")).toContainText('Invalid status filter "invalid"');
  await expect(page.getByRole("group", { name: "Filter runs" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Filters", exact: true })).toBeHidden();
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
});

test("keeps loading, empty, and error states bounded on a narrow screen", async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 800 });
  const workItemsEndpoint = /\/api\/v1\/work-items(?:\?|$)/;
  let releaseWorkItems: (() => void) | undefined;
  const release = new Promise<void>((resolve) => {
    releaseWorkItems = resolve;
  });
  await page.route(workItemsEndpoint, async (route) => {
    await release;
    await route.continue();
  });

  await page.goto("/#/work-items");
  await expect(page.locator(".daemon-state[role='status']")).toContainText(
    "Connecting to Goobers Instance",
  );
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
  releaseWorkItems?.();

  const search = page.getByRole("searchbox", { name: "Search work items" });
  await expect(search).toBeVisible();
  await page.unroute(workItemsEndpoint);
  await search.fill("no-such-work-item");
  await expect(page.getByText(/No confirmed provider actions match this filter/)).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);

  await page.route(workItemsEndpoint, async (route) => {
    await route.fulfill({
      body: JSON.stringify({ code: "test.error", message: "Unavailable for test" }),
      contentType: "application/json",
      status: 500,
    });
  });
  await page.reload();
  await expect(page.getByRole("heading", { name: "Couldn't load Goobers data" })).toBeVisible();
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
});
