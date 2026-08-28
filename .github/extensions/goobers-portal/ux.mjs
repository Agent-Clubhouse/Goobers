const ATTENTION_PHASES = new Set(["failed", "escalated", "blocked", "awaiting-human"]);

function runPhase(run) {
    return String(run?.phase || run?.status || "").toLowerCase();
}

function currentStage(run) {
    return run?.currentStage || run?.stage || run?.causalStage || run?.operator?.stage || "";
}

function reasonFor(run, phase) {
    return run?.operator?.latestError?.message ||
        run?.latestError?.message ||
        run?.reason ||
        run?.operator?.trajectory ||
        (phase === "awaiting-human" ? "Waiting for a human decision" : `${phase} run`);
}

function elapsedMillis(run, now) {
    const start = Date.parse(run?.startedAt || "");
    if (!Number.isFinite(start)) return null;
    const parsedEnd = Date.parse(run?.finishedAt || "");
    const end = Number.isFinite(parsedEnd) ? parsedEnd : now;
    return Math.max(0, end - start);
}

export function attentionKey(run) {
    return `${run?.runId || run?.id || ""}:${run?.lastActivityAt || run?.updatedAt || run?.eventSeq || ""}`;
}

export function deriveAttention(runs, { now = Date.now(), limit = 8 } = {}) {
    return (runs || [])
        .map((run) => {
            const phase = runPhase(run);
            const stale = String(run?.operator?.liveness || run?.liveness || "").toLowerCase() === "stale" ||
                phase === "stale";
            const concurrencyBlocked = Boolean(run?.concurrencyBlocked || run?.operator?.concurrencyBlocked);
            if (!ATTENTION_PHASES.has(phase) && !stale && !concurrencyBlocked) return null;
            const kind = stale ? "stale" : concurrencyBlocked ? "concurrency-blocked" : phase;
            return {
                id: run?.runId || run?.id,
                workflow: run?.workflow || "",
                gaggle: run?.gaggle || "",
                phase: kind,
                stage: currentStage(run),
                reason: reasonFor(run, kind),
                elapsedMillis: elapsedMillis(run, now),
                issue: run?.operator?.issue || run?.issue,
                pullRequest: run?.operator?.pullRequest || run?.pullRequest,
                nextAction: run?.nextAction || run?.operator?.nextAction ||
                    (kind === "failed" ? "Inspect the failure" :
                        kind === "escalated" || kind === "awaiting-human" ? "Review and decide" :
                            kind === "blocked" || kind === "concurrency-blocked" ? "Resolve the blocker" : "Inspect run activity"),
                key: attentionKey(run),
            };
        })
        .filter(Boolean)
        .sort((a, b) => (b.elapsedMillis ?? -1) - (a.elapsedMillis ?? -1))
        .slice(0, Math.max(0, limit));
}

const BOOLEAN_FILTER_KEYS = new Set(["showNoWork"]);

export function normalizeViewFilters(filters = {}) {
    const normalized = {};
    for (const [key, value] of Object.entries(filters || {})) {
        if (value === undefined || value === null || value === "") continue;
        if (BOOLEAN_FILTER_KEYS.has(key)) {
            if (value === true || value === "true" || value === "1") normalized[key] = true;
            else if (value === false || value === "false" || value === "0") normalized[key] = false;
            continue;
        }
        normalized[key] = value;
    }
    return normalized;
}

export function encodeViewState(filters = {}, selectedRun = "") {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(normalizeViewFilters(filters))) {
        if (value === false) continue;
        if (value === true) {
            params.set(key, "");
            continue;
        }
        params.set(key, String(value));
    }
    if (selectedRun) params.set("run", selectedRun);
    return params.toString();
}

export function decodeViewState(search = "") {
    const params = new URLSearchParams(search.startsWith("?") ? search.slice(1) : search);
    const selectedRun = params.get("run") || "";
    const filters = {};
    for (const [key, value] of params.entries()) {
        if (key === "run") continue;
        if (BOOLEAN_FILTER_KEYS.has(key)) {
            filters[key] = value === "" || value === "true" || value === "1";
            continue;
        }
        filters[key] = value;
    }
    return { filters, selectedRun };
}

export function deriveFreshnessState({ lastUpdatedAt, connected = true, mode = "daemon", now = Date.now(), staleAfterMs = 30000, offlineAfterMs = 120000 } = {}) {
    if (!connected) return "Offline";
    if (!lastUpdatedAt) return mode === "daemon" ? "Offline" : "Stale";
    const age = now - Number(lastUpdatedAt);
    if (!Number.isFinite(age)) return "Offline";
    if (age <= staleAfterMs) return "Live";
    if (age <= offlineAfterMs) return "Stale";
    return "Offline";
}
