// Extension: goobers-portal
// Canvas app for the Goobers workflow orchestrator: a live dashboard that
// discovers/connects to real running Goobers instances — local daemons,
// persisted GitHub Actions journals,
// (found via each instance root's `scheduler/up.lock` + `scheduler/api.address`,
// the same liveness signal `goobers dashboard` uses) or a remote control
// plane (any base URL) — and talks to the daemon's real versioned HTTP API
// (`/api/v1/...`) directly. GitHub Actions sources use the authenticated `gh`
// session and the Goobers offline trace reader. The extension never clones or
// seeds instance config.

import { createServer } from "node:http";
import { promises as fs } from "node:fs";
import path from "node:path";
import { createCanvas, joinSession, CanvasError } from "@github/copilot-sdk/extension";
import { renderHtml } from "./render.mjs";
import { listKnownSources, addSource, removeSource } from "./registry.mjs";
import { readPreferences, setThemePreference } from "./preferences.mjs";
import {
    probeSource,
    resolveSource,
    loadSnapshot,
    loadRunDetail,
    loadRunArtifact,
    loadRunTranscript,
    loadRuns,
    openEventStream,
    setWorkflowEnabled,
    startDaemon,
} from "./client.mjs";

// instanceId -> { server, url, selectedSourceId }
const servers = new Map();
let loggedInitialSourceProbeBatch = false;

function logEvent(event, details = {}) {
    process.stderr.write(`[goobers-portal] ${JSON.stringify({
        timestamp: new Date().toISOString(),
        event,
        ...details,
    })}\n`);
}

async function listSourcesWithStatus() {
    const known = await listKnownSources();
    const startedAt = Date.now();
    const logBatchDetails = !loggedInitialSourceProbeBatch;
    loggedInitialSourceProbeBatch = true;
    if (logBatchDetails) logEvent("source_probe_batch_started", { count: known.length });
    const sources = await Promise.all(known.map(async (source) => {
        const probeStartedAt = Date.now();
        try {
            const result = await probeSource(source);
            const durationMs = Date.now() - probeStartedAt;
            if (logBatchDetails || !result.connected || durationMs >= 1000) {
                logEvent("source_probe_finished", {
                    sourceId: source.id,
                    kind: source.kind,
                    connected: result.connected,
                    mode: result.mode,
                    durationMs,
                });
            }
            return result;
        } catch (err) {
            logEvent("source_probe_failed", {
                sourceId: source.id,
                kind: source.kind,
                durationMs: Date.now() - probeStartedAt,
                error: err.message || String(err),
            });
            throw err;
        }
    }));
    const durationMs = Date.now() - startedAt;
    if (logBatchDetails || durationMs >= 1000) {
        logEvent("source_probe_batch_finished", { count: sources.length, durationMs });
    }
    return sources;
}

async function snapshotFor(sourceId) {
    const known = await listKnownSources();
    const source = known.find((s) => s.id === sourceId);
    if (!source) throw new CanvasError("not_found", `unknown source ${sourceId}`);
    const resolved = await resolveSource(source);
    if (!resolved.ok) {
        logEvent("source_resolution_failed", {
            sourceId,
            kind: source.kind,
            error: resolved.reason,
        });
        return { sourceId, connected: false, reason: resolved.reason, source };
    }
    try {
        const data = await loadSnapshot(resolved);
        return { sourceId, connected: true, source, ...data };
    } catch (err) {
        logEvent("snapshot_load_failed", {
            sourceId,
            kind: source.kind,
            mode: resolved.mode,
            error: err.message || String(err),
        });
        return { sourceId, connected: false, reason: err.message || String(err), source };
    }
}

