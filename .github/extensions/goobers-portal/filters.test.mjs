import assert from "node:assert/strict";
import test from "node:test";
import { Script } from "node:vm";

import { filterRunSummaries, loadRuns } from "./client.mjs";
import { renderGraphLegend, renderHtml, renderRunAssociations, renderRunRowCells, renderTelemetryInsights } from "./render.mjs";

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
    assert.match(browserScript, /const persistedFilterState =/);
    assert.match(html, /id="filter-phase" class="native-multi-filter" multiple/);
    assert.match(html, /\.multi-filter-menu/);
    assert.match(browserScript, /function initMultiFilter/);
    assert.match(html, />Reset<\/button>/);
    assert.match(html, /\[role="tabpanel"\]\[hidden\] \{ display: none !important; \}/);
    assert.doesNotMatch(
        browserScript,
        /function portalRequestError[\s\S]*?function activateInternalTab[\s\S]*?return message;/,
    );
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
    assert.match(html, /addLink\("Issue"/);
    assert.match(html, /operator\.pullRequestTitle/);
    assert.match(html, /class="run-association-link work-chip"/);
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
            state: "open",
        },
        pullRequestTitle: 'Ship "the fix"',
    });
    assert.match(html, /Issue #7: Fix &lt;unsafe&gt;/);
    assert.match(html, /PR #42: Ship &quot;the fix&quot;/);
    assert.match(html, /class="run-association-link work-chip"/);
    assert.match(html, /data-status="open"/);
    assert.equal(renderRunAssociations({
        issue: { number: 8, title: "Unsafe", url: "javascript:alert(1)" },
    }), "\u2014");
});

test("run associations include issue and PR links from run-level refs", () => {
    const html = renderRunAssociations({
        operator: {
            issue: {
                number: 7,
                title: "Implement thing",
                url: "https://github.com/octo/app/issues/7",
            },
        },
        externalRefs: [
            {
                kind: "pr",
                id: 42,
                title: "Ship thing",
                url: "https://github.com/octo/app/pull/42",
            },
        ],
    });
    assert.match(html, /Issue #7: Implement thing/);
    assert.match(html, /PR #42: Ship thing/);
});

test("run associations link event refs with summary titles", () => {
    const html = renderRunAssociations({
        operator: {
            issue: {
                number: "159",
                title: "Classifier proposal",
            },
        },
        externalRefs: [
            {
                provider: "github",
                kind: "issue",
                id: "159",
                url: "https://github.com/example-org/example-repo/issues/159",
            },
        ],
    });
    assert.match(html, /href="https:\/\/github\.com\/example-org\/example-repo\/issues\/159"/);
    assert.match(html, /Issue #159: Classifier proposal/);
});

for (const field of ["refs", "externalRefs"]) {
    for (const kind of ["issue", "pr"]) {
        test(`runs table hydrates a URL-less ${kind} found only in run.${field}`, async () => {
            const originalFetch = globalThis.fetch;
            const runId = `${field}-${kind}`;
            const baseUrl = "http://run-level-refs";
            const ref = { kind, id: "159" };
            const url = `https://github.com/octo/app/${kind === "issue" ? "issues" : "pull"}/159`;
            const requests = [];
            globalThis.fetch = async (request) => {
                requests.push(request);
                if (String(request).includes("/api/v1/runs?")) {
                    return Response.json({ runs: [{ id: runId, [field]: [ref] }] });
                }
                assert.equal(request, `${baseUrl}/api/v1/runs/${runId}/events`);
                return Response.json({ events: [{ externalRef: { ...ref, url } }] });
            };
            try {
                const result = await loadRuns({ mode: "daemon", baseUrl });
                assert.equal(requests.length, 2, "run-level reference must trigger event hydration");
                const html = renderHtml("run-level-refs");
                const browserScript = [...html.matchAll(/<script[^>]*>([\s\S]*?)<\/script>/g)].at(-1)[1];
                const renderRuns = browserScript.match(/  function renderRuns\(runs\) \{[\s\S]*?(?=\n  function )/)[0];
                const rows = [];
                new Script(`${renderRuns}\nrenderRuns(runs);`).runInNewContext({
                    runs: result.runs,
                    sortRuns: (items) => items,
                    runsBody: { innerHTML: "", appendChild: (row) => rows.push(row) },
                    document: {
                        createElement: () => ({
                            dataset: {},
                            querySelectorAll: () => [],
                            addEventListener() {},
                        }),
                    },
                    safeExternalUrl: () => "",
                    renderRunAssociations,
                    renderRunRowCells,
                    fmtTime: () => "",
                    attachRunIdControls() {},
                    updateSortIndicators() {},
                });
                assert.equal(rows.length, 1);
                assert.ok(rows[0].innerHTML.includes(`href="${url}"`), "associated-work cell must contain hydrated link");
                assert.match(rows[0].innerHTML, /class="run-association-link work-chip"/);
            } finally {
                globalThis.fetch = originalFetch;
            }
        });
    }
}

for (const kind of ["issue", "pr"]) {
    test(`run associations preserve same-number ${kind} links across repositories and dedupe canonical URLs`, () => {
        const segment = kind === "issue" ? "issues" : "pull";
        const firstUrl = `https://github.com/octo/a/${segment}/42`;
        const secondUrl = `https://github.com/octo/b/${segment}/42`;
        const html = renderRunAssociations({
            refs: [
                { kind, id: "42", url: firstUrl },
                { kind, id: "42", url: secondUrl },
                { kind, number: 42, url: firstUrl + "#issuecomment-123" },
                { kind, id: "42", url: firstUrl + "/" },
                { kind, id: "42", url: firstUrl.replace("https:", "http:") },
                { kind, id: "42", url: firstUrl.replace("github.com", "www.github.com") },
                { kind, id: "42", url: firstUrl.replace("/octo/a/", "/OCTO/A/") },
                { kind, id: "42", url: `http://www.github.com/OCTO/A/${segment.toUpperCase()}/42/#comment` },
            ],
            externalRefs: [{ kind, externalId: "42", url: firstUrl }],
        });
        assert.ok(html.includes(`href="${firstUrl}"`));
        assert.ok(html.includes(`href="${secondUrl}"`));
        assert.equal((html.match(/class="run-association-link work-chip"/g) || []).length, 2);
    });
}

test("association URL normalization preserves non-GitHub resource distinctions", () => {
    const urls = [
        "https://example.test/Repo/issues/42",
        "https://example.test/repo/issues/42",
        "http://example.test/Repo/issues/42",
        "https://example.test/Repo/issues/42/",
        "https://github.com:8443/octo/a/issues/42",
        "https://github.com/octo/a/issues/42",
    ];
    const html = renderRunAssociations({
        refs: urls.map((url) => ({ kind: "issue", id: "42", url })),
    });
    assert.equal((html.match(/class="run-association-link work-chip"/g) || []).length, urls.length);
    for (const url of urls) assert.ok(html.includes(`href="${url}"`));
});

test("GitHub Actions rejects unsupported telemetry filters", async () => {
    const result = await loadRuns({ mode: "actions" }, { outcome: "success" });
    assert.deepEqual(result.runs, []);
    assert.match(result.error, /not available for GitHub Actions/);
});
