import assert from "node:assert/strict";
import test from "node:test";
import { Script } from "node:vm";

import { filterRunSummaries, loadRuns } from "./client.mjs";
import { renderGraphLegend, renderHtml, renderRunAssociations, renderTelemetryInsights } from "./render.mjs";

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

test("rendered browser script is valid JavaScript", () => {
    const html = renderHtml("test");
    const scripts = [...html.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)];
    assert.ok(scripts.length > 0);
    const browserScript = scripts.at(-1)[1];
    assert.doesNotThrow(() => new Script(browserScript));
    for (const helper of [
        "decodeStreamEvent",
        "decodeViewState",
        "deriveFreshnessState",
        "encodeViewState",
        "isInvalidCursorError",
        "mergeRunPage",
        "shouldApplyRestoredFilters",
    ]) {
        assert.match(browserScript, new RegExp(`const ${helper} =`));
    }
    assert.match(browserScript, /data-expand-attention/);
    assert.match(html, /\.attention-item \{[\s\S]*height: 84px/);
    assert.match(html, /aria-label="Dashboard sections"/);
    assert.match(browserScript, /aria-label="Run detail sections"/);
    assert.match(browserScript, /function initInternalTabs/);
});

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
    const legend = renderGraphLegend();
    assert.equal((legend.match(/<span class="legend-chip /g) || []).length, 6);
    assert.match(html, /const renderGraphLegend = function renderGraphLegend/);
    assert.match(html, /const filterTranscriptEntries = function filterTranscriptEntries/);
    assert.match(html, /initTranscriptFilters\(events, sourceId, runId\)/);
    assert.match(html, /data-transcript-filter="stage"/);
});

test("run detail waterfall uses all timestamps and renders execution metadata", () => {
    const html = renderHtml("waterfall-test");
    assert.match(html, /Math\.min\(\.\.\.timestamps\)/);
    assert.match(html, /Math\.max\(\.\.\.timestamps\)/);
    assert.match(html, /Idle gaps/);
    assert.match(html, /timing unavailable/);
    assert.match(html, /· retry/);
});

test("telemetry insights render unavailable values and measured zeroes", () => {
    const html = renderTelemetryInsights({ metrics: { costUSD: { value: 0, unit: "USD" } } });
    assert.match(html, /Unknown/);
    assert.match(html, /0 USD/);
    assert.match(renderHtml("insights-test"), /Telemetry insights/);
});

test("telemetry insights keep sub-second durations distinct from zero", () => {
    const html = renderTelemetryInsights({ durationMillis: 250 });
    assert.match(html, /250ms/);
});

test("telemetry insights disclose omitted hotspot stages", () => {
    const events = Array.from({ length: 6 }, (_, index) => ({
        type: "stage.finished",
        stage: "stage-" + index,
        attempt: 1,
        status: "failed",
        time: "2026-08-28T10:00:0" + index + "Z",
    }));
    assert.match(renderTelemetryInsights({ events }), /\+1 more hotspots\./);
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