async function runsFor(sourceId, filters) {
    const known = await listKnownSources();
    const source = known.find((s) => s.id === sourceId);
    if (!source) throw new CanvasError("not_found", `unknown source ${sourceId}`);
    const resolved = await resolveSource(source);
    if (!resolved.ok) {
        logEvent("source_resolution_failed", {
            sourceId,
            kind: source.kind,
            error: resolved.reason,
        });
        return { connected: false, reason: resolved.reason };
    }

    try {
        const data = await loadRuns(resolved, filters);
        return { connected: true, ...data };
    } catch (err) {
        logEvent("runs_load_failed", {
            sourceId,
            kind: source.kind,
            mode: resolved.mode,
            error: err.message || String(err),
        });
        return { connected: false, reason: err.message || String(err) };
    }
}

async function eventStreamFor(sourceId, lastEventId, request, response) {
    const known = await listKnownSources();
    const source = known.find((entry) => entry.id === sourceId);
    if (!source) throw new CanvasError("not_found", `unknown source ${sourceId}`);
    const resolved = await resolveSource(source);
    if (!resolved.ok) throw new CanvasError("not_connected", resolved.reason);
    const controller = new AbortController();
    request.on("close", () => controller.abort());
    const upstream = await openEventStream(resolved, lastEventId, controller.signal);
    response.writeHead(200, {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-cache",
        "X-Accel-Buffering": "no",
    });
    if (upstream.body) {
        for await (const chunk of upstream.body) response.write(chunk);
    }
    response.end();
}

async function runDetailFor(sourceId, runId) {
    const known = await listKnownSources();
    const source = known.find((s) => s.id === sourceId);
    if (!source) throw new CanvasError("not_found", `unknown source ${sourceId}`);
    const resolved = await resolveSource(source);
    if (!resolved.ok) {
        logEvent("source_resolution_failed", {
            sourceId,
            kind: source.kind,
            error: resolved.reason,
        });
        return { connected: false, reason: resolved.reason };
    }

    try {
        const run = await loadRunDetail(resolved, runId);
        return { connected: true, run };
    } catch (err) {
        logEvent("run_detail_load_failed", {
            sourceId,
            runId,
            kind: source.kind,
            mode: resolved.mode,
            error: err.message || String(err),
        });
        return { connected: false, reason: err.message || String(err) };
    }
}

async function runContentFor(sourceId, runId, kind, value) {
    const known = await listKnownSources();
    const source = known.find((s) => s.id === sourceId);
    if (!source) throw new CanvasError("not_found", `unknown source ${sourceId}`);
    const resolved = await resolveSource(source);
    if (!resolved.ok) throw new Error(resolved.reason);
    return kind === "artifact"
        ? await loadRunArtifact(resolved, runId, value)
        : await loadRunTranscript(resolved, runId, value);
}

async function setWorkflowEnabledFor(sourceId, gaggle, workflow, enabled) {
    const known = await listKnownSources();
    const source = known.find((s) => s.id === sourceId);
    if (!source) throw new CanvasError("not_found", `unknown source ${sourceId}`);
    const resolved = await resolveSource(source);
    if (!resolved.ok) {
        throw new CanvasError("not_connected", resolved.reason || "source is not connected");
    }
    return await setWorkflowEnabled(resolved, gaggle, workflow, enabled);
}

async function startDaemonFor(sourceId) {
    const known = await listKnownSources();
    const source = known.find((s) => s.id === sourceId);
    if (!source) throw new CanvasError("not_found", `unknown source ${sourceId}`);
    return await startDaemon(source);
}

async function browseDirectories(requestedPath) {
    const current = path.resolve(requestedPath || process.cwd());
    const stat = await fs.stat(current).catch(() => null);
    if (!stat?.isDirectory()) throw new Error(`Directory does not exist: ${current}`);
    const entries = await fs.readdir(current, { withFileTypes: true });
    let roots = [path.parse(current).root];
    if (process.platform === "win32") {
        const candidates = Array.from({ length: 26 }, (_, index) =>
            `${String.fromCharCode(65 + index)}:\\`);
        const available = await Promise.all(candidates.map(async (candidate) =>
            (await fs.stat(candidate).catch(() => null))?.isDirectory() ? candidate : null));
        roots = available.filter(Boolean);
    }
    return {
        current,
        parent: path.dirname(current) === current ? null : path.dirname(current),
        roots,
        directories: entries
            .filter((entry) => entry.isDirectory())
            .map((entry) => ({
                name: entry.name,
                path: path.join(current, entry.name),
            }))
            .sort((a, b) => a.name.localeCompare(b.name)),
    };
}

