import { expect, test, type Page } from "@playwright/test";

interface RunsResponse {
  runs?: Array<{ id?: string }>;
}

interface RouteCase {
  name: string;
  path: string;
  heading: string | RegExp;
}

function trackBrowserErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (message) => {
    if (message.type() === "error") errors.push(`console: ${message.text()}`);
  });
  page.on("pageerror", (error) => errors.push(`pageerror: ${error.message}`));
  return errors;
}

test("renders every route against a real demo dashboard without browser errors", async ({
  page,
  request,
}) => {
  test.setTimeout(60_000);
  const response = await request.get("/api/v1/runs");
  expect(response.ok()).toBe(true);
  const payload = (await response.json()) as RunsResponse;
  const runId = payload.runs?.[0]?.id;
  expect(runId, "the real demo run must be available to exercise the run route").toBeTruthy();

  const routes: RouteCase[] = [
    { name: "overview", path: "/#/overview", heading: "Active runs" },
    { name: "workflows", path: "/#/workflows", heading: "Workflows" },
    { name: "goobers", path: "/#/goobers", heading: "Goobers" },
    { name: "gaggle", path: "/#/gaggle/demo", heading: "Offline Demo" },
    { name: "runs", path: "/#/runs", heading: "Runs" },
    { name: "errors", path: "/#/errors", heading: "Matching errors" },
    { name: "insight", path: "/#/insight", heading: "Insight" },
    { name: "cost", path: "/#/cost", heading: "Cost" },
    { name: "work items", path: "/#/work-items", heading: "Work Items" },
    {
      name: "workflow",
      path: "/#/workflow/demo/demo",
      heading: "Hermetic full-loop demo",
    },
    { name: "run", path: `/#/run/${runId}`, heading: new RegExp(`^Run ${runId}$`) },
  ];

  const errors = trackBrowserErrors(page);
  for (const route of routes) {
    errors.length = 0;
    await test.step(`render ${route.name}`, async () => {
      const scopedAnalytics =
        route.name === "workflow"
          ? page.waitForResponse((candidate) => {
              const url = new URL(candidate.url());
              return (
                url.pathname === "/api/v1/telemetry/stats" &&
                url.searchParams.get("gaggle") === "demo" &&
                url.searchParams.get("workflow") === "demo"
              );
            })
          : undefined;
      await page.goto(route.path);
      await expect(page.getByRole("heading", { name: route.heading, exact: true })).toBeVisible();
      await expect(page.locator("main")).not.toBeEmpty();
      if (route.name === "workflow") {
        // This page issues the gaggle/workflow-scoped insight request whose
        // real null graphAnalytics value caused the #4825 white screen.
        expect((await scopedAnalytics)?.ok()).toBe(true);
        await expect(page.locator(".workflow-graph-shell")).toBeVisible();
      }
      // The portal deliberately keeps its SSE stream open, so network-idle is
      // not a meaningful readiness signal. Give post-render effects one turn
      // to surface rejected requests or render errors instead.
      await page.waitForTimeout(100);
      expect(errors, `${route.name} emitted browser errors`).toEqual([]);
    });
  }
});
