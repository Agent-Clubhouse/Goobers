import { expect, test, type Page, type Route } from "@playwright/test";
import { defaultPortalConfig } from "../src/cobrand";
import { populatedDaemonFixtures } from "../src/test/daemonFixtures";

const runId = "01JZ441DAEMONAPI";
const stage = "review";
const digest = "sha256:durable";
const content = "durable content";
const refreshIntervalMs = 1_000;

async function openInspector(page: Page): Promise<() => number> {
  const fixtures = populatedDaemonFixtures();
  const attempts = {
    runId,
    stage,
    attempts: [
      {
        id: "sta-review-1",
        visit: 1,
        number: 1,
        class: "initial" as const,
        status: "running" as const,
        startedSeq: 6,
        durationMillis: 1_500,
        artifacts: [
          {
            name: "result.txt",
            digest,
            size: content.length,
            mediaType: "text/plain",
            recordedSeq: 6,
          },
        ],
      },
    ],
  };
  const detail = fixtures.runDetails?.[runId];
  const events = fixtures.runEvents?.[runId];
  if (!detail || !events) {
    throw new Error("Expected active run fixtures.");
  }
  let attemptRequests = 0;
  let eventSequence = 0;

  await page.route("**/api/v1/**", async (route: Route) => {
    const path = new URL(route.request().url()).pathname;
    if (path === "/api/v1/events") {
      eventSequence += 1;
      await new Promise((resolve) => setTimeout(resolve, refreshIntervalMs));
      const cursor = `browser:${eventSequence}`;
      await route.fulfill({
        body: `id: ${cursor}\nevent: invalidate\ndata: ${JSON.stringify({
          cursor,
          models: ["run"],
          runIds: [runId],
          workflows: [],
        })}\n\n`,
        contentType: "text/event-stream",
      });
      return;
    }
    if (path === "/api/v1/portal/config") {
      await route.fulfill({ json: defaultPortalConfig });
      return;
    }
    if (path === `/api/v1/runs/${runId}`) {
      await route.fulfill({ json: detail });
      return;
    }
    if (path === `/api/v1/runs/${runId}/events`) {
      await route.fulfill({ json: events });
      return;
    }
    if (path === `/api/v1/runs/${runId}/stages/${stage}/attempts`) {
      attemptRequests += 1;
      await route.fulfill({ json: attempts });
      return;
    }
    if (path.startsWith(`/api/v1/runs/${runId}/artifacts/`)) {
      await route.fulfill({
        body: content,
        headers: {
          "Content-Length": String(content.length),
          "Content-Type": "text/plain",
          "X-Goobers-Digest": digest,
        },
      });
      return;
    }
    await route.fulfill({
      status: 500,
      json: { error: { code: "unexpected_test_request", message: path } },
    });
  });

  await page.goto(`/#/run/${runId}`);
  await expect(page.getByRole("button", { name: "View content" })).toBeVisible();
  return () => attemptRequests;
}

test("expanded artifact remains visible across three background refreshes", async ({ page }) => {
  test.setTimeout(30_000);
  const attemptRequests = await openInspector(page);
  const baseline = attemptRequests();

  await page.getByRole("button", { name: "View content" }).click();
  await expect(page.getByText(content)).toBeVisible();

  // Three refresh cycles need at least 3 * refreshIntervalMs (3000ms) of
  // artificial SSE delay alone, before any reconnect/fetch/render overhead —
  // the default 5000ms expect.poll timeout leaves too little margin under
  // load (#2604: observed failing deterministically at 5-vs-6 cycles on a
  // busy CI runner, unrelated to this file's own logic).
  await expect.poll(attemptRequests, { timeout: 15_000 }).toBeGreaterThanOrEqual(baseline + 3);
  await expect(page.getByText(content)).toBeVisible();
});

test("collapsed artifact remains collapsed across three background refreshes", async ({ page }) => {
  test.setTimeout(30_000);
  const attemptRequests = await openInspector(page);
  const baseline = attemptRequests();

  await expect.poll(attemptRequests, { timeout: 15_000 }).toBeGreaterThanOrEqual(baseline + 3);
  await expect(page.getByRole("button", { name: "View content" })).toBeVisible();
  await expect(page.getByText(content)).toHaveCount(0);
});
