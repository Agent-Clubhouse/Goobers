import { expect, test } from "@playwright/test";

const parent = "01JZE2ESMOKERUN";
const child = `child-${"a".repeat(64)}`;

for (const width of [1280, 390]) {
  test(`recorded child activity links stay usable at ${width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/api/v1/runs/**", async (route) => {
      const url = new URL(route.request().url());
      const isChild = url.pathname.includes(child);
      url.pathname = url.pathname.replace(child, parent);
      const response = await route.fetch({ url: url.href });
      if (url.pathname === `/api/v1/runs/${parent}`) {
        const detail = await response.json();
        detail.id = isChild ? child : parent;
        detail.childActivity = isChild
          ? { status: "recorded", parked: false, waits: [], parent: { runId: parent, workflow: "Implementation", stageOccurrence: "inspect/branch0/visit1" } }
          : { status: "recorded", parked: false, waits: [{ runId: child, stage: "inspect", branch: 1, action: "wait", since: "2026-10-09T15:00:00Z", sequence: 7 }] };
        await route.fulfill({ response, json: detail });
      } else if (isChild && url.pathname.endsWith("/events")) {
        const events = await response.json();
        events.runId = child;
        await route.fulfill({ response, json: events });
      } else {
        await route.fulfill({ response });
      }
    });
    await page.goto(`/#/run/${parent}`);
    const panel = page.getByRole("region", { name: "Child workflows" });
    await expect(panel).toContainText("These stages are waiting on child workflows.");
    await panel.scrollIntoViewIfNeeded();
    const bounds = await panel.boundingBox();
    expect(bounds).not.toBeNull();
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    expect(await panel.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
    await panel.screenshot({ path: testInfo.outputPath("child-activity.png") });
    await panel.getByRole("button", { name: `Open child ${child}` }).click();
    await expect(page).toHaveURL(new RegExp(`#/run/${child}$`));
    await expect(panel.getByRole("button", { name: "Implementation" })).toBeVisible();
    await panel.getByRole("button", { name: "Implementation" }).click();
    await expect(page).toHaveURL(new RegExp(`#/run/${parent}$`));
    expect(errors).toEqual([]);
  });
}
