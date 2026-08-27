// Talks to a real Goobers daemon over its versioned HTTP API
// (`/api/v1/...`, see internal/apicontract + internal/httpapi in the goobers
// repo) instead of shelling out to the CLI or cloning config into a
// throwaway instance directory.
//
// Two kinds of source:
//   - "local"  — a filesystem instance root. We discover whether a live
//                `goobers up` daemon is attached to it by reading
//                `<root>/scheduler/up.lock` (mtime = last heartbeat, same
//                liveness signal `goobers dashboard` uses) and
//                `<root>/scheduler/api.address` (the address the daemon
//                published on startup). We never guess the default address:
//                another local instance may own it, and accepting that health
//                response would silently connect this source to the wrong
//                daemon.
//   - "remote" — a user-supplied base URL (host:port or full URL) for a
//                control-plane daemon running elsewhere. No filesystem
//                probing; we just hit its /api/v1/health directly.

import { promises as fs } from "node:fs";
import path from "node:path";
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import {
    loadActionsArtifact,
    loadActionsRunDetail,
    loadActionsRuns,
    loadActionsSnapshot,
    loadActionsTranscript,
    parseWorkflowURL,
    probeActionsSource,
} from "./actions-source.mjs";

const execFileAsync = promisify(execFile);
const GOOBERS_BIN = process.platform === "win32" ? "goobers.exe" : "goobers";

const FETCH_TIMEOUT_MS = 4000;

function withScheme(address) {
    return /^https?:\/\//i.test(address) ? address : `http://${address}`;
}

async function statMTime(file) {
    try {
        const st = await fs.stat(file);
        return st.mtimeMs;
    } catch {
        return null;
    }
}

async function readAddressFile(file) {
    try {
        const raw = await fs.readFile(file, "utf8");
        const line = raw.split("\n")[0].trim();
        return line || null;
    } catch {
        return null;
    }
}

async function fetchJSON(url, { token, timeoutMs = FETCH_TIMEOUT_MS } = {}) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
        const res = await fetch(url, {
            signal: controller.signal,
            headers: token ? { Authorization: `Bearer ${token}` } : undefined,
        });
        const text = await res.text();
        let body;
        try {
            body = text ? JSON.parse(text) : undefined;
        } catch {
            body = text;
        }
        if (!res.ok) {
            const err = new Error(`HTTP ${res.status}${body?.error ? `: ${body.error}` : ""}`);
            err.status = res.status;
            err.body = body;
            throw err;
        }
        return body;
    } finally {
        clearTimeout(timer);
    }
}

/** PUT/POST a JSON body against a daemon endpoint and return the parsed JSON response (or throw on non-2xx). */
/**
 * PUT/POST a JSON body against a daemon endpoint and return the parsed JSON
 * response (or throw on non-2xx).
 *
 * Mutations trigger a synchronous config re-materialize + reload inside the
 * daemon, during which it drops pooled keep-alive connections. Node's fetch
 * surfaces that as UND_ERR_SOCKET / ECONNRESET on a socket it had already
 * reused. Since the request never reached a handler in that case, retrying is
 * safe and turns a spurious failure into a successful call.
 */
async function sendJSON(method, url, payload, { token, timeoutMs = FETCH_TIMEOUT_MS, retries = 2 } = {}) {
    let lastErr;
    for (let attempt = 0; attempt <= retries; attempt++) {
        try {
            return await sendJSONOnce(method, url, payload, { token, timeoutMs });
        } catch (err) {
            lastErr = err;
            const transient = /UND_ERR_SOCKET|ECONNRESET|ECONNREFUSED|EPIPE|other side closed/i.test(err.message || "");
            if (!transient || attempt === retries) throw err;
            await new Promise((r) => setTimeout(r, 750 * (attempt + 1)));
        }
    }
    throw lastErr;
}

async function sendJSONOnce(method, url, payload, { token, timeoutMs = FETCH_TIMEOUT_MS } = {}) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
        const headers = { "Content-Type": "application/json", Connection: "close" };
        if (token) headers.Authorization = "Bearer " + token;
        const res = await fetch(url, {
            method,
            signal: controller.signal,
            headers,
            body: JSON.stringify(payload ?? {}),
        });
        const text = await res.text();
        let body;
        try {
            body = text ? JSON.parse(text) : undefined;
        } catch {
            body = text;
        }
        if (!res.ok) {
            const err = new Error(`HTTP ${res.status}${body?.error ? `: ${body.error}` : ""}`);
            err.status = res.status;
            err.body = body;
            throw err;
        }
        return body;
    } catch (err) {
        // Node's fetch collapses every transport-level problem into a bare
        // "fetch failed" and hides the real reason on `err.cause`. Unwrap it so
        // callers and the UI get something actionable (ECONNREFUSED, DNS, ...).
        if (err && err.name === "AbortError") {
            throw new Error(`${method} ${url} timed out after ${timeoutMs}ms`);
        }
        if (err && err.message === "fetch failed") {
            const cause = err.cause;
            const detail = cause ? cause.code || cause.message || String(cause) : "unknown transport error";
            throw new Error(`${method} ${url} failed: ${detail}`);
        }
        throw err;
    } finally {
        clearTimeout(timer);
    }
}

