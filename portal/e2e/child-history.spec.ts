import { expect, test } from "@playwright/test";

const parent = "01JZE2ESMOKERUN";
const timestamp = "2026-10-09T15:00:00Z";
for (const width of [1280, 390]) {
  test(`accepted child history supports paging at ${width}px`, async ({ page }, testInfo) => {
    await page.setViewportSize({ width, height: 900 });
    let firstPageReads = 0;
    await page.route(`**/api/v1/runs/${parent}/children**`, async (route) => {
      const next = new URL(route.request().url()).searchParams.get("cursor") === "next";
      if (!next) firstPageReads++;
      await route.fulfill({ json: { runId: parent, gaggle: "core", status: "recorded", observedAt: timestamp, nextCursor: next ? "" : "next", items: [{
        childId: next ? "child-b" : "child-a", runId: `child-${"a".repeat(64)}`, stageOccurrence: "b".repeat(256), invocationKey: next ? "Finished child" : "Inspect changes", state: next ? "cancelled" : "running", acceptedAt: timestamp, updatedAt: timestamp, cancellationRequested: true,
        ...(next ? { terminalAt: timestamp, acknowledgedAt: timestamp, expiredAt: timestamp } : {}),
        publication: { status: next ? "expired" : "recorded", items: next ? [] : [
          { action: "branch", state: "confirmed", head: "factory/children/" + "c".repeat(256), base: "main", commit: "a".repeat(40), pullRequestUrl: "", pullRequestNumber: 0, needsHuman: false },
          { action: "pr", state: "confirmed", head: "factory/children/child", base: "main", commit: "a".repeat(40), pullRequestUrl: "https://github.com/owner/repo/pull/7", pullRequestNumber: 7, needsHuman: false },
        ] },
      }, ...(!next ? [{ childId: "uncertain", runId: "uncertain-child", stageOccurrence: "stage-2", invocationKey: "Publication needs review", state: "failed", acceptedAt: timestamp, updatedAt: timestamp, terminalAt: timestamp, cancellationRequested: false, publication: { status: "recorded", items: [
        { action: "branch", state: "confirmed", head: "factory/children/uncertain", base: "main", commit: "a".repeat(40), pullRequestUrl: "", pullRequestNumber: 0, needsHuman: false },
        { action: "pr", state: "effect_pending", head: "factory/children/uncertain", base: "main", commit: "a".repeat(40), pullRequestUrl: "", pullRequestNumber: 0, needsHuman: true },
      ] } }] : [])] } });
    });
    await page.goto(`/#/run/${parent}`);
    const panel = page.getByRole("region", { name: "Accepted child history" });
    await expect(panel).toContainText("Cancellation requested; stopping is not yet confirmed.");
    await expect(panel).toContainText("PR publication needs a human.");
    await expect(panel.getByRole("link", { name: "Open child PR #7" })).toHaveAttribute("href", "https://github.com/owner/repo/pull/7");
    await panel.scrollIntoViewIfNeeded();
    expect(await panel.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
    const bounds = await panel.boundingBox();
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    await panel.screenshot({ path: testInfo.outputPath("child-history.png") });
    await panel.getByRole("button", { name: "Next children" }).click();
    await expect(panel).toContainText("Finished child");
    await expect(panel).toContainText("Saved child result expired");
    await expect(panel).not.toContainText("stopping is not yet confirmed");
    await expect(panel.getByRole("button", { name: /Open child run/ })).toHaveCount(0);
    await expect(panel.getByRole("link", { name: /Open child PR/ })).toHaveCount(0);
    await panel.getByRole("button", { name: "Refresh child history" }).click();
    await expect(panel).toContainText("Inspect changes");
    expect(firstPageReads).toBeGreaterThanOrEqual(2);
  });
}
