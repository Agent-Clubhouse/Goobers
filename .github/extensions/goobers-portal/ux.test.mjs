import assert from "node:assert/strict";
import test from "node:test";

import { attentionKey, decodeViewState, deriveAttention, encodeViewState, mutationPayload } from "./ux.mjs";

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
        filters: { phase: "failed", showNoWork: "true" },
        selectedRun: "run/7",
    });
});

test("mutationPayload preserves audited fields and omits absent values", () => {
    assert.deepEqual(mutationPayload({
        actor: "operator",
        idempotencyKey: "key",
        sequence: 4,
        visit: 2,
        branch: "approve",
        rationale: "safe",
    }), {
        actor: "operator",
        idempotencyKey: "key",
        sequence: 4,
        visit: 2,
        branch: "approve",
        rationale: "safe",
    });
});
