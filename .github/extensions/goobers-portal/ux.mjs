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

function attentionWorkSource(run) {
    const operator = run?.operator || {};
    const issue = operator.issue || run?.issue || operator.workItem || run?.workItem;
    if (issue) {
        const identity = issue.url || issue.externalId || issue.number || issue.id;
        if (identity !== undefined && identity !== null && identity !== "") {
            return `work:${issue.provider || issue.kind || ""}:${identity}`;
        }
    }
    const pullRequest = operator.pullRequest || run?.pullRequest;
    if (pullRequest) {
        const identity = pullRequest.url || pullRequest.externalId || pullRequest.number || pullRequest.id;
        if (identity !== undefined && identity !== null && identity !== "") {
            return `pr:${pullRequest.provider || ""}:${identity}`;
        }
    }
    const trigger = run?.trigger || {};
    if (trigger.ref || trigger.kind) return `trigger:${trigger.kind || ""}:${trigger.ref || ""}`;
    return `run:${run?.runId || run?.id || ""}`;
}

function attentionRecency(run, index) {
    for (const value of [run?.startedAt, run?.createdAt, run?.lastActivityAt, run?.updatedAt, run?.finishedAt]) {
        const parsed = Date.parse(value || "");
        if (Number.isFinite(parsed)) return parsed;
    }
    return -index;
}

export function deriveAttention(runs, { now = Date.now(), limit = 8 } = {}) {
    const latestByWork = new Map();
    (runs || []).forEach((run, index) => {
        const group = `${run?.gaggle || ""}/${run?.workflow || ""}|${attentionWorkSource(run)}`;
        const recency = attentionRecency(run, index);
        const existing = latestByWork.get(group);
        if (!existing || recency > existing.recency) latestByWork.set(group, { run, recency });
    });

    return [...latestByWork.values()]
        .map(({ run, recency }) => {
            const phase = runPhase(run);
            const stale = String(run?.operator?.liveness || run?.liveness || "").toLowerCase() === "stale" ||
                phase === "stale";
            const concurrencyBlocked = Boolean(run?.concurrencyBlocked || run?.operator?.concurrencyBlocked);
            if (!ATTENTION_PHASES.has(phase) && !stale && !concurrencyBlocked) return null;
            const kind = stale ? "stale" : concurrencyBlocked ? "concurrency-blocked" : phase;
            return { recency, item: {
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
            } };
        })
        .filter(Boolean)
        .sort((a, b) => b.recency - a.recency)
        .slice(0, Math.max(0, limit))
        .map(({ item }) => item);
}

const BOOLEAN_FILTER_KEYS = new Set(["showNoWork"]);

export function normalizeViewFilters(filters = {}) {
    const normalized = {};
    for (const [key, value] of Object.entries(filters || {})) {
        if (value === undefined || value === null || value === "") continue;
        if (Array.isArray(value)) {
            const values = [...new Set(value.map(String).map((item) => item.trim()).filter(Boolean))];
            if (values.length) normalized[key] = values;
            continue;
        }
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
        if (Array.isArray(value)) {
            for (const item of value) params.append(key, item);
            continue;
        }
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
        if (Object.hasOwn(filters, key)) {
            filters[key] = Array.isArray(filters[key]) ? [...filters[key], value] : [filters[key], value];
        } else {
            filters[key] = value;
        }
    }
    return { filters, selectedRun };
}

export function decodeStreamEvent(data) {
    if (data && typeof data === "object") return data;
    if (typeof data !== "string" || !data.trim()) return null;
    try {
        const event = JSON.parse(data);
        return event && typeof event === "object" ? event : null;
    } catch {
        return null;
    }
}

export function mergeRunPage(existingRuns = [], page = {}, append = false) {
    const runs = page.runs || [];
    return {
        runs: append ? [...existingRuns, ...runs] : runs,
        cursor: page.cursor || "",
        hasMore: Boolean(page.cursor),
    };
}

export function isInvalidCursorError(error = "") {
    return /cursor/i.test(String(error));
}