/**
 * Probe a local instance root for a live daemon. Returns
 * { live, baseUrl, reason } — live=false with a human-readable reason when
 * no reachable daemon is found (does NOT throw; callers show reason in UI).
 *
 * Liveness is determined by actually reaching /api/v1/health, not by lock
 * heartbeat age — a healthy daemon's up.lock heartbeat cadence can exceed
 * any fixed staleness window while idle, so treat the lock/address files
 * only as "where to look," and the health call as the real liveness check.
 */
async function resolveLocalBaseUrl(root) {
    const schedulerDir = path.join(root, "scheduler");
    const lockPath = path.join(schedulerDir, "up.lock");
    const addressPath = path.join(schedulerDir, "api.address");

    const lockMTime = await statMTime(lockPath);
    if (lockMTime === null) {
        return { live: false, reason: "no scheduler/up.lock — `goobers up` has never run here, or this isn't an instance root" };
    }

    const address = await readAddressFile(addressPath);
    if (!address) {
        return {
            live: false,
            reason: "no scheduler/api.address — the daemon is not running or did not finish startup",
        };
    }
    const baseUrl = withScheme(address);
    try {
        await fetchJSON(`${baseUrl}/api/v1/health`, { timeoutMs: 2000 });
        return { live: true, baseUrl };
    } catch (err) {
        return { live: false, reason: `daemon not responding at ${address}: ${err.message || err}` };
    }
}

async function hasInstanceConfig(root) {
    try {
        await fs.access(path.join(root, "instance.yaml"));
        return true;
    } catch {
        return false;
    }
}

/**
 * Resolve a registry source entry (or ad-hoc {kind,value,token}) into a
 * connection descriptor:
 *   { ok, mode: "daemon"|"standalone", baseUrl?, root?, token?, reason? }
 *
 * "daemon" means a live `goobers up` process answered health at baseUrl.
 * "standalone" means no daemon is running, but the root has a real
 * instance.yaml — the same no-daemon fallback `goobers dashboard` itself
 * uses, read via `goobers status --json` / `goobers runs list --json`
 * against that root (no cloning, no seeding, just reading what's there).
 */
export async function resolveSource(source) {
    if (source.kind === "github-actions") {
        try {
            return { ok: true, mode: "actions", ...source, source, ...parseWorkflowURL(source.value) };
        } catch (err) {
            return { ok: false, reason: err.message || String(err) };
        }
    }
    if (source.kind === "remote") {
        return { ok: true, mode: "daemon", baseUrl: withScheme(source.value), token: source.token };
    }
    if (source.kind === "local") {
        const probe = await resolveLocalBaseUrl(source.value);
        if (probe.live) return { ok: true, mode: "daemon", baseUrl: probe.baseUrl, token: source.token };
        if (await hasInstanceConfig(source.value)) {
            return { ok: true, mode: "standalone", root: source.value, standaloneReason: probe.reason };
        }
        return { ok: false, reason: probe.reason };
    }
    return { ok: false, reason: `unknown source kind: ${source.kind}` };
}

/** Live status (not persisted) for a source: reachable + health payload. */
export async function probeSource(source) {
    if (source.kind === "github-actions") {
        try {
            return await probeActionsSource(source);
        } catch (err) {
            return { ...source, connected: false, reason: err.message || String(err) };
        }
    }
    const resolved = await resolveSource(source);
    if (!resolved.ok) {
        return { ...source, connected: false, reason: resolved.reason };
    }
    if (resolved.mode === "standalone") {
        try {
            const status = await runGoobersJSON(resolved.root, ["status"]);
            return { ...source, connected: true, mode: "standalone", health: { ready: true, standalone: true, warnings: status.warnings || [] } };
        } catch (err) {
            return { ...source, connected: false, reason: cliErrorMessage(err) };
        }
    }
    try {
        const health = await fetchJSON(`${resolved.baseUrl}/api/v1/health`, { token: resolved.token });
        return { ...source, connected: true, mode: "daemon", baseUrl: resolved.baseUrl, health };
    } catch (err) {
        return { ...source, connected: false, baseUrl: resolved.baseUrl, reason: err.message || String(err) };
    }
}

