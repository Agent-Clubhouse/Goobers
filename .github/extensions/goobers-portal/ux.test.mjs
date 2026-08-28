import assert from "node:assert/strict";
import test from "node:test";

import { attentionKey, decodeViewState, deriveAttention, deriveFreshnessState, encodeViewState } from "./ux.mjs";

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

test("stream routing: deriveFreshnessState works with various timestamps", () => {
    const now = Date.parse("2026-08-28T11:00:00Z");
    // Just connected: lastUpdatedAt is now
    assert.equal(deriveFreshnessState({ connected: true, lastUpdatedAt: now, mode: "daemon", now }), "Live");
    // 15s old: still within stale threshold (30s)
    assert.equal(deriveFreshnessState({ connected: true, lastUpdatedAt: now - 15000, mode: "daemon", now }), "Live");
    // 45s old: exceeds stale threshold but within offline threshold (120s)
    assert.equal(deriveFreshnessState({ connected: true, lastUpdatedAt: now - 45000, mode: "daemon", now }), "Stale");
    // 150s old: exceeds offline threshold
    assert.equal(deriveFreshnessState({ connected: true, lastUpdatedAt: now - 150000, mode: "daemon", now }), "Offline");
    // Disconnected: always offline
    assert.equal(deriveFreshnessState({ connected: false, lastUpdatedAt: now, mode: "daemon", now }), "Offline");
});

test("restored-filter execution: decodeViewState extracts all filter types", () => {
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

test("restored-filter execution: partial query strings decode correctly", () => {
    // Partial query with some filters: empty string values are preserved in URLSearchParams
    const search = "?phase=failed&showNoWork&run=run/7";
    const { filters, selectedRun } = decodeViewState(search);
    assert.deepEqual(filters, {
        phase: "failed",
        showNoWork: true,
    });
    assert.equal(selectedRun, "run/7");
});

test("cursor recovery: encodeViewState round-trips with cursor pagination state", () => {
    const filters = { gaggle: "prod", workflow: "test", showNoWork: true };
    const encoded = encodeViewState(filters, "");
    const decoded = decodeViewState(encoded);
    assert.deepEqual(decoded.filters, filters);
    assert.equal(decoded.selectedRun, "");
});

test("pagination: encodeViewState preserves runId across cursor-based load-more", () => {
    const filters1 = { phase: "running" };
    const encoded1 = encodeViewState(filters1, "run/1");
    const decoded1 = decodeViewState(encoded1);
    assert.equal(decoded1.selectedRun, "run/1");

    // Simulate load-more: same filters, no selected run in URL
    const filters2 = { phase: "running" };
    const encoded2 = encodeViewState(filters2, "");
    const decoded2 = decodeViewState(encoded2);
    assert.equal(decoded2.selectedRun, "");
    assert.deepEqual(decoded2.filters, filters1);
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
