import assert from "node:assert/strict";
import test from "node:test";

import {
    attentionKey,
    decodeStreamEvent,
    decodeViewState,
    deriveAttemptLineage,
    deriveAttention,
    deriveFailureBreadcrumbs,
    deriveFreshnessState,
    deriveTelemetryInsights,
    encodeViewState,
    filterTranscriptEntries,
    isInvalidCursorError,
    mergeRunPage,
    shouldApplyRestoredFilters,
} from "./ux.mjs";

test("deriveAttention returns bounded actionable states with causal details", () => {
    const runs = [
        { id: "failed", workflow: "build", phase: "failed", currentStage: "test", startedAt: "2026-08-28T10:00:00Z", reason: "ignored" },
        { id: "running", phase: "running" },
        { id: "stale", phase: "running", operator: { liveness: "stale", latestError: { message: "heartbeat lost" } } },
    ];
    const result = deriveAttention(runs, { now: Date.parse("2026-08-28T11:00:00Z"), limit: 2 });
    assert.deepEqual(result.map((item) => item.id), ["failed", "stale"]);
    assert.equal(result[0].stage, "test");
    assert.equal(result[1].reason, "heartbeat lost");
    assert.equal(result[0].nextAction, "Inspect the failure");
    assert.equal(attentionKey(runs[0]), "failed:");
});

test("view state round trips filters and selected run", () => {
    const encoded = encodeViewState({ phase: "failed", showNoWork: true, empty: "" }, "run/7");
    assert.deepEqual(decodeViewState(encoded), {
        filters: { phase: "failed", showNoWork: true },
        selectedRun: "run/7",
    });
});

test("elapsed time preserves finished runs at the Unix epoch", () => {
    const result = deriveAttention([{
        id: "epoch",
        phase: "failed",
        startedAt: "1970-01-01T00:00:00Z",
        finishedAt: "1970-01-01T00:00:01Z",
    }], { now: Date.parse("2026-08-28T11:00:00Z") });
    assert.equal(result[0].elapsedMillis, 1000);
});

test("view state keeps boolean no-work flags and partial query strings readable", () => {
    const encoded = encodeViewState({ phase: "failed", showNoWork: true, empty: "" }, "run/7");
    assert.match(encoded, /showNoWork=/);
    assert.deepEqual(decodeViewState("phase=failed&showNoWork&run=run/7"), {
        filters: { phase: "failed", showNoWork: true },
        selectedRun: "run/7",
    });
    assert.deepEqual(decodeViewState(encoded), {
        filters: { phase: "failed", showNoWork: true },
        selectedRun: "run/7",
    });
});

test("freshness status transitions live to stale and offline as age grows", () => {
    const now = Date.parse("2026-08-28T11:00:00Z");
    assert.equal(deriveFreshnessState({ connected: true, lastUpdatedAt: now - 5000, mode: "daemon", now }), "Live");
    assert.equal(deriveFreshnessState({ connected: true, lastUpdatedAt: now - 60000, mode: "daemon", now }), "Stale");
    assert.equal(deriveFreshnessState({ connected: false, lastUpdatedAt: now - 60000, mode: "daemon", now }), "Offline");
});

test("freshness defaults to Offline for daemon mode when never updated", () => {
    const now = Date.parse("2026-08-28T11:00:00Z");
    assert.equal(deriveFreshnessState({ connected: true, lastUpdatedAt: null, mode: "daemon", now }), "Offline");
});

test("freshness defaults to Stale for polling mode when never updated", () => {
    const now = Date.parse("2026-08-28T11:00:00Z");
    assert.equal(deriveFreshnessState({ connected: true, lastUpdatedAt: null, mode: "polling", now }), "Stale");
});

test("stream routing decodes JSON events and ignores malformed payloads", () => {
    assert.deepEqual(decodeStreamEvent('{"type":"run.updated","runId":"run-7"}'), {
        type: "run.updated",
        runId: "run-7",
    });
    assert.equal(decodeStreamEvent("not-json"), null);
    assert.equal(decodeStreamEvent(""), null);
});

test("restored-filter state extracts all filter types", () => {
    const search = "?gaggle=test&workflow=build&phase=running&trigger=item&stage=deploy&outcome=success&population=attempts&showNoWork=&run=abc123";
    const { filters, selectedRun } = decodeViewState(search);
    assert.deepEqual(filters, {
        gaggle: "test",
        workflow: "build",
        phase: "running",
        trigger: "item",
        stage: "deploy",
        outcome: "success",
        population: "attempts",
        showNoWork: true,
    });
    assert.equal(selectedRun, "abc123");
});

test("partial restored-filter state decodes correctly", () => {
    // Partial query with some filters: empty string values are preserved in URLSearchParams
    const search = "?phase=failed&showNoWork&run=run/7";
    const { filters, selectedRun } = decodeViewState(search);
    assert.deepEqual(filters, {
        phase: "failed",
        showNoWork: true,
    });
    assert.equal(selectedRun, "run/7");
});

test("restored-filter execution requests a filtered run load", () => {
    assert.equal(shouldApplyRestoredFilters({ phase: "failed" }), true);
    assert.equal(shouldApplyRestoredFilters({}), false);
});

