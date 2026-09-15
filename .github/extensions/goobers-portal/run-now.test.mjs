import assert from "node:assert/strict";
import test from "node:test";
import { Script } from "node:vm";

import { renderHtml } from "./render.mjs";

const html = renderHtml("run-now-test");
const start = html.indexOf("  function runNowNeedsForce(");
const end = html.indexOf("  const startBarEl =", start);
assert.ok(start >= 0 && end > start, "rendered run-now handlers are present");
const script = new Script(html.slice(start, end) + "\nrunWorkflowNow;");

function deferred() {
    let resolve, reject;
    const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
    return { promise, resolve, reject };
}

function harness({ response, confirm = () => false, loadSnapshot = async () => {} }) {
    const sourceSelect = { value: "source-a" };
    const pendingWorkflowRuns = new Set();
    const requests = [];
    const dialogs = [];
    const statuses = [];
    const errors = [];
    const context = {
        sourceSelect,
        sourceSelectionEpoch: 0,
        workflowRunRequests: new Map(),
        pendingWorkflowRuns,
        loadSnapshot,
        fetch: async (_url, init) => {
            const body = JSON.parse(init.body);
            requests.push(body);
            return response(body);
        },
        window: { confirm: (message) => { dialogs.push(message); return confirm(); } },
        setRunStatus: (message) => statuses.push(message),
        errorEl: { set textContent(message) { errors.push(message); } },
    };
    const run = script.runInNewContext(context);
    const switchSource = (source) => {
        sourceSelect.value = source;
        ++context.sourceSelectionEpoch;
        context.workflowRunRequests.clear();
        pendingWorkflowRuns.clear();
    };
    return { run, sourceSelect, switchSource, pendingWorkflowRuns, requests, dialogs, statuses, errors };
}

function rejection(reason) {
    return { ok: false, code: "trigger_rejected", reason: "conditions: " + reason };
}

for (const reason of ["budget", "daily-budget"]) {
    test(`run now retries ${reason} only after accepting force confirmation`, async () => {
        const h = harness({
            confirm: () => true,
            response: ({ force }) => ({
                json: async () => force ? { ok: true, result: { runId: "forced-run" } } : rejection(reason),
            }),
        });
        await h.run("team", "implementation");
        assert.deepEqual(h.requests, [false, true].map((force) => ({
            source: "source-a", gaggle: "team", workflow: "implementation", force,
        })));
        assert.equal(h.dialogs.length, 1);
        assert.match(h.dialogs[0], /--force/);
        assert.deepEqual(h.statuses, ["Triggered implementation (forced-run)"]);
        assert.deepEqual(h.errors, []);
        assert.equal(h.pendingWorkflowRuns.size, 0);
    });
}

for (const reason of ["budget", "daily-budget", "concurrency"]) {
    test(`run now does not retry ${reason} without force approval`, async () => {
        const h = harness({ response: () => ({ json: async () => rejection(reason) }) });
        await h.run("team", "implementation");
        assert.equal(h.requests.length, 1);
        assert.equal(h.requests[0].force, false);
        assert.equal(h.dialogs.length, reason === "concurrency" ? 0 : 1);
        assert.deepEqual(h.statuses, []);
        assert.deepEqual(h.errors, ["Failed to run implementation: conditions: " + reason]);
        assert.equal(h.pendingWorkflowRuns.size, 0);
    });
}

for (const outcome of ["success", "rejected", "budget", "fetch-error", "json-error"]) {
    test(`run now ignores late ${outcome} after a source switch`, async () => {
        const entered = deferred();
        const result = deferred();
        const h = harness({
            confirm: () => true,
            response: () => {
                if (outcome === "fetch-error") {
                    entered.resolve();
                    return result.promise;
                }
                return { json: () => { entered.resolve(); return result.promise; } };
            },
        });
        const running = h.run("team", "implementation");
        await entered.promise;
        h.switchSource("source-b");
        // A run on B may now use the same key.
        h.pendingWorkflowRuns.add("team/implementation");
        if (outcome.endsWith("-error")) result.reject(new Error("late failure"));
        else result.resolve(outcome === "success"
            ? { ok: true, result: { runId: "late-run" } }
            : rejection(outcome === "budget" ? "budget" : "concurrency"));
        await running;
        assert.equal(h.requests.length, 1);
        assert.deepEqual(h.dialogs, []);
        assert.deepEqual(h.statuses, []);
        assert.deepEqual(h.errors, []);
        assert.ok(h.pendingWorkflowRuns.has("team/implementation"));
    });
}

