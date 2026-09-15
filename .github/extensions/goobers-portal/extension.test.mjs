import assert from "node:assert/strict";
import test from "node:test";
import { createServer } from "node:http";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { addSource } from "./registry.mjs";
import { renderFleetPortalLink } from "./render.mjs";

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
            "view_run", "set_workflow_enabled", "run_workflow_now", "start_daemon", "refresh",
        ],
    );
});

test("extension opens a server and dispatches actions and HTTP requests", async (t) => {
    const open = canvas.declaration.open;
    const opened = await open({ instanceId: "test-instance", input: {} });
    try {
        const refresh = canvas.declaration.actions.find(({ name }) => name === "refresh");
        assert.deepEqual(await refresh.handler({ instanceId: "test-instance" }), {
            connected: false,
            reason: "no source selected",
        });

        await t.test("remote daemon Fleet metadata survives the extension snapshot and clears when absent", async (t) => {
            const home = await fs.mkdtemp(path.join(os.tmpdir(), "goobers-portal-fleet-"));
            const previousHome = process.env.COPILOT_HOME;
            process.env.COPILOT_HOME = home;
            t.after(async () => {
                if (previousHome === undefined) delete process.env.COPILOT_HOME;
                else process.env.COPILOT_HOME = previousHome;
                await fs.rm(home, { recursive: true, force: true });
            });
            const warnings = [{
                code: "MODEL002",
                severity: "warning",
                scope: "Goober/coder",
                explanation: "requested model is unavailable",
            }];
            let fleet = { associated: true, canonicalUri: "https://fleet.example.test/", fleetId: "test-fleet" };
            let fail = false;
            const daemon = createServer((req, res) => {
                if (fail) {
                    res.writeHead(503).end();
                    return;
                }
                const pathname = new URL(req.url, "http://localhost").pathname;
                const body = pathname === "/api/v1/instance" ? { name: "remote-instance", fleet, warnings }
                    : pathname === "/api/v1/health" ? { ready: true }
                    : pathname === "/api/v1/gaggles" ? { items: [] }
                    : pathname === "/api/v1/runs" ? { runs: [] }
                    : {};
                res.setHeader("Content-Type", "application/json");
                res.end(JSON.stringify(body));
            });
            await new Promise((resolve) => daemon.listen(0, "127.0.0.1", resolve));
            t.after(() => new Promise((resolve) => daemon.close(resolve)));
            const source = await addSource({ kind: "remote", value: `http://127.0.0.1:${daemon.address().port}` });
            const opened = await canvas.declaration.open({ instanceId: "fleet-test", input: {} });
            t.after(() => canvas.declaration.onClose({ instanceId: "fleet-test" }));
            const snapshot = async () => {
                const response = await fetch(`${opened.url}api/snapshot?source=${encodeURIComponent(source.id)}`);
                assert.equal(response.status, 200);
                return response.json();
            };
            const associated = await snapshot();
            assert.equal(associated.connected, true);
            assert.deepEqual(associated.fleet, fleet);
            assert.deepEqual(associated.instance.warnings, warnings);
            assert.match(renderFleetPortalLink(associated.fleet), /href="https:\/\/fleet\.example\.test\/"/);

            fleet = { associated: false };
            assert.deepEqual((await snapshot()).fleet, fleet);
            fleet = undefined;
            const unassociated = await snapshot();
            assert.equal(unassociated.connected, true);
            assert.deepEqual(unassociated.fleet, { available: false });
            assert.equal(renderFleetPortalLink(unassociated.fleet), "");

            fail = true;
            const unavailable = await snapshot();
            assert.equal(unavailable.connected, false);
            assert.match(unavailable.reason, /503/);
            assert.equal(renderFleetPortalLink(unavailable.fleet), "");
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
