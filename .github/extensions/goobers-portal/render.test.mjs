import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { lintInnerHTMLAssignments } from "./innerhtml-lint.mjs";
import {
    formatRunDetailTime,
    renderCausalDiagnosis,
    renderExecutionWaterfall,
    renderHtml,
    renderRunDetailSummary,
    renderRunEventItems,
    renderOperatorPanel,
    renderRunRowCells,
    renderSnapshotCard,
    renderTransitions,
} from "./render.mjs";

test("causal diagnosis renders attempts and escaped failure breadcrumbs", () => {
    const html = renderCausalDiagnosis({
        events: [
            { type: "stage.started", stage: "build", attempt: 1 },
            { type: "stage.finished", stage: "build", attempt: 1, status: "failed", error: "<unsafe> failed" },
        ],
    });
    assert.match(html, /Attempt lineage/);
    assert.match(html, /build/);
    assert.match(html, /&lt;unsafe&gt;/);
});

test("execution waterfall renders retry timing, gaps, and unavailable timing", () => {
    const html = renderExecutionWaterfall({
        events: [
            { type: "stage.started", stage: "first", attempt: 1, time: "2026-08-27T00:00:00Z" },
            { type: "stage.finished", stage: "first", attempt: 1, status: "succeeded", time: "2026-08-27T00:00:01Z" },
            { type: "stage.started", stage: "retry", attempt: 2, time: "2026-08-27T00:00:03Z" },
            { type: "stage.finished", stage: "retry", attempt: 2, status: "failed", time: "2026-08-27T00:00:04Z" },
            { type: "stage.started", stage: "unknown", attempt: 1 },
        ],
    });
    assert.match(html, /retry/);
    assert.match(html, /Idle gaps: 2s/);
    assert.match(html, /timing unavailable/);
});

// #4567: the snapshot cards and run table interpolate values that originate
// outside the portal — an instance name read from instance.yaml, run IDs,
// workflow and gaggle names, and phases carried in run metadata. Before this
// they reached innerHTML raw. These cover the escaping directly, and the
// inlining test below covers the browser actually getting the escaped copy.

const HOSTILE = '<img src=x onerror=alert(1)>';

test("snapshot cards escape an untrusted instance name", () => {
    const html = renderSnapshotCard("Instance", HOSTILE);
    assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
    assert.doesNotMatch(html, /<img/);
    assert.match(html, /<div class="label">Instance<\/div>/);
});

test("snapshot cards preserve ordinary numeric values", () => {
    assert.match(renderSnapshotCard("Recent runs", 12), /<div class="value">12<\/div>/);
});

test("run rows escape every field the run itself controls", () => {
    const html = renderRunRowCells(
        {
            runId: '"><script>alert(1)</script>',
            workflow: HOSTILE,
            gaggle: "a & b",
            trigger: { kind: HOSTILE },
            phase: "<b>running</b>",
        },
        { startedAt: HOSTILE, lastActivityAt: "'" },
    );
    for (const raw of ["<script>", "<img", "<b>running</b>"]) {
        assert.ok(!html.includes(raw), `unescaped ${raw} in ${html}`);
    }
    assert.match(html, /&quot;&gt;&lt;script&gt;/);
    assert.match(html, /a &amp; b/);
    assert.match(html, /&lt;b&gt;running&lt;\/b&gt;/);
    assert.match(html, /&#39;/);
});

test("run rows keep pre-escaped association and actions markup as markup", () => {
    const html = renderRunRowCells(
        { runId: "r1" },
        {
            actionsLink: '<a class="actions-run-link" href="https://example.test">Action</a>',
            associations: '<div class="run-associations">linked</div>',
        },
    );
    assert.match(html, /<a class="actions-run-link" href="https:\/\/example.test">Action<\/a>/);
    assert.match(html, /<div class="run-associations">linked<\/div>/);
});

test("run rows fall back without throwing on an empty run", () => {
    const html = renderRunRowCells(undefined, undefined);
    assert.match(html, /<td><code><\/code><\/td>/);
    assert.ok(!html.includes("undefined"), html);
});

test("run detail summary escapes every metadata field", () => {
    const html = renderRunDetailSummary({
        id: HOSTILE,
        workflow: HOSTILE,
        workflowVersion: '"><script>version</script>',
        gaggle: HOSTILE,
        phase: HOSTILE,
        terminal: true,
        repassCount: HOSTILE,
        retryCount: HOSTILE,
        trigger: { kind: HOSTILE },
        startedAt: HOSTILE,
        finishedAt: HOSTILE,
        events: [{ type: "stage.finished", stage: HOSTILE }],
        transitions: [{ terminal: true, status: HOSTILE }],
    });
    assert.doesNotMatch(html, /<img|<script>/);
    assert.match(html, /&lt;img/);
    assert.match(html, /&lt;script&gt;version/);
});

test("run event items escape hostile metadata and preserve safe links", () => {
    const html = renderRunEventItems([{
        seq: HOSTILE,
        type: HOSTILE,
        stage: HOSTILE,
        status: HOSTILE,
        time: HOSTILE,
        artifact: { digest: '"><script>digest</script>', name: HOSTILE, size: HOSTILE },
        externalRef: { url: "https://example.test/run", provider: HOSTILE, kind: HOSTILE, id: HOSTILE },
        outputs: { value: HOSTILE },
    }], HOSTILE, HOSTILE);
    assert.doesNotMatch(html, /<img|<script>/);
    assert.match(html, /&lt;img/);
    assert.match(html, /href="https:\/\/example\.test\/run"/);
    assert.match(html, /%3Cimg%20src%3Dx/);
});

test("run transitions escape hostile sequence, verdict, and state metadata", () => {
    const html = renderTransitions([{
        seq: HOSTILE,
        source: HOSTILE,
        target: HOSTILE,
        verdict: HOSTILE,
        status: HOSTILE,
        terminal: false,
    }]);
    assert.doesNotMatch(html, /<img/);
    assert.match(html, /&lt;img/);
    assert.doesNotMatch(html, /class="[^"]*<img/);
});