function cliErrorMessage(err) {
    return err && err.stderr ? String(err.stderr).trim() : String(err.message || err);
}

async function runGoobersJSON(root, args) {
    const { stdout } = await execFileAsync(GOOBERS_BIN, [...args, "--json", root], {
        timeout: 15000,
        maxBuffer: 10 * 1024 * 1024,
    });
    return JSON.parse(stdout);
}

/** Standalone (no-daemon) snapshot, read directly off disk via the CLI's own --json reader. */
async function loadStandaloneSnapshot(root) {
    const [status, runsList] = await Promise.all([
        runGoobersJSON(root, ["status"]),
        runGoobersJSON(root, ["runs", "list"]).catch((err) => ({ runs: [], error: cliErrorMessage(err) })),
    ]);
    const summary = status.summary || { workflows: [] };
    const workflows = (summary.workflows || []).map((w) => ({
        identity: { gaggle: w.gaggle, name: w.workflow },
        gaggle: w.gaggle,
        concurrency: { activeRuns: w.inFlight || 0, maxConcurrentRuns: w.maxConcurrentRuns || 0 },
    }));
    return {
        mode: "standalone",
        health: { ready: true, standalone: true },
        instance: { name: path.basename(root), warnings: status.warnings || [] },
        gaggles: [],
        workflows,
        runs: runsList.runs || status.runs || [],
    };
}

/** Build a runs query string from filter options (all optional). */
function buildRunsQuery(filters = {}) {
    const params = new URLSearchParams();
    params.set("limit", String(filters.limit || 50));
    for (const key of ["gaggle", "workflow", "stage", "outcome", "population", "phase", "trigger", "since", "until", "cursor"]) {
        if (filters[key]) params.set(key, filters[key]);
    }
    return params.toString();
}

/** Fetch just the runs list for a resolved connection, with optional filters. */
export async function loadRuns(resolved, filters = {}) {
    if (resolved.mode === "actions") return await loadActionsRuns(resolved, filters);
    if (resolved.mode === "standalone") {
        const snapshot = await loadStandaloneSnapshot(resolved.root);
        return { runs: snapshot.runs };
    }
    const { baseUrl, token } = resolved;
    try {
        const runs = await fetchJSON(`${baseUrl}/api/v1/runs?${buildRunsQuery(filters)}`, { token });
        return { runs: runs.runs || [], cursor: runs.cursor };
    } catch (err) {
        return { runs: [], error: err.message || String(err) };
    }
}

/** Fetch the full portal snapshot for a resolved connection. */
export async function loadSnapshot(resolved, runFilters = {}) {
    if (resolved.mode === "actions") return await loadActionsSnapshot(resolved, runFilters);
    if (resolved.mode === "standalone") {
        return await loadStandaloneSnapshot(resolved.root);
    }
    const { baseUrl, token } = resolved;
    const [health, instance, gaggles, portalConfig] = await Promise.all([
        fetchJSON(`${baseUrl}/api/v1/health`, { token }),
        fetchJSON(`${baseUrl}/api/v1/instance`, { token }),
        fetchJSON(`${baseUrl}/api/v1/gaggles`, { token }),
        fetchJSON(`${baseUrl}/api/v1/portal/config`, { token }).catch(() => null),
    ]);

    const gaggleNames = (gaggles.items || []).map((g) => g.name);
    const workflowsByGaggle = await Promise.all(
        gaggleNames.map((name) =>
            fetchJSON(`${baseUrl}/api/v1/gaggles/${encodeURIComponent(name)}/workflows`, { token }).catch((err) => ({
                items: [],
                error: err.message || String(err),
            })),
        ),
    );
    const workflows = gaggleNames.flatMap((name, i) => (workflowsByGaggle[i].items || []).map((w) => ({ ...w, gaggle: name })));

    let runs = { runs: [] };
    try {
        runs = await fetchJSON(`${baseUrl}/api/v1/runs?${buildRunsQuery(runFilters)}`, { token });
    } catch (err) {
        runs = { runs: [], error: err.message || String(err) };
    }

    return { baseUrl, health, instance, gaggles: gaggles.items || [], workflows, runs: runs.runs || [], capabilities: portalConfig?.capabilities || {} };
}