test("causal diagnosis derives attempt lineage and terminal breadcrumbs", () => {
    const run = {
        phase: "failed",
        events: [
            { type: "stage.started", stage: "build", attempt: 1, time: "2026-08-28T10:00:00Z" },
            { type: "stage.finished", stage: "build", attempt: 1, status: "failed", time: "2026-08-28T10:01:00Z", error: "npm test failed" },
            { type: "stage.started", stage: "build", attempt: 2, time: "2026-08-28T10:02:00Z" },
            { type: "stage.finished", stage: "build", attempt: 2, status: "failed", time: "2026-08-28T10:03:00Z", error: "timeout waiting for dependencies" },
        ],
        transitions: [{ source: "build", target: "final", verdict: "failed", terminal: true, status: "failed" }],
    };
    const diagnosis = deriveAttemptLineage(run);
    assert.deepEqual(diagnosis.attempts.map((item) => item.attempt), [1, 2]);
    assert.equal(diagnosis.breadcrumbs[0].detail, "failed");
    assert.equal(diagnosis.breadcrumbs[diagnosis.breadcrumbs.length - 1].label, "Latest error");
    assert.deepEqual(deriveFailureBreadcrumbs(run).map((item) => item.label), ["Terminal outcome", "Responsible stage", "Latest error"]);
});

test("transcript filtering narrows by stage, role, attempt, and text", () => {
    const entries = [
        { stage: "build", role: "assistant", attempt: 1, message: "First pass" },
        { stage: "test", role: "tool", attempt: 2, message: "Second pass" },
        { stage: "build", role: "assistant", attempt: 2, message: "Retry after timeout" },
    ];
    assert.deepEqual(filterTranscriptEntries(entries, { stage: "build", attempt: 2 }).length, 1);
    assert.deepEqual(filterTranscriptEntries(entries, { role: "tool", text: "Second" }).map((item) => item.stage), ["test"]);
});

test("cursor recovery identifies cursor failures and resets the page", () => {
    assert.equal(isInvalidCursorError("invalid cursor for runs"), true);
    assert.equal(isInvalidCursorError("daemon unavailable"), false);
    assert.deepEqual(mergeRunPage([{ id: "old" }], { runs: [{ id: "fresh" }], cursor: "next" }), {
        runs: [{ id: "fresh" }],
        cursor: "next",
        hasMore: true,
    });
});

test("pagination appends runs and exposes the next cursor", () => {
    assert.deepEqual(mergeRunPage([{ id: "run-1" }], {
        runs: [{ id: "run-2" }],
        cursor: "cursor-2",
    }, true), {
        runs: [{ id: "run-1" }, { id: "run-2" }],
        cursor: "cursor-2",
        hasMore: true,
    });
});

test("freshness transitions: deriveFreshnessState boundary at staleAfterMs", () => {
    const now = Date.parse("2026-08-28T11:00:00Z");
    const staleAfterMs = 30000;
    // Just at threshold (age = 30000): still Live
    assert.equal(
        deriveFreshnessState({ connected: true, lastUpdatedAt: now - staleAfterMs, mode: "daemon", now, staleAfterMs }),
        "Live"
    );
    // Just after threshold (age = 30001): now Stale
    assert.equal(
        deriveFreshnessState({ connected: true, lastUpdatedAt: now - (staleAfterMs + 1), mode: "daemon", now, staleAfterMs }),
        "Stale"
    );
});

test("freshness transitions: deriveFreshnessState boundary at offlineAfterMs", () => {
    const now = Date.parse("2026-08-28T11:00:00Z");
    const staleAfterMs = 30000;
    const offlineAfterMs = 120000;
    // Just at offline threshold (age = 120000): still Stale
    assert.equal(
        deriveFreshnessState({
            connected: true,
            lastUpdatedAt: now - offlineAfterMs,
            mode: "daemon",
            now,
            staleAfterMs,
            offlineAfterMs,
        }),
        "Stale"
    );
    // Just after offline threshold (age = 120001): now Offline
    assert.equal(
        deriveFreshnessState({
            connected: true,
            lastUpdatedAt: now - (offlineAfterMs + 1),
            mode: "daemon",
            now,
            staleAfterMs,
            offlineAfterMs,
        }),
        "Offline"
    );
});

test("telemetry insights distinguish partial data and preserve explicit zero units", () => {
    const result = deriveTelemetryInsights({
        startedAt: "2026-08-28T10:00:00Z",
        finishedAt: "2026-08-28T10:00:10Z",
        failureCount: 0,
        repassCount: 0,
        model: "copilot-model",
        metrics: { inputTokens: { value: 0, unit: "tokens" }, costUSD: { value: 0, unit: "USD" } },
        events: [
            { type: "stage.started", stage: "queue", attempt: 1, time: "2026-08-28T10:00:02Z" },
            { type: "stage.finished", stage: "queue", attempt: 1, status: "succeeded", time: "2026-08-28T10:00:10Z" },
        ],
    });
    assert.deepEqual(result.counts, { failures: 0, repasses: 0 });
    assert.equal(result.duration.totalMillis, 10000);
    assert.equal(result.duration.queueMillis, 2000);
    assert.deepEqual(result.usage, [
        { label: "Input tokens", value: 0, unit: "tokens" },
        { label: "Cost", value: 0, unit: "USD" },
    ]);
    assert.equal(result.budgets.length, 0);
});

test("telemetry insights aggregate retry and failure hotspots", () => {
    const result = deriveTelemetryInsights({
        events: [
            { type: "stage.finished", stage: "build", attempt: 1, status: "failed", time: "2026-08-28T10:00:00Z" },
            { type: "stage.finished", stage: "build", attempt: 2, status: "succeeded", time: "2026-08-28T10:00:01Z" },
            { type: "stage.finished", stage: "test", attempt: 1, status: "failed", time: "2026-08-28T10:00:02Z" },
        ],
    });
    assert.deepEqual(result.hotspots.slice(0, 2), [
        { stage: "build", attempts: 2, failures: 1, retries: 1, score: 2 },
        { stage: "test", attempts: 1, failures: 1, retries: 0, score: 1 },
    ]);
});
