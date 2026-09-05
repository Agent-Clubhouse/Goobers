import assert from "node:assert/strict";
import test from "node:test";

import {
    interventionCapability,
    interventionIdempotencyKey,
    loadRuns,
    requireDurableInterventionResult,
    runStageIntervention,
    validateIntervention,
} from "./client.mjs";

test("multi-value filters fan out daemon queries and merge unique runs", async () => {
    const originalFetch = globalThis.fetch;
    const requests = [];
    globalThis.fetch = async (url) => {
        requests.push(url);
        const phase = new URL(url).searchParams.get("phase");
        return new Response(JSON.stringify({
            runs: [{ id: phase, phase, startedAt: phase === "failed" ? "2026-08-28T02:00:00Z" : "2026-08-28T01:00:00Z" }],
        }), { status: 200 });
    };
    try {
        const result = await loadRuns(
            { mode: "daemon", baseUrl: "http://daemon" },
            { phase: ["running", "failed"] },
        );
        assert.equal(requests.length, 2);
        assert.deepEqual(result.runs.map((run) => run.id), ["failed", "running"]);
        assert.equal(result.cursor, "");
    } finally {
        globalThis.fetch = originalFetch;
    }
});

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
    assert.equal(interventionCapability({ revealRun: true }, "approve"), true);
    assert.equal(interventionCapability({ revealRun: false }, "approve"), false);
    assert.equal(interventionCapability({ revealRun: true }, "rerun"), true);
    assert.equal(
        interventionIdempotencyKey("run/1", "stage/one", "rerun"),
        interventionIdempotencyKey("run/1", "stage/one", "rerun"),
    );
});

test("run intervention sends encoded path and reuses its idempotency key", async () => {
    const originalFetch = globalThis.fetch;
    const requests = [];
    globalThis.fetch = async (url, options) => {
        requests.push({ url, options });
        if (requests.length === 1) {
            throw new Error("fetch failed", { cause: { code: "ECONNRESET" } });
        }
        return new Response(JSON.stringify({ journalSeq: 8 }), { status: 200 });
    };
    try {
        const resolved = { mode: "daemon", baseUrl: "http://daemon", token: "token" };
        const input = { actor: "operator", instructionAddendum: "retry safely" };
        const first = await runStageIntervention(resolved, "rerun", "run/one", "stage/one", input);
        const second = await runStageIntervention(resolved, "rerun", "run/one", "stage/one", input);
        assert.equal(requests[0].url, "http://daemon/api/v1/runs/run%2Fone/stages/stage%2Fone/rerun");
        assert.equal(requests[0].options.headers["Idempotency-Key"], first.idempotencyKey);
        assert.equal(requests[1].options.headers["Idempotency-Key"], first.idempotencyKey);
        assert.equal(requests[2].options.headers["Idempotency-Key"], first.idempotencyKey);
        assert.equal(second.idempotencyKey, first.idempotencyKey);
    } finally {
        globalThis.fetch = originalFetch;
    }
});

test("run intervention surfaces typed server errors", async () => {
    const originalFetch = globalThis.fetch;
    globalThis.fetch = async () => new Response(JSON.stringify({
        error: { code: "stage_paused", message: "stage is not paused" },
    }), { status: 409 });
    try {
        await assert.rejects(
            runStageIntervention(
                { mode: "daemon", baseUrl: "http://daemon" },
                "approve", "run", "stage", { actor: "operator", decision: "pass" },
            ),
            (error) => error.code === "stage_paused" && error.message.includes("stage is not paused"),
        );
    } finally {
        globalThis.fetch = originalFetch;
    }
});
