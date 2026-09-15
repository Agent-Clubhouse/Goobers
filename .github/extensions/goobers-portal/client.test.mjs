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
import { renderRunAssociations } from "./render.mjs";

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

test("daemon run summaries hydrate associated issue refs from run events", async () => {
    const originalFetch = globalThis.fetch;
    const requests = [];
    globalThis.fetch = async (url) => {
        requests.push(url);
        if (String(url).includes("/api/v1/runs?")) {
            return new Response(JSON.stringify({
                runs: [{
                    id: "run-one",
                    operator: {
                        issue: { number: "159", title: "Implement classifier" },
                    },
                }],
            }), { status: 200 });
        }
        return new Response(JSON.stringify({
            events: [
                {
                    externalRef: {
                        provider: "github",
                        kind: "issue",
                        id: "159",
                        url: "https://github.com/octo/app/issues/159",
                    },
                },
            ],
        }), { status: 200 });
    };
    try {
        const result = await loadRuns({ mode: "daemon", baseUrl: "http://daemon" });
        assert.equal(requests.length, 2);
        assert.deepEqual(result.runs[0].externalRefs, [{
            provider: "github",
            kind: "issue",
            id: "159",
            url: "https://github.com/octo/app/issues/159",
        }]);
    } finally {
        globalThis.fetch = originalFetch;
    }
});

for (const terminal of [false, true]) {
    test(`${terminal ? "terminal cached" : "live"} hydration keeps newly associated refs on refresh`, async () => {
        const originalFetch = globalThis.fetch;
        const issue = { kind: "issue", id: "159", url: "https://github.com/octo/app/issues/159" };
        const pr = { kind: "pr", id: "42", url: "https://github.com/octo/app/pull/42" };
        let refreshes = 0;
        let eventReads = 0;
        globalThis.fetch = async (url) => {
            if (String(url).includes("/api/v1/runs?")) {
                refreshes++;
                return Response.json({ runs: [{
                    id: `refresh-${terminal}`,
                    terminal,
                    operator: { issue: { number: "159" } },
                    externalRefs: terminal && refreshes > 1 ? [pr] : [],
                }] });
            }
            eventReads++;
            const refs = !terminal && refreshes > 1 ? [issue, pr] : [issue];
            return Response.json({ events: refs.map((externalRef) => ({ externalRef })) });
        };
        try {
            const resolved = { mode: "daemon", baseUrl: "http://association-refresh" };
            const first = await loadRuns(resolved);
            assert.deepEqual(first.runs[0].externalRefs, [issue]);
            const second = await loadRuns(resolved);
            assert.equal(eventReads, terminal ? 1 : 2);
            assert.equal(second.runs[0].externalRefs.length, 2);
            const html = renderRunAssociations(second.runs[0]);
            assert.ok(html.includes(`href="${issue.url}"`));
            assert.ok(html.includes(`href="${pr.url}"`));
        } finally {
            globalThis.fetch = originalFetch;
        }
    });
}

for (const scenario of ["terminal advances", "terminal resumes", "empty run advances"]) {
    test(`association cache invalidates when ${scenario}`, async () => {
        const originalFetch = globalThis.fetch;
        const issue = { kind: "issue", id: "159", url: "https://github.com/octo/app/issues/159" };
        const pr = { kind: "pr", id: "42", url: "https://github.com/octo/app/pull/42" };
        let refreshes = 0;
        let eventReads = 0;
        globalThis.fetch = async (url) => {
            if (String(url).includes("/api/v1/runs?")) {
                refreshes++;
                return Response.json({ runs: [{
                    id: scenario,
                    terminal: scenario === "terminal advances" ||
                        (scenario === "terminal resumes" && refreshes === 1),
                    lastActivityAt: `2026-09-15T03:00:0${refreshes}Z`,
                    operator: { issue: { number: "159" } },
                }] });
            }
            eventReads++;
            const refs = refreshes > 1 ? [issue, pr] : scenario === "empty run advances" ? [] : [issue];
            return Response.json({ events: refs.map((externalRef) => ({ externalRef })) });
        };
        try {
            const resolved = { mode: "daemon", baseUrl: "http://association-revisions" };
            await loadRuns(resolved);
            const second = await loadRuns(resolved);
            assert.equal(eventReads, 2);
            assert.deepEqual(second.runs[0].externalRefs, [issue, pr]);
        } finally {
            globalThis.fetch = originalFetch;
        }
    });
}

