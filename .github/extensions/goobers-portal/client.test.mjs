import assert from "node:assert/strict";
import test from "node:test";

import {
    interventionCapability,
    interventionIdempotencyKey,
    requireDurableInterventionResult,
    validateIntervention,
} from "./client.mjs";

test("run interventions validate actor and action-specific fields", () => {
    assert.throws(() => validateIntervention("approve", { decision: "pass" }), /actor is required/);
    assert.throws(() => validateIntervention("approve", { actor: "operator" }), /decision=pass/);
    assert.throws(() => validateIntervention("override", { actor: "operator" }), /rationale/);
    assert.throws(() => validateIntervention("rerun", { actor: "operator" }), /addendum/);
    assert.doesNotThrow(() => validateIntervention("approve", { actor: "operator", decision: "pass" }));
});

test("durable confirmation rejects missing or zero journal positions", () => {
    assert.throws(() => requireDurableInterventionResult({ phase: "running", journalSeq: 0 }), /durable journal/);
    assert.deepEqual(requireDurableInterventionResult({ phase: "running", journalSeq: 4 }), {
        phase: "running", journalSeq: 4,
    });
});

test("run action capabilities are gated and idempotency keys are reused", () => {
    assert.equal(interventionCapability({ approve: true }, "approve"), true);
    assert.equal(interventionCapability({ approve: false }, "approve"), false);
    assert.equal(interventionCapability({ approve: true }, "rerun"), false);
    assert.equal(
        interventionIdempotencyKey("run/1", "stage/one", "rerun"),
        interventionIdempotencyKey("run/1", "stage/one", "rerun"),
    );
});
