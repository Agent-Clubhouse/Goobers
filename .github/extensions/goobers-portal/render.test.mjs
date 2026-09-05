import assert from "node:assert/strict";
import test from "node:test";

import { renderCausalDiagnosis, renderExecutionWaterfall } from "./render.mjs";

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
