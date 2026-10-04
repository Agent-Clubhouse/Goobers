import { expect, test, type Page } from "@playwright/test";

async function chooseDraftScope(page: Page, name: string) {
  const dialog = page.getByRole("dialog", { name: "Filters", exact: true });
  await dialog.getByRole("button", { name: "Draft scope", exact: true }).click();
  await dialog.getByRole("button", { name, exact: true }).click();
}

const mobileViewports = [
  { width: 320, height: 800 },
  { width: 390, height: 844 },
  { width: 430, height: 860 },
  { width: 844, height: 390 },
];

for (const width of [1440, 390]) {
  test(`shares heading and toolbar content spacing at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    for (const [route, title, selector] of [
      ["workflows", "Workflows", ".workflow-gaggle-group"],
      ["goobers", "Goobers", ".goober-groups"],
      ["runs", "Runs", ".content-section .data-table"],
      ["work-items", "Work Items", ".work-items-table"],
    ]) {
      await page.goto(`/#/${route}`);
      const heading = page.getByRole("heading", { name: title, exact: true, level: 1 });
      const content = page.locator(selector).first();
      await expect(heading).toBeVisible();
      await expect(content).toBeVisible();
      for (const gap of [16, 24]) {
        await page.evaluate((value) => {
          document.documentElement.style.setProperty("--space-page-content", `${value}px`);
        }, gap);
        await expect.poll(async () => {
          const header = await heading.locator("xpath=ancestor::header").boundingBox();
          const box = await content.boundingBox();
          if (!header || !box) throw new Error("Expected visible page heading and content.");
          return Math.abs(box.y - header.y - header.height - gap);
        }).toBeLessThanOrEqual(1);
      }
      await page.evaluate(() => document.documentElement.style.removeProperty("--space-page-content"));
    }
  });
}

test("aligns scope and time-window fields in the shared control group", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  for (const route of ["insight", "cost"]) {
    await page.goto(`/#/${route}`);
    const scope = page.getByRole("button", { name: "Scope", exact: true });
    const time = page.getByRole("combobox", { name: "Time window" });
    await expect(scope).toBeVisible();
    await expect(time).toBeVisible();
    const scopeBox = await scope.boundingBox();
    const timeBox = await time.boundingBox();
    if (!scopeBox || !timeBox) throw new Error("Expected visible scope and time controls.");
    expect(Math.abs(scopeBox.y - timeBox.y)).toBeLessThanOrEqual(1);
    expect(Math.abs(scopeBox.height - timeBox.height)).toBeLessThanOrEqual(1);
  }
});

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

