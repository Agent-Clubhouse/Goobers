import { expect, test, type Locator, type Page } from "@playwright/test";

const phoneLayouts = [
  { name: "320x568", width: 320, height: 568 },
  { name: "390x844", width: 390, height: 844 },
  { name: "430x932", width: 430, height: 932 },
  { name: "844x390 landscape", width: 844, height: 390 },
  { name: "200% zoom reflow", width: 195, height: 422 },
] as const;

async function expectTouchTarget(locator: Locator) {
  const box = await locator.boundingBox();
  expect(box).not.toBeNull();
  expect(box!.width).toBeGreaterThanOrEqual(44);
  expect(box!.height).toBeGreaterThanOrEqual(44);
}

async function expectNoDocumentOverflow(page: Page) {
  await expect
    .poll(() => page.evaluate(() => document.documentElement.scrollWidth - innerWidth))
    .toBeLessThanOrEqual(1);
}

for (const layout of phoneLayouts) {
  test(`keeps the standalone shell content-first at ${layout.name}`, async ({ page }) => {
    await page.setViewportSize(layout);
    await page.goto("/#/overview");

    const heading = page.getByRole("main").getByRole("heading").first();
    await expect(heading).toBeVisible();
    const headingBox = await heading.boundingBox();
    expect(headingBox).not.toBeNull();
    expect(headingBox!.y).toBeLessThanOrEqual(88);

    const navigation = page.getByRole("navigation", { name: "Mobile primary" });
    const navigationBox = await navigation.boundingBox();
    expect(navigationBox).not.toBeNull();
    expect(navigationBox!.height).toBeLessThanOrEqual(64);
    for (const button of await navigation.getByRole("button").all()) {
      await expectTouchTarget(button);
    }
    await expect(page.locator(".topbar-instance-name")).toBeVisible();
    await expect(page.locator(".mobile-live-status")).toHaveAttribute("role", "status");
    await expectNoDocumentOverflow(page);
  });
}

test("reaches primary and secondary destinations without losing scope", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/overview");
  await page.evaluate(() => {
    window.location.hash = "#/runs?gaggle=core&window=24h";
  });
  await expect(page).toHaveURL(/#\/runs\?gaggle=core&window=24h$/);

  const navigation = page.getByRole("navigation", { name: "Mobile primary" });
  await navigation.getByRole("button", { name: "Workflows" }).click();
  await expect(page).toHaveURL(/#\/workflows$/);
  await expect(page.getByRole("heading", { name: "Workflows", exact: true })).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(/#\/runs\?gaggle=core&window=24h$/);

  const more = navigation.getByRole("button", { name: "More" });
  await more.click();
  const dialog = page.getByRole("dialog", { name: "Goobers" });
  await expect(dialog).toBeVisible();
  await dialog.getByRole("button", { name: "Insight" }).click();
  await expect(page).toHaveURL(/#\/insight\?gaggle=core&window=24h$/);
  await expect(page.getByRole("heading", { name: "Insight", exact: true })).toBeVisible();
  await expect(more).toHaveAttribute("aria-current", "page");

  await more.click();
  await expect(dialog.getByRole("link", { name: "Open gaggle Core product" })).toBeVisible();
  await expect(dialog.getByRole("button", { name: /Use (dark|light) theme/ })).toBeVisible();
  await expect(dialog.getByLabel("Portal details")).toContainText("e2e-fixture");
  await page.keyboard.press("Escape");

  await page.goBack();
  await expect(page).toHaveURL(/#\/runs\?gaggle=core&window=24h$/);
  await page.goBack();
  await expect(page).toHaveURL(/#\/overview$/);
});

test("dismisses the menu with Escape and browser Back and restores focus", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/overview");

  const more = page
    .getByRole("navigation", { name: "Mobile primary" })
    .getByRole("button", { name: "More" });
  await more.focus();
  await more.press("Enter");
  await expect(page.getByRole("dialog", { name: "Goobers" })).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog", { name: "Goobers" })).toBeHidden();
  await expect(more).toBeFocused();
  await expect(page).toHaveURL(/#\/overview$/);

  await more.click();
  await page.goBack();
  await expect(page.getByRole("dialog", { name: "Goobers" })).toBeHidden();
  await expect(more).toBeFocused();
  await expect(page).toHaveURL(/#\/overview$/);
});

test("contains focus within the menu and keeps focused controls visible above a keyboard", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/overview");

  await page
    .getByRole("navigation", { name: "Mobile primary" })
    .getByRole("button", { name: "More" })
    .click();
  const dialog = page.getByRole("dialog", { name: "Goobers" });
  const focusable = dialog.locator(
    'button:not([disabled]), a[href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  );
  const first = focusable.first();
  const last = focusable.last();

  await first.focus();
  await page.keyboard.press("Shift+Tab");
  await expect(last).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(first).toBeFocused();
  const focusableCount = await focusable.count();
  for (let index = 0; index <= focusableCount; index += 1) {
    await page.keyboard.press("Tab");
    expect(await dialog.evaluate((element) =>
      element === document.activeElement || element.contains(document.activeElement),
    )).toBe(true);
  }

  await page.setViewportSize({ width: 390, height: 430 });
  await last.focus();
  await expect
    .poll(async () => {
      const box = await last.boundingBox();
      return box ? box.y + box.height : Number.POSITIVE_INFINITY;
    })
    .toBeLessThanOrEqual(430);
});

test("consumes the menu history entry when leaving compact mode", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/overview");
  await page
    .getByRole("navigation", { name: "Mobile primary" })
    .getByRole("button", { name: "Runs" })
    .click();
  await expect(page).toHaveURL(/#\/runs$/);
  await page
    .getByRole("navigation", { name: "Mobile primary" })
    .getByRole("button", { name: "More" })
    .click();

  await page.setViewportSize({ width: 1024, height: 768 });
  await expect(page.getByRole("dialog", { name: "Goobers" })).toHaveCount(0);
  await expect.poll(() => page.evaluate(() => window.history.state?.portalMenu)).toBeUndefined();
  await page.goBack();
  await expect(page).toHaveURL(/#\/overview$/);
});

for (const layout of phoneLayouts.slice(0, 2)) {
  test(`composes one compact host header at ${layout.name}`, async ({ page }) => {
    await page.setViewportSize(layout);
    await page.goto("/?host=fleet#/overview");

    await expect(page.locator("#portal-header-host .topbar")).toHaveCount(1);
    await expect(page.locator("#root > .portal-frame > .topbar")).toHaveCount(0);
    const heading = page.getByRole("main").getByRole("heading").first();
    await expect(heading).toBeVisible();
    const headingBox = await heading.boundingBox();
    expect(headingBox).not.toBeNull();
    expect(headingBox!.y).toBeLessThanOrEqual(136);
    await expect(page.getByRole("navigation", { name: "Mobile primary" })).toHaveCount(1);
    await expectNoDocumentOverflow(page);
  });
}

test("defers compact navigation to an embedding host without duplication", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/?host=fleet&hostNavigation=true#/overview");

  await expect(page.getByRole("navigation", { name: "Host mobile primary" })).toHaveCount(1);
  await expect(page.getByRole("navigation", { name: "Mobile primary", exact: true })).toHaveCount(0);
  await expect(page.locator(".mobile-primary-nav")).toHaveCount(1);
});

test("retains the desktop sidebar and header layout", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/#/overview");

  await expect(page.getByRole("navigation", { name: "Primary" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Mobile primary" })).toBeHidden();
  await expect(page.locator(".topbar-brand")).toBeVisible();
  await expect(page.locator(".sidebar")).toBeVisible();
});