async function startServer(instanceId) {
    const server = createServer(async (req, res) => {
        const url = new URL(req.url, "http://localhost");
        const startedAt = Date.now();
        res.on("finish", () => {
            const durationMs = Date.now() - startedAt;
            if (res.statusCode >= 400 || durationMs >= 1000) {
                logEvent("http_request_finished", {
                    instanceId,
                    method: req.method,
                    path: url.pathname,
                    statusCode: res.statusCode,
                    durationMs,
                });
            }
        });
        req.on("aborted", () => {
            logEvent("http_request_aborted", {
                instanceId,
                method: req.method,
                path: url.pathname,
                durationMs: Date.now() - startedAt,
            });
        });
        try {
            if (url.pathname === "/api/selected-source") {
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify({ sourceId: servers.get(instanceId)?.selectedSourceId || null }));
                return;
            }
            if (url.pathname === "/api/sources") {
                const sources = await listSourcesWithStatus();
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify({ sources }));
                return;
            }
            if (url.pathname === "/api/preferences") {
                if (req.method === "POST") {
                    const chunks = [];
                    for await (const chunk of req) chunks.push(chunk);
                    const body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
                    const preferences = await setThemePreference(body.theme);
                    res.setHeader("Content-Type", "application/json; charset=utf-8");
                    res.end(JSON.stringify(preferences));
                    return;
                }
                const preferences = await readPreferences();
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify(preferences));
                return;
            }
            if (url.pathname === "/api/directories") {
                const directories = await browseDirectories(url.searchParams.get("path") || process.cwd());
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify(directories));
                return;
            }
            if (url.pathname === "/api/snapshot") {
                const sourceId = url.searchParams.get("source");
                if (!sourceId) {
                    res.setHeader("Content-Type", "application/json; charset=utf-8");
                    res.end(JSON.stringify({ connected: false, reason: "no source selected" }));
                    return;
                }
                if (url.pathname === "/api/events") {
                    const sourceId = url.searchParams.get("source");
                    if (!sourceId) {
                        res.statusCode = 400;
                        res.end("source is required");
                        return;
                    }
                    await eventStreamFor(sourceId, req.headers["last-event-id"] || "", req, res);
                    return;
                }
                const entry = servers.get(instanceId);
                if (entry) entry.selectedSourceId = sourceId;
                const data = await snapshotFor(sourceId);
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify(data));
                return;
            }
            if (url.pathname === "/api/runs") {
                const sourceId = url.searchParams.get("source");
                if (!sourceId) {
                    res.setHeader("Content-Type", "application/json; charset=utf-8");
                    res.end(JSON.stringify({ connected: false, reason: "no source selected" }));
                    return;
                }
                const filters = {};
                for (const key of ["gaggle", "workflow", "stage", "outcome", "population", "phase", "trigger", "since", "until", "limit", "cursor"]) {
                    const v = url.searchParams.get(key);
                    if (v) filters[key] = v;
                }
                const data = await runsFor(sourceId, filters);
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify(data));
                return;
            }
            if (url.pathname === "/api/run") {
                const sourceId = url.searchParams.get("source");
                const runId = url.searchParams.get("id");
                if (!sourceId || !runId) {
                    res.statusCode = 400;
                    res.setHeader("Content-Type", "application/json; charset=utf-8");
                    res.end(JSON.stringify({ connected: false, reason: "source and id are required" }));
                    return;
                }
                const data = await runDetailFor(sourceId, runId);
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify(data));
                return;
            }
            if (url.pathname === "/api/run-artifact" || url.pathname === "/api/run-transcript") {
                const sourceId = url.searchParams.get("source");
                const runId = url.searchParams.get("id");
                const value = url.pathname.endsWith("artifact")
                    ? url.searchParams.get("digest")
                    : url.searchParams.get("seq");
                if (!sourceId || !runId || !value) {
                    res.statusCode = 400;
                    res.end("source, id, and resource identifier are required");
                    return;
                }
                const kind = url.pathname.endsWith("artifact") ? "artifact" : "transcript";
                const content = await runContentFor(sourceId, runId, kind, value);
                res.setHeader("Content-Type", content.mediaType);
                res.setHeader("X-Content-Type-Options", "nosniff");
                res.setHeader("Content-Security-Policy", "sandbox; default-src 'none'");
                res.end(content.bytes);
                return;
            }
            if (url.pathname === "/api/add-source" && req.method === "POST") {
                const chunks = [];
                for await (const chunk of req) chunks.push(chunk);
                const body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
                const entry = await addSource(body);
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify(entry));
                return;
            }
            if (url.pathname === "/api/remove-source" && req.method === "POST") {
                const chunks = [];
                for await (const chunk of req) chunks.push(chunk);
                const body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
                const removed = await removeSource(body.id);
                res.setHeader("Content-Type", "application/json; charset=utf-8");
                res.end(JSON.stringify({ removed }));
                return;
            }
            if (url.pathname === "/api/set-workflow-enabled" && req.method === "POST") {
                const chunks = [];
                for await (const chunk of req) chunks.push(chunk);
                const body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
                try {
                    const result = await setWorkflowEnabledFor(body.source, body.gaggle, body.workflow, !!body.enabled);
                    res.setHeader("Content-Type", "application/json; charset=utf-8");
                    res.end(JSON.stringify({ ok: true, result }));
                } catch (err) {
                    res.setHeader("Content-Type", "application/json; charset=utf-8");
                    res.end(JSON.stringify({ ok: false, reason: err.message || String(err) }));
                }
                return;
            }
            if (url.pathname === "/api/start-daemon" && req.method === "POST") {
                const chunks = [];
                for await (const chunk of req) chunks.push(chunk);
                const body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}");
                try {
                    const result = await startDaemonFor(body.source);
                    res.setHeader("Content-Type", "application/json; charset=utf-8");
                    res.end(JSON.stringify({ ok: true, ...result }));
                } catch (err) {
                    res.setHeader("Content-Type", "application/json; charset=utf-8");
                    res.end(JSON.stringify({ ok: false, reason: err.message || String(err) }));
                }
                return;
            }
            res.setHeader("Content-Type", "text/html; charset=utf-8");
            const preferences = await readPreferences();
            res.end(renderHtml(instanceId, preferences.theme));
        } catch (err) {
            logEvent("http_request_failed", {
                instanceId,
                method: req.method,
                path: url.pathname,
                durationMs: Date.now() - startedAt,
                error: err.message || String(err),
            });
            res.statusCode = 500;
            res.setHeader("Content-Type", "application/json; charset=utf-8");
            res.end(JSON.stringify({ error: err.message || String(err) }));
        }
    });
    server.on("close", () => logEvent("http_server_closed", { instanceId }));
    server.on("error", (err) => {
        logEvent("http_server_error", { instanceId, error: err.message || String(err) });
    });
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    const port = typeof address === "object" && address ? address.port : 0;
    const url = `http://127.0.0.1:${port}/`;
    logEvent("http_server_started", { instanceId, url });
    return { server, url, selectedSourceId: null };
}