test("terminal hydration invalidates when only the structural lastSeq advances", async () => {
    const originalFetch = globalThis.fetch;
    const issue = { kind: "issue", id: "159", url: "https://github.com/octo/app/issues/159" };
    const pr = { kind: "pr", id: "42", url: "https://github.com/octo/app/pull/42" };
    let refreshes = 0;
    let eventReads = 0;
    globalThis.fetch = async (url) => {
        if (String(url).includes("/api/v1/runs?")) {
            return Response.json({ runs: [{
                id: "unstamped-event",
                terminal: true,
                phase: "completed",
                lastSeq: ++refreshes,
                finishedAt: "2026-09-15T03:00:00Z",
                lastActivityAt: "2026-09-15T03:00:00Z",
                operator: { issue: { number: "159" } },
            }] });
        }
        eventReads++;
        return Response.json({ events: (refreshes === 1 ? [issue] : [issue, pr])
            .map((externalRef) => ({ externalRef })) });
    };
    try {
        const resolved = { mode: "daemon", baseUrl: "http://association-sequence" };
        await loadRuns(resolved);
        const second = await loadRuns(resolved);
        assert.equal(eventReads, 2);
        assert.deepEqual(second.runs[0].externalRefs, [issue, pr]);
        assert.ok(renderRunAssociations(second.runs[0]).includes(`href="${pr.url}"`));
    } finally {
        globalThis.fetch = originalFetch;
    }
});

for (const outcome of ["empty", "failed"]) {
    test(`live hydration retries ${outcome} events with an unchanged summary and revision`, async () => {
        const originalFetch = globalThis.fetch;
        const issue = { kind: "issue", id: "159", url: "https://github.com/octo/app/issues/159" };
        let eventReads = 0;
        globalThis.fetch = async (url) => {
            if (String(url).includes("/api/v1/runs?")) {
                return Response.json({ runs: [{
                    id: outcome,
                    terminal: false,
                    phase: "running",
                    lastSeq: 1,
                    lastActivityAt: "2026-09-15T03:00:00Z",
                    operator: { issue: { number: "159" } },
                }] });
            }
            eventReads++;
            if (eventReads === 1) {
                return outcome === "empty"
                    ? Response.json({ events: [] })
                    : Response.json({ error: "temporarily unavailable" }, { status: 503 });
            }
            return Response.json({ events: [{ externalRef: issue }] });
        };
        try {
            const resolved = { mode: "daemon", baseUrl: "http://association-live-retry" };
            const first = await loadRuns(resolved);
            if (outcome === "failed") {
                assert.match(first.runs[0].operator.diagnosticsLimitations[0], /temporarily unavailable/);
            }
            const second = await loadRuns(resolved);
            assert.equal(eventReads, 2);
            assert.deepEqual(second.runs[0].externalRefs, [issue]);
            assert.equal(second.runs[0].operator.diagnosticsLimitations, undefined);
        } finally {
            globalThis.fetch = originalFetch;
        }
    });
}

test("terminal hydration caches are isolated by credential including anonymous access", async () => {
    const originalFetch = globalThis.fetch;
    const tokens = ["test-credential-a", "test-credential-b", undefined];
    const refs = tokens.map((_, index) => ({
        kind: "issue", id: "159", url: `https://github.com/octo/repo-${index}/issues/159`,
    }));
    const eventReads = [0, 0, 0];
    globalThis.fetch = async (url, options) => {
        const index = tokens.findIndex((token) => options.headers?.Authorization ===
            (token ? `Bearer ${token}` : undefined));
        assert.notEqual(index, -1);
        if (String(url).includes("/api/v1/runs?")) {
            return Response.json({ runs: [{
                id: "same-run",
                terminal: true,
                lastSeq: 1,
                operator: { issue: { number: "159" } },
            }] });
        }
        eventReads[index]++;
        return Response.json({ events: [{ externalRef: refs[index] }] });
    };
    try {
        for (let refresh = 0; refresh < 2; refresh++) {
            for (const [index, token] of tokens.entries()) {
                const result = await loadRuns({ mode: "daemon", baseUrl: "http://shared-daemon", token });
                assert.deepEqual(result.runs[0].externalRefs, [refs[index]]);
            }
        }
        assert.deepEqual(eventReads, [1, 1, 1], "reuse only the matching credential's cache");
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
