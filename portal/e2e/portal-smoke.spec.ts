import { expect, test, type Page } from "@playwright/test";

function trackConsoleErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") {
      errors.push(message.text());
    }
  });
  return errors;
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