/**
 * Enable or disable a workflow's non-manual triggers (WF-010). Only
 * supported in "daemon" mode: standalone reads have no mutation endpoint to
 * call. Returns { gaggle, workflow, enabled } on success; throws on failure
 * (e.g. daemon lacks the capability, or workflow/gaggle isn't found).
 */
export async function setWorkflowEnabled(resolved, gaggle, workflow, enabled) {
    if (resolved.mode === "standalone") {
        throw new Error("Enabling/disabling workflows is not available in standalone (no-daemon) mode.");
    }
    const { baseUrl, token } = resolved;
    const path = `/api/v1/gaggles/${encodeURIComponent(gaggle)}/workflows/${encodeURIComponent(workflow)}/enabled`;
    // This is not a plain read: the daemon rewrites the workflow YAML and then
    // synchronously re-materializes + reloads the whole config set before
    // answering, which routinely takes longer than the default read timeout.
    return await sendJSON("PUT", `${baseUrl}${path}`, { enabled }, { token, timeoutMs: 60000 });
}

/**
 * Start the goobers daemon (`goobers up`) for a registered source.
 *
 * Only meaningful for "local" sources: the extension runs on this machine, so
 * it can only spawn a process against a filesystem instance root. A "remote"
 * source is just a base URL — there is no way to start a process on another
 * host over the daemon's read API, so we fail with a clear message rather than
 * pretending.
 *
 * The child is fully detached (own process group / no console window on
 * Windows) and its stdio is discarded, so it survives the extension process
 * exiting — matching how a user would run `goobers up` in their own terminal.
 * We then poll /api/v1/health until it answers, because startup routinely
 * takes ~15s (preflight + config materialization + scheduler warmup).
 */
export async function startDaemon(source, { timeoutMs = 90000, onProgress } = {}) {
    if (source.kind === "remote") {
        const loopback = /^(https?:\/\/)?(127\.0\.0\.1|localhost|\[::1\])(:|\/|$)/i.test(source.value);
        throw new Error(
            loopback
                ? `${source.value} is registered as a *remote* control-plane URL, so the portal only knows a URL — not which instance root it belongs to, and can't start it. Since it's on this machine, add it as a local source (its instance root directory) instead, and the Start daemon button will work.`
                : "This source is a remote control-plane URL. The portal can't start a daemon on another host — start `goobers up` there, then reconnect.",
        );
    }
    if (source.kind !== "local") {
        throw new Error(`unknown source kind: ${source.kind}`);
    }
    const root = source.value;

    const already = await resolveLocalBaseUrl(root);
    if (already.live) {
        return { started: false, alreadyRunning: true, baseUrl: already.baseUrl };
    }
    if (!(await hasInstanceConfig(root))) {
        throw new Error(`${root} has no instance.yaml — it isn't a goobers instance root.`);
    }

    let child;
    const logPath = path.join(root, "scheduler", "portal-up.log");
    let logFd = null;
    try {
        await fs.mkdir(path.dirname(logPath), { recursive: true });
        const handle = await fs.open(logPath, "w");
        logFd = handle.fd;
        // Keep the FileHandle alive for the life of the child; closing the
        // handle object would close the fd out from under the detached process.
        startDaemon._logHandles = startDaemon._logHandles || new Set();
        startDaemon._logHandles.add(handle);
    } catch {
        logFd = null;
    }

    try {
        child = spawn(GOOBERS_BIN, ["up", "--quiet", root], {
            cwd: root,
            detached: true,
            stdio: logFd === null ? "ignore" : ["ignore", logFd, logFd],
            windowsHide: true,
        });
    } catch (err) {
        throw new Error(`failed to launch ${GOOBERS_BIN}: ${err.message || err}`);
    }

    // Track spawn errors and early exits. `goobers up` refuses to start on a
    // preflight/config failure and exits non-zero within a second or two —
    // without this we'd poll health for the full timeout and then report a
    // misleading "didn't answer in time" instead of the actual error.
    let spawnError = null;
    let exitInfo = null;
    child.once("error", (err) => {
        spawnError = err;
    });
    child.once("exit", (code, signal) => {
        exitInfo = { code, signal };
    });

    await new Promise((r) => setTimeout(r, 500));
    if (spawnError) {
        throw new Error(
            `failed to launch ${GOOBERS_BIN}: ${spawnError.message || spawnError} — is the goobers binary on PATH?`,
        );
    }
    child.unref();

    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
        await new Promise((r) => setTimeout(r, 1500));
        const probe = await resolveLocalBaseUrl(root);
        if (probe.live) {
            return { started: true, pid: child.pid, baseUrl: probe.baseUrl, logPath };
        }
        if (exitInfo && exitInfo.code !== 0) {
            const detail = await readLogTail(logPath);
            throw new Error(
                `\`goobers up\` exited with code ${exitInfo.code} before the API came up.${detail ? `\n\n${detail}` : ` See ${logPath}.`}`,
            );
        }
        if (onProgress) onProgress(probe.reason);
    }
    throw new Error(
        `started goobers up (pid ${child.pid}) but it didn't answer /api/v1/health within ${Math.round(timeoutMs / 1000)}s — see ${logPath}.`,
    );
}

