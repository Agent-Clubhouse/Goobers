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
  await page.goto("/#/runs?gaggle=core&window=24h");

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

test("composes one compact host header without duplicating Portal chrome", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
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

test("retains the desktop sidebar and header layout", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/#/overview");

  await expect(page.getByRole("navigation", { name: "Primary" })).toBeVisible();
  await expect(page.getByRole("navigation", { name: "Mobile primary" })).toBeHidden();
  await expect(page.locator(".topbar-brand")).toBeVisible();
  await expect(page.locator(".sidebar")).toBeVisible();
});