test("operator panel escapes hostile issue, liveness, trajectory, and review metadata", () => {
    const html = renderOperatorPanel({
        issue: { number: HOSTILE, title: HOSTILE },
        pullRequest: { provider: HOSTILE, kind: HOSTILE, id: HOSTILE },
        liveness: HOSTILE,
        trajectory: HOSTILE,
        review: { verdict: HOSTILE, rationale: HOSTILE },
        latestError: { code: HOSTILE, message: HOSTILE },
        potentialBlockers: [HOSTILE],
    });
    assert.doesNotMatch(html, /<img/);
    assert.match(html, /&lt;img/);
    assert.doesNotMatch(html, /class="[^"]*<img/);
});

test("run-detail time fallback escapes values when date conversion throws", () => {
    const hostile = { toString() { return HOSTILE; }, valueOf() { throw new Error("no conversion"); } };
    const formatted = formatRunDetailTime(hostile);
    assert.doesNotMatch(formatted, /<img/);
    assert.match(formatted, /&lt;img/);
});

test("innerHTML lint rejects direct unescaped property interpolation", async () => {
    const source = await readFile(new URL("./render.mjs", import.meta.url), "utf8");
    assert.deepEqual(lintInnerHTMLAssignments(source), []);
    assert.deepEqual(
        lintInnerHTMLAssignments('element.innerHTML = "<p>" + run.workflow + "</p>";'),
        ["line 1: innerHTML concatenates unescaped run.workflow"],
    );
    assert.deepEqual(
        lintInnerHTMLAssignments('element.innerHTML = "<p>" + escapeHtml(run.workflow) + "</p>";'),
        [],
    );
    assert.deepEqual(
        lintInnerHTMLAssignments('element.innerHTML = "<p>" + workflow + "</p>";'),
        ["line 1: innerHTML concatenates unescaped workflow"],
    );
    for (const mutation of [
        'element.innerHTML = "<p>" + String(run.workflow) + "</p>";',
        'element.innerHTML = "<p>" + (run.workflow ? run.workflow : "") + "</p>";',
        'element.innerHTML = `<p>${run.workflow}</p>`;',
        'let runDetailHtml = "<p>" + run.workflow; element.innerHTML = runDetailHtml;',
        'let runDetailHtml = "<p>"; runDetailHtml += run.workflow; element.innerHTML = runDetailHtml;',
        'let rowMarkup = "<p>"; rowMarkup += run.workflow; element.innerHTML = rowMarkup;',
    ]) {
        assert.equal(lintInnerHTMLAssignments(mutation).length, 1, mutation);
    }
    assert.deepEqual(
        lintInnerHTMLAssignments('let runDetailHtml = "<p>" + escapeHtml(run.workflow); element.innerHTML = runDetailHtml;'),
        [],
    );
});

test("the browser script receives the escaping helpers, not raw interpolation", () => {
    const page = renderHtml("inst-1");
    // The helpers are inlined verbatim for the client to call...
    assert.match(page, /const renderSnapshotCard = function renderSnapshotCard/);
    assert.match(page, /const renderRunRowCells = function renderRunRowCells/);
    assert.match(page, /const renderRunDetailSummary = function renderRunDetailSummary/);
    assert.match(page, /const renderRunEventItems = function renderRunEventItems/);
    assert.match(page, /const renderTransitions = function renderTransitions/);
    assert.match(page, /const renderOperatorPanel = function renderOperatorPanel/);
    // ...remapped onto the client's own escapeHtml, so no stale identifier
    // survives to throw at runtime.
    assert.ok(!page.includes("escapeAssociationHtml"), "escapeAssociationHtml leaked into the page");
    // ...and the call sites go through them rather than concatenating.
    assert.match(page, /div\.innerHTML = renderSnapshotCard\(label, value\);/);
    assert.match(page, /tr\.innerHTML = renderRunRowCells\(r, \{/);
    assert.match(page, /let html = renderRunDetailSummary\(r, \{ actionsLink \}\);/);
    // The pre-fix raw forms are gone.
    assert.ok(!page.includes('"<td>" + (r.workflow || "")'), "raw workflow interpolation still present");
    assert.ok(!page.includes('"<td><code>" + runId'), "raw runId interpolation still present");
    assert.doesNotMatch(page, /\+\s*r\.[A-Za-z_$]/, "raw run-detail interpolation still present");
});
