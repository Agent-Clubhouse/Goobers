import assert from "node:assert/strict";
import test from "node:test";

import {
    renderCausalDiagnosis,
    renderExecutionWaterfall,
    renderHtml,
    renderRunRowCells,
    renderSnapshotCard,
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

test("the browser script receives the escaping helpers, not raw interpolation", () => {
    const page = renderHtml("inst-1");
    // The helpers are inlined verbatim for the client to call...
    assert.match(page, /const renderSnapshotCard = function renderSnapshotCard/);
    assert.match(page, /const renderRunRowCells = function renderRunRowCells/);
    // ...remapped onto the client's own escapeHtml, so no stale identifier
    // survives to throw at runtime.
    assert.ok(!page.includes("escapeAssociationHtml"), "escapeAssociationHtml leaked into the page");
    // ...and the call sites go through them rather than concatenating.
    assert.match(page, /div\.innerHTML = renderSnapshotCard\(label, value\);/);
    assert.match(page, /tr\.innerHTML = renderRunRowCells\(r, \{/);
    // The pre-fix raw forms are gone.
    assert.ok(!page.includes('"<td>" + (r.workflow || "")'), "raw workflow interpolation still present");
    assert.ok(!page.includes('"<td><code>" + runId'), "raw runId interpolation still present");
});