test("isolates mobile filter drafts and supports dismissal, scope changes, and Back", async ({
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
  await chooseDraftScope(page, "Gaggle · core");
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  await expect(page).toHaveURL(/#\/runs\?status=all$/);
  await expect(trigger).toBeFocused();

  await trigger.click();
  await chooseDraftScope(page, "Gaggle · core");
  await dialog.getByRole("button", { name: "Apply filters" }).click();
  await expect(page).toHaveURL(/#\/runs\?gaggle=core&status=all$/);
  const scope = page.getByRole("button", { name: "Scope", exact: true });
  await expect(scope).toContainText("Gaggle · core");
  await scope.click();
  await page.getByRole("button", { name: "Instance", exact: true }).click();
  await expect(page).toHaveURL(/#\/runs\?status=all$/);
  await expect(scope).toContainText("Instance");
  await expect(page.getByRole("region", { name: "Run history" })).toBeVisible();

  await trigger.click();
  await chooseDraftScope(page, "Gaggle · core");
  await dialog.getByRole("button", { name: "Apply filters" }).click();
  await expect(page).toHaveURL(/#\/runs\?gaggle=core&status=all$/);

  await trigger.click();
  await page.goBack();
  await expect(dialog).toBeHidden();
  await expect(trigger).toBeFocused();

  await trigger.click();
  await chooseDraftScope(page, "Instance");
  await dialog.getByRole("button", { name: "All runs", exact: true }).click();
  await dialog.getByRole("button", { name: "Apply filters" }).click();
  await expect(page).toHaveURL(/#\/runs$/);
  await expect(page.getByRole("region", { name: "Run history" })).toBeVisible();
});

test("applies, edits, and clears advanced Runs sheet filters", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/runs?status=all");

  await page.getByRole("button", { name: "Filters", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Filters" });
  await dialog.getByLabel("Draft stage filter").fill("review");
  await dialog.getByLabel("Draft outcome filter").selectOption("failure");
  await dialog.getByLabel("Draft population filter").selectOption("attempts");
  await dialog.getByLabel("Draft since filter").fill("2026-07-18T00:00:00Z");
  await dialog.getByLabel("Draft until filter").fill("2026-07-19T00:00:00Z");
  await dialog.getByLabel("Draft time window filter").selectOption("24h");
  await dialog.getByRole("button", { name: "Apply filters" }).click();

  await expect(page).toHaveURL(
    /#\/runs\?stage=review&outcome=failure&population=attempts&since=2026-07-18T00%3A00%3A00Z&until=2026-07-19T00%3A00%3A00Z&window=24h&status=all$/,
  );
  await expect(page.getByLabel("7 active filters")).toBeVisible();
  await page.getByRole("button", { name: "Filters", exact: true }).click();
  await expect(dialog.getByLabel("Draft stage filter")).toHaveValue("review");
  await dialog.getByLabel("Draft stage filter").fill("");
  await dialog.getByRole("button", { name: "Apply filters" }).click();
  await expect(page).not.toHaveURL(/stage=review/);
  await page.getByRole("button", { name: "Filters", exact: true }).click();
  await dialog.getByRole("button", { name: "All runs", exact: true }).click();
  await dialog.getByLabel("Draft outcome filter").selectOption("");
  await dialog.getByLabel("Draft population filter").selectOption("");
  await dialog.getByLabel("Draft since filter").fill("");
  await dialog.getByLabel("Draft until filter").fill("");
  await dialog.getByLabel("Draft time window filter").selectOption("");
  await dialog.getByRole("button", { name: "Apply filters" }).click();
  await expect(page).toHaveURL(/#\/runs$/);
});

test("consumes filter sheet history before applying filters", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/overview");
  await page.getByRole("button", { name: "Open navigation menu" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Runs", exact: true }).click();
  await expect(page).toHaveURL(/#\/runs$/);

  await page.getByRole("button", { name: "Filters", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Filters" });
  await chooseDraftScope(page, "Gaggle · core");
  await dialog.getByRole("button", { name: "Apply filters" }).click();
  await expect(page).toHaveURL(/#\/runs\?gaggle=core$/);

  await page.goBack();
  await expect(page).toHaveURL(/#\/runs$/);
  await page.goBack();
  await expect(page).toHaveURL(/#\/overview$/);
});

test("cancels invalid-route sheet edits without changing the route", async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 800 });
  await page.goto("/#/runs?status=surprising");

  await page.getByRole("button", { name: "Filters", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Filters" });
  await dialog.getByRole("button", { name: "complete" }).click();
  await expect(page).toHaveURL(/#\/runs\?status=surprising$/);
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();

  await expect(dialog).toBeHidden();
  await expect(page).toHaveURL(/#\/runs\?status=surprising$/);
});

test("reflows controls at an equivalent 200% browser zoom", async ({ page }) => {
  await page.setViewportSize({ width: 780, height: 844 });
  await page.goto("/#/work-items");

  await page.setViewportSize({ width: 390, height: 422 });
  const trigger = page.getByRole("button", { name: "Filters", exact: true });
  const search = page.getByRole("searchbox", { name: "Search work items" });
  await expect(trigger).toBeVisible();
  await expect(search).toBeVisible();
  await trigger.focus();
  await expect(trigger).toBeFocused();
  expect(await page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);

  await trigger.click();
  const dialog = page.getByRole("dialog", { name: "Filters" });
  await expect(dialog.getByRole("button", { name: "Cancel filter changes" })).toBeFocused();
  await expect(dialog.getByRole("button", { name: "Apply filters" })).toBeVisible();
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
    "Loading...",
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
