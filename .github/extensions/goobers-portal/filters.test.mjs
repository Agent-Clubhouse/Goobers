import assert from "node:assert/strict";
import test from "node:test";

import { filterRunSummaries, loadRuns } from "./client.mjs";
import { renderHtml } from "./render.mjs";

const runs = [
    {
        id: "one",
        gaggle: "crawler",
        workflow: "feature-pr",
        phase: "running",
        trigger: { kind: "manual" },
        startedAt: "2026-08-27T01:00:00Z",
    },
    {
        id: "two",
        gaggle: "crawler",
        workflow: "merge-review",
        phase: "completed",
        trigger: { kind: "schedule" },
        startedAt: "2026-08-27T02:00:00Z",
    },
];

test("filterRunSummaries applies standalone-supported filters together", () => {
    assert.deepEqual(
        filterRunSummaries(runs, {
            gaggle: "crawler",
            workflow: "merge-review",
            phase: "completed",
            trigger: "schedule",
            since: "2026-08-27T01:30:00Z",
            until: "2026-08-27T02:30:00Z",
        }).map((run) => run.id),
        ["two"],
    );
});

test("runs filters include the stage required by outcome and population", () => {
    const html = renderHtml("filters-test");
    assert.match(html, /id="filter-stage"/);
    assert.match(html, /id="filter-outcome"/);
    assert.match(html, /id="filter-population"/);
    assert.match(html, /if \(data\.error\)/);
    assert.match(html, /Close and reopen this canvas to reconnect/);
    assert.match(html, /\.kv-wide \{ grid-column: 1 \/ -1; \}/);
    assert.match(html, /label === "Latest error" \? " kv-wide"/);
});

test("GitHub Actions rejects unsupported telemetry filters", async () => {
    const result = await loadRuns({ mode: "actions" }, { outcome: "success" });
    assert.deepEqual(result.runs, []);
    assert.match(result.error, /not available for GitHub Actions/);
});