/**
 * Read the tail of a daemon startup log, preferring lines that look like the
 * actual failure (`error:` / `FATAL`) over the wall of preflight warnings
 * `goobers up` prints before it fails.
 */
async function readLogTail(logPath, maxLines = 12) {
    try {
        const raw = await fs.readFile(logPath, "utf8");
        const lines = raw.split(/\r?\n/).filter((l) => l.trim());
        if (lines.length === 0) return "";
        const errors = lines.filter((l) => /^\s*(error|fatal|panic)\b/i.test(l));
        const chosen = errors.length ? errors : lines.slice(-maxLines);
        return chosen.join("\n");
    } catch {
        return "";
    }
}

/**
 * Fetch full run detail (graph, transitions, operator) for a single run.
 * Only supported in "daemon" mode — standalone/CLI reads don't expose the
 * per-run graph+transitions payload the real portal's run/replay view needs.
 */
export async function loadRunDetail(resolved, runId) {
    if (resolved.mode === "actions") return await loadActionsRunDetail(resolved, runId);
    if (resolved.mode === "standalone") {
        throw new Error("Run detail is not available in standalone (no-daemon) mode.");
    }
    const { baseUrl, token } = resolved;
    const encodedRun = encodeURIComponent(runId);
    const [detail, eventList] = await Promise.all([
        fetchJSON(`${baseUrl}/api/v1/runs/${encodedRun}`, { token }),
        fetchJSON(`${baseUrl}/api/v1/runs/${encodedRun}/events`, { token }),
    ]);
    if (detail.operator?.pullRequest?.provider === "github" &&
        detail.operator.pullRequest.url &&
        !detail.operator.pullRequestBody) {
        try {
            const { stdout } = await execFileAsync("gh", [
                "pr", "view", detail.operator.pullRequest.url, "--json", "body",
            ], {
                timeout: 30000,
                maxBuffer: 10 * 1024 * 1024,
                windowsHide: true,
            });
            const pull = JSON.parse(stdout);
            detail.operator = {
                ...detail.operator,
                pullRequestBody: pull.body || "",
                diagnosticsLimitations: (detail.operator.diagnosticsLimitations || [])
                    .filter((item) => !String(item).startsWith("pull request description unavailable:")),
            };
        } catch (err) {
            const reason = cliErrorMessage(err);
            const existing = detail.operator.diagnosticsLimitations || [];
            if (!existing.some((item) => String(item).startsWith("pull request description unavailable:"))) {
                detail.operator = {
                    ...detail.operator,
                    diagnosticsLimitations: [
                        ...existing,
                        `pull request description unavailable: ${reason}`,
                    ],
                };
            }
        }
    }
    return { ...detail, events: eventList.events || [] };
}

async function fetchRunContent(resolved, resourcePath) {
    if (resolved.mode === "standalone") {
        throw new Error("Run content is not available in standalone (no-daemon) mode.");
    }
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);
    try {
        const headers = resolved.token ? { Authorization: `Bearer ${resolved.token}` } : undefined;
        const response = await fetch(`${resolved.baseUrl}${resourcePath}`, {
            signal: controller.signal,
            headers,
        });
        if (!response.ok) {
            throw new Error(`HTTP ${response.status}: ${await response.text()}`);
        }
        return {
            bytes: Buffer.from(await response.arrayBuffer()),
            mediaType: response.headers.get("content-type") || "application/octet-stream",
        };
    } finally {
        clearTimeout(timer);
    }
}

export async function loadRunArtifact(resolved, runId, digest) {
    if (resolved.mode === "actions") {
        return await loadActionsArtifact(resolved, runId, digest);
    }
    return await fetchRunContent(
        resolved,
        `/api/v1/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(digest)}`,
    );
}

export async function loadRunTranscript(resolved, runId, seq) {
    if (resolved.mode === "actions") {
        return await loadActionsTranscript(resolved, runId, seq);
    }
    return await fetchRunContent(
        resolved,
        `/api/v1/runs/${encodeURIComponent(runId)}/transcripts/${encodeURIComponent(seq)}`,
    );
}