export function shouldApplyRestoredFilters(filters = {}) {
    return Object.keys(filters).length > 0;
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

export function asString(value) {
    if (value === null || value === undefined) return "";
    if (typeof value === "string") return value.trim();
    if (typeof value === "number" || typeof value === "boolean") return String(value);
    if (typeof value === "object") {
        if (typeof value.message === "string") return value.message.trim();
        if (typeof value.error === "string") return value.error.trim();
        if (typeof value.reason === "string") return value.reason.trim();
        if (typeof value.text === "string") return value.text.trim();
        if (typeof value.content === "string") return value.content.trim();
        try {
            return JSON.stringify(value);
        } catch {
            return String(value);
        }
    }
    return String(value).trim();
}

export function deriveAttemptLineage(run = {}) {
    const events = Array.isArray(run.events) ? run.events : [];
    const transitions = Array.isArray(run.transitions) ? run.transitions : [];
    const stageEvents = events
        .filter((event) => event && (event.stage || event.name))
        .map((event) => {
            const stage = event.stage || event.name || "unknown";
            const attempt = Number(event.attempt ?? event.runAttempt ?? event.policyAttempt ?? event.attemptNumber ?? 1);
            const state = event.status || event.verdict || event.decision || (event.type === "stage.started" ? "running" : "completed");
            return {
                stage,
                kind: event.type && String(event.type).startsWith("gate.") ? "gate" : "stage",
                attempt: Number.isFinite(attempt) && attempt > 0 ? attempt : 1,
                type: event.type,
                status: String(state).toLowerCase(),
                time: event.time || event.startedAt || event.finishedAt || "",
                seq: Number(event.seq) || 0,
                reason: asString(event.reason || event.rationale || event.error),
            };
        })
        .sort((a, b) => (a.time || "").localeCompare(b.time || "") || a.seq - b.seq);

    const byStageAttempt = new Map();
    for (const item of stageEvents) {
        const key = `${item.kind}|${item.stage}|${item.attempt}`;
        const entry = byStageAttempt.get(key) || { stage: item.stage, kind: item.kind, attempt: item.attempt, events: [], states: [] };
        entry.events.push(item);
        entry.states.push(item.status);
        byStageAttempt.set(key, entry);
    }

    const attempts = [...byStageAttempt.values()].map((item) => {
        const start = item.events[0]?.time || "";
        const end = item.events[item.events.length - 1]?.time || start;
        const status = item.states[item.states.length - 1] || item.states[0] || "pending";
        return {
            stage: item.stage,
            kind: item.kind,
            attempt: item.attempt,
            status,
            start,
            end,
            events: item.events,
        };
    }).sort((a, b) => a.stage.localeCompare(b.stage) || a.attempt - b.attempt);

    const terminal = [...transitions].reverse().find((transition) => transition && transition.terminal);
    const breadcrumbs = [];
    if (terminal) {
        breadcrumbs.push({ label: "Terminal outcome", detail: terminal.verdict || terminal.status || run.phase || "terminal", kind: "terminal" });
        if (terminal.source) {
            const attempt = attempts.filter((entry) => entry.stage === terminal.source).slice(-1)[0]?.attempt || 1;
            breadcrumbs.push({ label: "Responsible stage", detail: terminal.source, attempt, kind: "stage" });
        }
    }
    const lastError = [...events].reverse().find((event) => {
        const candidate = asString(event?.error || event?.reason || event?.rationale || event?.raw || event?.outputs?.error || event?.latestError?.message);
        return !!candidate && /error|failed|timeout|blocked|escalated|reject|panic/i.test(candidate);
    });
    const errorText = lastError ? asString(lastError.error || lastError.reason || lastError.rationale || lastError.raw || lastError.outputs?.error || lastError.latestError?.message) : "";
    if (errorText) breadcrumbs.push({ label: "Latest error", detail: errorText, kind: "error" });

    return {
        attempts,
        lineage: attempts,
        breadcrumbs: breadcrumbs.filter((entry) => entry.detail),
        terminal,
    };
}

export function deriveFailureBreadcrumbs(run = {}) {
    return deriveAttemptLineage(run).breadcrumbs;
}

export function numericValue(value) {
    if (typeof value === "number" && Number.isFinite(value)) return value;
    if (typeof value === "string" && value.trim() !== "" && Number.isFinite(Number(value))) return Number(value);
    if (value && typeof value === "object" && Number.isFinite(Number(value.value))) return Number(value.value);
    return null;
}

export function explicitMeasure(source, keys, requireUnit = false) {
    for (const key of keys) {
        if (!Object.prototype.hasOwnProperty.call(source || {}, key)) continue;
        const value = numericValue(source[key]);
        const unit = typeof source[key] === "object" && source[key] !== null
            ? String(source[key].unit || "").trim()
            : "";
        if (value !== null && (!requireUnit || unit)) return { value, unit: unit || key };
    }
    return null;
}

export function measureFromPayload(payload, keys, requireUnit = false) {
    for (const source of [payload, payload?.telemetry, payload?.usage, payload?.metrics]) {
        const result = explicitMeasure(source, keys, requireUnit);
        if (result) return result;
    }
    return null;
}

export function deriveTelemetryInsights(run = {}) {
    const diagnosis = deriveAttemptLineage(run);
    const attempts = diagnosis.attempts || [];
    const durations = attempts
        .map((entry) => [Date.parse(entry.start), Date.parse(entry.end)])
        .filter(([start, end]) => Number.isFinite(start) && Number.isFinite(end) && end >= start);
    const execution = measureFromPayload(run, ["executionMillis", "executionDurationMillis"]);
    const runDuration = measureFromPayload(run, ["durationMillis", "runDurationMillis"]) ||
        (Number.isFinite(Date.parse(run.startedAt)) && Number.isFinite(Date.parse(run.finishedAt))
            ? { value: Date.parse(run.finishedAt) - Date.parse(run.startedAt), unit: "durationMillis" }
            : null);
    const mergedDurations = durations
        .sort((a, b) => a[0] - b[0])
        .reduce((merged, [start, end]) => {
            const previous = merged[merged.length - 1];
            if (previous && start <= previous[1]) previous[1] = Math.max(previous[1], end);
            else merged.push([start, end]);
            return merged;
        }, []);
    const derivedExecutionMillis = mergedDurations.reduce((sum, [start, end]) => sum + end - start, 0);
    const executionMillis = execution?.value ?? derivedExecutionMillis;
    const executionKnown = execution?.value !== undefined || durations.length > 0;
    const queueWait = measureFromPayload(run, ["queueWaitMillis", "queueWaitDurationMillis", "queueDurationMillis"]);
    const totalMillis = runDuration?.value ?? null;
    const queueMillis = queueWait?.value ?? (
        totalMillis !== null && executionKnown && totalMillis >= executionMillis
            ? totalMillis - executionMillis
            : null
    );

    const stageMap = new Map();
    for (const attempt of attempts) {
        const item = stageMap.get(attempt.stage) || { stage: attempt.stage, attempts: 0, failures: 0, retries: 0 };
        item.attempts += 1;
        if (["failed", "failure", "error", "blocked"].includes(attempt.status)) item.failures += 1;
        if (attempt.attempt > 1) item.retries += 1;
        stageMap.set(attempt.stage, item);
    }
    const hotspots = [...stageMap.values()]
        .map((item) => ({ ...item, score: item.failures + item.retries }))
        .sort((a, b) => b.score - a.score || b.failures - a.failures || a.stage.localeCompare(b.stage));
    const eventFailures = (run.events || []).filter((event) =>
        ["failed", "failure", "error", "blocked"].includes(String(event?.status || event?.verdict || "").toLowerCase()),
    ).length;
    const hasEvents = Array.isArray(run.events);
    const failures = Object.prototype.hasOwnProperty.call(run, "failureCount")
        ? numericValue(run.failureCount)
        : (hasEvents ? eventFailures : (hotspots.length ? hotspots.reduce((sum, item) => sum + item.failures, 0) : null));
    const hasTransitions = Array.isArray(run.transitions);
    const repasses = Object.prototype.hasOwnProperty.call(run, "repassCount")
        ? numericValue(run.repassCount)
        : (hasTransitions ? run.transitions.filter((transition) => transition?.repass).length : null);

    const model = run.model || run.telemetry?.model || null;
    const usage = [];
    const usageKeys = [
        ["Input tokens", ["inputTokens", "input_tokens", "gen_ai.usage.input_tokens"]],
        ["Output tokens", ["outputTokens", "output_tokens", "gen_ai.usage.output_tokens"]],
        ["Tokens", ["tokens", "totalTokens", "total_tokens"]],
        ["Premium requests", ["copilotPremiumRequests", "premiumRequests", "goobers.usage.copilot_premium_requests"]],
        ["Cost", ["costUSD", "cost_usd", "goobers.usage.cost_usd"]],
    ];
    for (const [label, keys] of usageKeys) {
        const measure = measureFromPayload(run, keys, true);
        if (measure) usage.push({ label, value: measure.value, unit: measure.unit });
    }
    const budgets = [];
    for (const [label, keys] of [
        ["Token budget", ["maxTokens", "max_tokens"]],
        ["Cost budget", ["maxCostUSD", "max_cost_usd"]],
    ]) {
        const measure = measureFromPayload(run, keys, true);
        if (measure) budgets.push({ label, value: measure.value, unit: measure.unit });
    }
    return {
        duration: { totalMillis, queueMillis, executionMillis: executionKnown ? executionMillis : null },
        counts: { failures, repasses },
        hotspots,
        model,
        usage,
        budgets,
    };
}

export function filterTranscriptEntries(entries = [], filters = {}) {
    const stageFilter = String(filters.stage || "").trim().toLowerCase();
    const roleFilter = String(filters.role || "").trim().toLowerCase();
    const attemptFilter = filters.attempt;
    const textFilter = String(filters.text || "").trim().toLowerCase();

    return (entries || []).filter((entry) => {
        const stage = String(entry?.stage || entry?.stageName || entry?.component || "").toLowerCase();
        const role = String(entry?.role || entry?.actor || entry?.author || entry?.speaker || "").toLowerCase();
        const attempt = Number(entry?.attempt ?? entry?.attemptNumber ?? entry?.runAttempt ?? 0);
        const payload = asString(entry?.message || entry?.content || entry?.text || entry?.body || entry?.entry || entry || "").toLowerCase();

        if (stageFilter && !stage.includes(stageFilter)) return false;
        if (roleFilter && !role.includes(roleFilter)) return false;
        if (attemptFilter !== undefined && attemptFilter !== "" && attemptFilter !== null) {
            const target = Number(attemptFilter);
            if (Number.isFinite(target) && target !== attempt) return false;
        }
        if (textFilter && !payload.includes(textFilter)) return false;
        return true;
    });
}