const inputSchema = {
    type: "object",
    properties: {
        root: {
            type: "string",
            description: "Optional: a local goobers instance root directory to register and select on open (added to the known-sources list, not cloned).",
        },
        remoteUrl: {
            type: "string",
            description: "Optional: a remote control-plane base URL (e.g. http://host:8080) to register and select on open.",
        },
        workflowUrl: {
            type: "string",
            description: "Optional: a GitHub Actions workflow URL to register and select on open.",
        },
    },
};

const canvases = [
        createCanvas({
            id: "goobers-portal",
            displayName: "Goobers Portal",
            description: "Dashboard for live Goobers instances and persisted GitHub Actions run journals, with source selection and deep run diagnostics.",
            inputSchema,
            actions: [
                {
                    name: "list_sources",
                    description: "List known local, remote, and GitHub Actions Goobers sources with live connectivity status.",
                    handler: async () => ({ sources: await listSourcesWithStatus() }),
                },
                {
                    name: "add_local_instance",
                    description: "Register a local goobers instance root by path so the portal can discover and connect to it if a daemon is running there.",
                    inputSchema: {
                        type: "object",
                        properties: {
                            root: { type: "string", description: "Path to the goobers instance root directory." },
                            label: { type: "string", description: "Optional friendly name." },
                        },
                        required: ["root"],
                    },
                    handler: async (ctx) => await addSource({ kind: "local", value: ctx.input.root, label: ctx.input.label }),
                },
                {
                    name: "add_remote_control_plane",
                    description: "Register a remote Goobers control-plane URL (host:port or full URL) so the portal can connect to it.",
                    inputSchema: {
                        type: "object",
                        properties: {
                            url: { type: "string", description: "Base URL or host:port of the remote daemon, e.g. http://10.0.0.5:8080." },
                            label: { type: "string", description: "Optional friendly name." },
                            token: { type: "string", description: "Optional bearer token if the remote API requires auth." },
                        },
                        required: ["url"],
                    },
                    handler: async (ctx) => await addSource({ kind: "remote", value: ctx.input.url, label: ctx.input.label, token: ctx.input.token }),
                },
                {
                    name: "add_github_actions_workflow",
                    description: "Register a GitHub Actions workflow URL and use the authenticated gh session to browse runs and their uploaded Goobers journals.",
                    inputSchema: {
                        type: "object",
                        properties: {
                            url: { type: "string", description: "GitHub Actions workflow URL." },
                            label: { type: "string", description: "Optional friendly name." },
                        },
                        required: ["url"],
                    },
                    handler: async (ctx) =>
                        await addSource({ kind: "github-actions", value: ctx.input.url, label: ctx.input.label }),
                },
                {
                    name: "remove_source",
                    description: "Remove a previously registered source by its id.",
                    inputSchema: { type: "object", properties: { id: { type: "string" } }, required: ["id"] },
                    handler: async (ctx) => ({ removed: await removeSource(ctx.input.id) }),
                },
                {
                    name: "list_runs",
                    description: "List runs for a source with optional filters (gaggle, workflow, stage, outcome, population, phase, trigger, since, until, limit, cursor).",
                    inputSchema: {
                        type: "object",
                        properties: {
                            source: { type: "string" },
                            gaggle: { type: "string" },
                            workflow: { type: "string" },
                            stage: { type: "string" },
                            outcome: { type: "string" },
                            population: { type: "string" },
                            phase: { type: "string" },
                            trigger: { type: "string" },
                            since: { type: "string" },
                            until: { type: "string" },
                            limit: { type: "number" },
                            cursor: { type: "string" },
                        },
                        required: ["source"],
                    },
                    handler: async (ctx) => {
                        const { source, ...filters } = ctx.input;
                        return await runsFor(source, filters);
                    },
                },
                {
                    name: "view_run",
                    description: "Fetch full run detail (workflow graph, transition history, operator status) for a single run.",
                    inputSchema: {
                        type: "object",
                        properties: {
                            source: { type: "string", description: "Source id (from list_sources)." },
                            runId: { type: "string", description: "Run id." },
                        },
                        required: ["source", "runId"],
                    },
                    handler: async (ctx) => await runDetailFor(ctx.input.source, ctx.input.runId),
                },
                {
                    name: "set_workflow_enabled",
                    description: "Enable or disable a workflow's non-manual triggers (schedule/signal/webhook/backlog-item). Manual triggers are unaffected. Persists to the workflow's YAML and hot-reloads the daemon.",
                    inputSchema: {
                        type: "object",
                        properties: {
                            source: { type: "string", description: "Source id (from list_sources)." },
                            gaggle: { type: "string" },
                            workflow: { type: "string" },
                            enabled: { type: "boolean" },
                        },
                        required: ["source", "gaggle", "workflow", "enabled"],
                    },
                    handler: async (ctx) =>
                        await setWorkflowEnabledFor(ctx.input.source, ctx.input.gaggle, ctx.input.workflow, !!ctx.input.enabled),
                },
                {
                    name: "start_daemon",
                    description: "Start the goobers daemon (`goobers up`) for a registered local instance root that isn't currently running, then wait until its API answers health. Fails for remote control-plane sources, which must be started on their own host.",
                    inputSchema: {
                        type: "object",
                        properties: {
                            source: { type: "string", description: "Source id (from list_sources)." },
                        },
                        required: ["source"],
                    },
                    handler: async (ctx) => await startDaemonFor(ctx.input.source),
                },
                {
                    name: "refresh",
                    description: "Re-fetch the live snapshot for this canvas instance's currently selected source.",
                    handler: async (ctx) => {
                        const entry = servers.get(ctx.instanceId);
                        if (!entry) throw new CanvasError("not_found", `no open portal instance ${ctx.instanceId}`);
                        if (!entry.selectedSourceId) return { connected: false, reason: "no source selected" };
                        return await snapshotFor(entry.selectedSourceId);
                    },
                },
            ],
            open: async (ctx) => {
                let entry = servers.get(ctx.instanceId);
                logEvent("canvas_open_started", {
                    instanceId: ctx.instanceId,
                    reusedServer: Boolean(entry),
                });
                if (!entry) {
                    entry = await startServer(ctx.instanceId);
                    servers.set(ctx.instanceId, entry);
                }
                if (ctx.input && ctx.input.root) {
                    const src = await addSource({ kind: "local", value: ctx.input.root });
                    entry.selectedSourceId = src.id;
                } else if (ctx.input && ctx.input.remoteUrl) {
                    const src = await addSource({ kind: "remote", value: ctx.input.remoteUrl });
                    entry.selectedSourceId = src.id;
                } else if (ctx.input && ctx.input.workflowUrl) {
                    const src = await addSource({ kind: "github-actions", value: ctx.input.workflowUrl });
                    entry.selectedSourceId = src.id;
                }
                logEvent("canvas_open_finished", {
                    instanceId: ctx.instanceId,
                    url: entry.url,
                    selectedSourceId: entry.selectedSourceId,
                });
                return {
                    title: "Goobers Portal",
                    url: entry.url,
                };
            },
            onClose: async (ctx) => {
                const entry = servers.get(ctx.instanceId);
                logEvent("canvas_close_received", {
                    instanceId: ctx.instanceId,
                    hadServer: Boolean(entry),
                });
                if (entry) {
                    servers.delete(ctx.instanceId);
                    await new Promise((resolve) => entry.server.close(() => resolve()));
                }
            },
        }),
];

// The current SDK's createCanvas helper omits the protocol's icon field, so
// attach it to the mutable wire declaration before joining.
canvases[0].declaration.icon = "goober-mascot.png";

const session = await joinSession({
    canvases,
});

session.on("session.canvas.opened", (event) => {
    if (event.data.extensionId !== "project:goobers-portal") return;
    logEvent("canvas_open_event_observed", {
        instanceId: event.data.instanceId,
        icon: event.data.icon,
        url: event.data.url,
        reopen: event.data.reopen,
        availability: event.data.availability,
    });
});

logEvent("extension_joined", { sessionId: session.sessionId });
