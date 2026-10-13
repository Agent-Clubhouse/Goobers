import { expect, test } from "@playwright/test";
import type { RunDetail, RunList, RunSummary } from "../src/api/types";

const runId = "01JZE2ESMOKERUN";
const itemURL = "https://example.test/incidents/123";

function withWorkItem(run: RunSummary): RunSummary {
  if (run.id !== runId) return run;
  return {
    ...run,
    operator: {
      ...run.operator,
      liveness: "active",
      trajectory: "implement",
      claim: { leaseStatus: "held", providerMarker: "recorded" },
      potentialBlockers: [],
      issue: { number: "123", title: "Example incident", url: itemURL },
      displayTitle: "Investigate #123: Example incident",
    },
  };
}

test.beforeEach(async ({ page, context }) => {
  await context.route("https://example.test/**", (route) =>
    route.fulfill({ contentType: "text/html", body: "<h1>Source item</h1>" }),
  );
  await page.route(/\/api\/v1\/runs(?:\?.*)?$/, async (route) => {
    const response = await route.fetch();
    const payload: RunList = await response.json();
    await route.fulfill({ response, json: { ...payload, runs: payload.runs.map(withWorkItem) } });
  });
  await page.route(`**/api/v1/runs/${runId}`, async (route) => {
    const response = await route.fetch();
    const payload: RunDetail = await response.json();
    await route.fulfill({ response, json: { ...payload, ...withWorkItem(payload) } });
  });
});

for (const path of ["/#/runs", "/#/overview", "/#/workflow/core/implementation"]) {
  test(`work-item link and row navigation remain independent on ${path}`, async ({ page }) => {
    await page.goto(path);
    const rowLink = page.getByRole("link", { name: `Open run ${runId}`, exact: true });
    const row = rowLink.locator("..");
    const itemLink = row.getByRole("link", { name: "#123", exact: true });
    await expect(itemLink).toBeVisible();
    await expect(row.locator(".row-title")).toHaveText("Investigate #123: Example incident");

    const popupPromise = page.waitForEvent("popup");
    await itemLink.click();
    const popup = await popupPromise;
    await expect(popup).toHaveURL(itemURL);
    await expect(page).toHaveURL(new RegExp(`${path.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`));
    await popup.close();

    await itemLink.focus();
    const keyboardPopupPromise = page.waitForEvent("popup");
    await page.keyboard.press("Enter");
    const keyboardPopup = await keyboardPopupPromise;
    await expect(keyboardPopup).toHaveURL(itemURL);
    await keyboardPopup.close();

    await rowLink.click();
    await expect(page).toHaveURL(new RegExp(`/#/run/${runId}$`));
    await expect(page.getByRole("heading", { name: "Investigate #123: Example incident" })
      .getByRole("link", { name: "#123" })).toHaveAttribute("href", itemURL);
  });
}
