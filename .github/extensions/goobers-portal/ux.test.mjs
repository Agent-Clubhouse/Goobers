import assert from "node:assert/strict";
import test from "node:test";

import {
    attentionKey,
    decodeStreamEvent,
    decodeViewState,
    deriveAttention,
    deriveFreshnessState,
    encodeViewState,
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