for (const outcome of ["rejected", "fetch-error"]) {
    test(`run now does not write ${outcome} after switching sources during final refresh`, async () => {
        const entered = deferred();
        const refreshed = deferred();
        let snapshots = 0;
        const h = harness({
            response: () => {
                if (outcome === "fetch-error") throw new Error("failed request");
                return { json: async () => rejection("concurrency") };
            },
            loadSnapshot: async () => {
                if (++snapshots === 2) {
                    entered.resolve();
                    await refreshed.promise;
                }
            },
        });
        const running = h.run("team", "implementation");
        await entered.promise;
        h.switchSource("source-b");
        refreshed.resolve();
        await running;
        assert.deepEqual(h.statuses, []);
        assert.deepEqual(h.errors, []);
    });
}

for (const roundTrip of [false, true]) {
    for (const outcome of ["success", "rejected", "budget", "fetch-error", "json-error"]) {
        test(`old ${outcome} cannot overwrite a newer run ${roundTrip ? "after A-B-A" : "on the same source"}`, async () => {
            const entered = [deferred(), deferred()];
            const results = [deferred(), deferred()];
            let index = 0;
            const h = harness({
                confirm: () => true,
                response: () => {
                    const current = index++;
                    if (current === 0 && outcome === "fetch-error") {
                        entered[current].resolve();
                        return results[current].promise;
                    }
                    return { json: () => { entered[current].resolve(); return results[current].promise; } };
                },
            });
            const oldRun = h.run("team", "implementation");
            await entered[0].promise;
            if (roundTrip) {
                h.switchSource("source-b");
                h.switchSource("source-a");
            }
            const newRun = h.run("team", "implementation");
            await entered[1].promise;
            if (outcome.endsWith("-error")) results[0].reject(new Error("stale failure"));
            else results[0].resolve(outcome === "success"
                ? { ok: true, result: { runId: "stale-run" } }
                : rejection(outcome === "budget" ? "budget" : "concurrency"));
            await oldRun;
            assert.ok(h.pendingWorkflowRuns.has("team/implementation"), "new run remains pending");
            assert.deepEqual(h.statuses, []);
            assert.deepEqual(h.errors, []);
            assert.deepEqual(h.dialogs, []);
            assert.equal(h.requests.length, 2);
            results[1].resolve({ ok: true, result: { runId: "new-run" } });
            await newRun;
            assert.deepEqual(h.statuses, ["Triggered implementation (new-run)"]);
            assert.equal(h.pendingWorkflowRuns.size, 0);
        });
    }
}

for (const proceed of [false, true]) {
    for (const roundTrip of [false, true]) {
        test(`source change during ${proceed ? "accepted" : "declined"} force confirmation${roundTrip ? " and back" : ""} invalidates the run`, async () => {
            const h = harness({
                response: () => ({ json: async () => rejection("budget") }),
                confirm: () => {
                    h.switchSource("source-b");
                    if (roundTrip) h.switchSource("source-a");
                    return proceed;
                },
            });
            await h.run("team", "implementation");
            assert.equal(h.dialogs.length, 1);
            assert.equal(h.requests.length, 1);
            assert.deepEqual(h.statuses, []);
            assert.deepEqual(h.errors, []);
            assert.equal(h.pendingWorkflowRuns.size, 0);
        });
    }
}

test("an older final refresh cannot clear or report an error over a newer run", async () => {
    const oldRefresh = deferred();
    const refreshing = deferred();
    const newResult = deferred();
    const newEntered = deferred();
    let snapshots = 0;
    let requests = 0;
    const h = harness({
        loadSnapshot: async () => {
            if (++snapshots === 2) {
                refreshing.resolve();
                await oldRefresh.promise;
            }
        },
        response: () => ({
            json: async () => {
                if (++requests === 1) return rejection("concurrency");
                newEntered.resolve();
                return newResult.promise;
            },
        }),
    });
    const oldRun = h.run("team", "implementation");
    await refreshing.promise;
    const newRun = h.run("team", "implementation");
    await newEntered.promise;
    oldRefresh.resolve();
    await oldRun;
    assert.deepEqual(h.errors, []);
    assert.deepEqual(h.statuses, []);
    assert.ok(h.pendingWorkflowRuns.has("team/implementation"));
    newResult.resolve({ ok: true, result: { runId: "new-run" } });
    await newRun;
    assert.deepEqual(h.statuses, ["Triggered implementation (new-run)"]);
    assert.equal(h.pendingWorkflowRuns.size, 0);
});

test("force retry is bounded even if the budget is rejected again", async () => {
    const h = harness({
        confirm: () => true,
        response: () => ({ json: async () => rejection("budget") }),
    });
    await h.run("team", "implementation");
    assert.deepEqual(h.requests.map(({ force }) => force), [false, true]);
    assert.equal(h.dialogs.length, 1);
    assert.deepEqual(h.errors, ["Failed to run implementation: conditions: budget"]);
    assert.equal(h.pendingWorkflowRuns.size, 0);
});
