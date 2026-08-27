import assert from "node:assert/strict";
import test from "node:test";

import { filterRunSummaries, loadRuns } from "./client.mjs";
import { renderHtml, renderRunAssociations } from "./render.mjs";

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
    assert.match(html, /<th>Associated work<\/th>/);
    assert.match(html, /Issue #" \+ operator\.issue\.number/);
    assert.match(html, /operator\.pullRequestTitle/);
    assert.match(html, /class="run-association-link"/);
});

test("run associations render safe issue and PR title links", () => {
    const html = renderRunAssociations({
        issue: {
            number: 7,
            title: "Fix <unsafe>",
            url: "https://github.com/octo/app/issues/7",
        },
        pullRequest: {
            id: "42",
            url: "https://github.com/octo/app/pull/42",
        },
        pullRequestTitle: 'Ship "the fix"',
    });
    assert.match(html, /Issue #7: Fix &lt;unsafe&gt;/);
    assert.match(html, /PR #42: Ship &quot;the fix&quot;/);
    assert.equal(renderRunAssociations({
        issue: { number: 8, title: "Unsafe", url: "javascript:alert(1)" },
    }), "\u2014");
});

test("GitHub Actions rejects unsupported telemetry filters", async () => {
    const result = await loadRuns({ mode: "actions" }, { outcome: "success" });
    assert.deepEqual(result.runs, []);
    assert.match(result.error, /not available for GitHub Actions/);
});
