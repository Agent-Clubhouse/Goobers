import assert from "node:assert/strict";
import test from "node:test";

process.env.GOOBERS_PORTAL_TEST = "1";
const { canvases, session } = await import("./extension.mjs");
const canvas = canvases[0];

test("extension registers the Portal canvas and all supported actions", () => {
    assert.equal(canvas.declaration.id, "goobers-portal");
    assert.equal(canvas.declaration.displayName, "Goobers Portal");
    assert.deepEqual(session.canvases, canvases);
    assert.deepEqual(
        canvas.declaration.actions.map(({ name }) => name),
        [
            "list_sources", "add_local_instance", "add_remote_control_plane",
            "add_github_actions_workflow", "remove_source", "list_runs",
            "view_run", "set_workflow_enabled", "start_daemon", "refresh",
        ],
    );
});

test("extension opens a server and dispatches actions and HTTP requests", async () => {
    const open = canvas.declaration.open;
    const opened = await open({ instanceId: "test-instance", input: {} });
    try {
        const refresh = canvas.declaration.actions.find(({ name }) => name === "refresh");
        assert.deepEqual(await refresh.handler({ instanceId: "test-instance" }), {
            connected: false,
            reason: "no source selected",
        });

        const selected = await fetch(`${opened.url}api/selected-source`);
        assert.equal(selected.status, 200);
        assert.deepEqual(await selected.json(), { sourceId: null });

        const failedAction = await fetch(`${opened.url}api/run-action`, {
            method: "POST",
            headers: { "content-type": "application/json" },
            body: JSON.stringify({ source: "local:missing", action: "cancel", runId: "run", stage: "stage" }),
        });
        assert.equal(failedAction.status, 200);
        assert.deepEqual(await failedAction.json(), {
            ok: false,
            code: "not_found",
            reason: "unknown source local:missing",
        });
    } finally {
        await canvas.declaration.onClose({ instanceId: "test-instance" });
    }
});
