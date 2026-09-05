import { promises as fs } from "node:fs";
import path from "node:path";
import os from "node:os";
import crypto from "node:crypto";
import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const GOOBERS_BIN = process.platform === "win32" ? "goobers.exe" : "goobers";
const workflowCache = new Map();
const HOSTED_PROGRESS_SCHEMA = "goobers.dev/hosted-progress/v1";
const HOSTED_PROGRESS_START = "<!-- goobers-progress:v1 -->";
const HOSTED_PROGRESS_END = "<!-- /goobers-progress:v1 -->";

function copilotHome() {
    return process.env.COPILOT_HOME || path.join(os.homedir(), ".copilot");
}

function actionsCacheRoot(source) {
    const key = crypto.createHash("sha256")
        .update(source.value || source.workflowURL)
        .digest("hex")
        .slice(0, 16);
    return path.join(copilotHome(), "extensions", "goobers-portal", "artifacts", "github-actions", key);
}

export function parseWorkflowURL(value) {
    let url;
    try {
        url = new URL(value);
    } catch {
        throw new Error("Enter a GitHub Actions workflow URL.");
    }
    const parts = url.pathname.split("/").filter(Boolean);
    if (url.hostname.toLowerCase() !== "github.com" ||
        parts.length < 5 ||
        parts[2] !== "actions" ||
        parts[3] !== "workflows") {
        throw new Error("Expected https://github.com/<owner>/<repo>/actions/workflows/<workflow>.");
    }
    return {
        owner: parts[0],
        repo: parts[1],
        workflow: decodeURIComponent(parts[4]),
        workflowURL: `https://github.com/${parts[0]}/${parts[1]}/actions/workflows/${encodeURIComponent(decodeURIComponent(parts[4]))}`,
    };
}

async function ghJSON(args, timeout = 30000) {
    try {
        const { stdout } = await execFileAsync("gh", args, {
            timeout,
            maxBuffer: 50 * 1024 * 1024,
            windowsHide: true,
        });
        return JSON.parse(stdout);
    } catch (err) {
        const detail = String(err.stderr || err.message || err).trim();
        throw new Error(`GitHub CLI request failed: ${detail}. Authenticate with gh auth login.`);
    }
}

async function workflowMetadata(resolved) {
    const key = `${resolved.owner}/${resolved.repo}/${resolved.workflow}`;
    const cached = workflowCache.get(key);
    if (cached && Date.now() - cached.loadedAt < 30000) return cached.value;
    const value = await ghJSON([
        "api",
        `repos/${resolved.owner}/${resolved.repo}/actions/workflows/${encodeURIComponent(resolved.workflow)}`,
    ]);
    workflowCache.set(key, { loadedAt: Date.now(), value });
    return value;
}

function actionPhase(run) {
    if (run.status !== "completed") return "running";
    switch (run.conclusion) {
        case "success": return "completed";
        case "failure":
        case "timed_out":
        case "startup_failure": return "failed";
        case "cancelled":
        case "skipped":
        case "stale": return "aborted";
        default: return "escalated";
    }
}

function triggerKind(event) {
    if (event === "workflow_dispatch" || event === "repository_dispatch") return "manual";
    if (event === "schedule") return "schedule";
    if (event === "issues") return "item";
    return "webhook";
}

export function actionRunSummary(resolved, workflow, run, pullMetadata) {
    const phase = actionPhase(run);
    const pull = run.pull_requests?.[0];
    const operator = pull ? {
        pullRequest: {
            provider: "github",
            kind: "pr",
            id: String(pull.number),
            url: pullMetadata?.url || `https://github.com/${resolved.owner}/${resolved.repo}/pull/${pull.number}`,
        },
        pullRequestTitle: pullMetadata?.title || "",
    } : undefined;
    return {
        id: String(run.id),
        runId: String(run.id),
        workflow: workflow.name || resolved.workflow,
        gaggle: `${resolved.owner}/${resolved.repo}`,
        trigger: { kind: triggerKind(run.event), ref: run.event },
        phase,
        terminal: phase !== "running",
        startedAt: run.run_started_at || run.created_at,
        lastActivityAt: run.updated_at,
        finishedAt: phase === "running" ? undefined : run.updated_at,
        actionsURL: run.html_url,
        actionsAttempt: run.run_attempt,
        actionsConclusion: run.conclusion,
        operator,
    };
}

async function actionPullMetadata(resolved, workflowRuns) {
    const numbers = [...new Set(workflowRuns
        .flatMap((run) => run.pull_requests || [])
        .map((pull) => Number(pull.number))
        .filter((number) => Number.isInteger(number) && number > 0))];
    if (!numbers.length) return new Map();
    const selections = numbers
        .map((number, index) => `pr${index}: pullRequest(number: ${number}) { number title url }`)
        .join("\n");
    const query = `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){${selections}}}`;
    const response = await ghJSON([
        "api", "graphql",
        "-f", `query=${query}`,
        "-F", `owner=${resolved.owner}`,
        "-F", `name=${resolved.repo}`,
    ]);
    return new Map(Object.values(response.data?.repository || {})
        .filter(Boolean)
        .map((pull) => [Number(pull.number), pull]));
}

async function workflowRuns(resolved, filters = {}) {
    const limit = Math.min(Number(filters.limit) || 50, 100);
    const response = await ghJSON([
        "api",
        `repos/${resolved.owner}/${resolved.repo}/actions/workflows/${encodeURIComponent(resolved.workflow)}/runs?per_page=${limit}`,
    ]);
    const workflow = await workflowMetadata(resolved);
    const workflowRuns = response.workflow_runs || [];
    const pullMetadata = await actionPullMetadata(resolved, workflowRuns);
    let runs = workflowRuns.map((run) => {
        const pullNumber = Number(run.pull_requests?.[0]?.number);
        return actionRunSummary(resolved, workflow, run, pullMetadata.get(pullNumber));
    });
    if (filters.gaggle) runs = runs.filter((run) => run.gaggle === filters.gaggle);
    if (filters.workflow) runs = runs.filter((run) => run.workflow === filters.workflow);
    if (filters.phase) runs = runs.filter((run) => run.phase === filters.phase);
    if (filters.trigger) runs = runs.filter((run) => run.trigger.kind === filters.trigger);
    if (filters.since) runs = runs.filter((run) => new Date(run.startedAt) >= new Date(filters.since));
    if (filters.until) runs = runs.filter((run) => new Date(run.startedAt) <= new Date(filters.until));
    return { workflow, runs };
}

export async function probeActionsSource(source) {
    const parsed = parseWorkflowURL(source.value);
    const workflow = await workflowMetadata(parsed);
    return {
        ...source,
        connected: true,
        mode: "actions",
        health: { ready: true, actions: true },
        workflowName: workflow.name,
    };
}

export async function loadActionsSnapshot(resolved, filters = {}) {
    const { workflow, runs } = await workflowRuns(resolved, filters);
    const gaggle = `${resolved.owner}/${resolved.repo}`;
    return {
        mode: "actions",
        health: { ready: true, actions: true },
        instance: {
            name: `GitHub · ${gaggle}`,
            warnings: [],
        },
        gaggles: [{ name: gaggle }],
        workflows: [{
            identity: { gaggle, name: workflow.name || resolved.workflow },
            gaggle,
            displayName: workflow.name || resolved.workflow,
            purpose: `GitHub Actions workflow ${workflow.path || resolved.workflow}`,
            triggers: [{ type: "manual" }],
            concurrency: {
                activeRuns: runs.filter((run) => run.phase === "running").length,
                maxConcurrentRuns: 0,
            },
        }],
        runs,
        capabilities: {},
    };
}

export async function loadActionsRuns(resolved, filters = {}) {
    const { runs } = await workflowRuns(resolved, filters);
    return { runs };
}

async function findRunDirectories(root) {
    const found = [];
    async function visit(directory, depth) {
        if (depth > 8) return;
        let entries;
        try {
            entries = await fs.readdir(directory, { withFileTypes: true });
        } catch {
            return;
        }
        const names = new Set(entries.map((entry) => entry.name));
        if (names.has("events.jsonl") && names.has("run.yaml")) {
            found.push(directory);
            return;
        }
        await Promise.all(entries
            .filter((entry) => entry.isDirectory())
            .map((entry) => visit(path.join(directory, entry.name), depth + 1)));
    }
    await visit(root, 0);
    return found.sort();
}

async function findCachedWorkflowGraph(resolved, workflow) {
    const root = actionsCacheRoot(resolved);
    const candidates = [];
    async function visit(directory, depth) {
        if (depth > 8) return;
        let entries;
        try {
            entries = await fs.readdir(directory, { withFileTypes: true });
        } catch {
            return;
        }
        await Promise.all(entries.map(async (entry) => {
            const target = path.join(directory, entry.name);
            if (entry.isDirectory()) {
                await visit(target, depth + 1);
            } else if (entry.name === "workflow-graph") {
                try {
                    const [graph, stat] = await Promise.all([
                        fs.readFile(target, "utf8").then(JSON.parse),
                        fs.stat(target),
                    ]);
                    if (!workflow || graph.name === workflow) {
                        candidates.push({ graph, modifiedAt: stat.mtimeMs });
                    }
                } catch {
                    // A corrupt cache entry is ignored; another completed run
                    // may still provide the pinned graph.
                }
            }
        }));
    }
    await visit(root, 0);
    candidates.sort((a, b) => b.modifiedAt - a.modifiedAt);
    return candidates[0]?.graph || null;
}

function safeDirectoryName(value) {
    return value.replace(/[^A-Za-z0-9._-]+/g, "-").slice(0, 120);
}

function durationMillis(value) {
    let total = 0;
    const pattern = /(\d+(?:\.\d+)?)(ms|h|m|s)/g;
    for (const match of String(value || "").matchAll(pattern)) {
        const amount = Number(match[1]);
        const multiplier = match[2] === "h"
            ? 3600000
            : match[2] === "m"
                ? 60000
                : match[2] === "s"
                    ? 1000
                    : 1;
        total += amount * multiplier;
    }
    return total;
}

function logEventTime(line, startedAt, elapsed) {
    const timestamp = line.match(/\b(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z)\b/)?.[1];
    if (timestamp) return timestamp;
    if (startedAt && elapsed) {
        return new Date(new Date(startedAt).getTime() + durationMillis(elapsed)).toISOString();
    }
    return startedAt || new Date().toISOString();
}

export function parseGoobersLiveLog(text, startedAt) {
    const events = [];
    let identity = null;
    let seq = 1;
    let lastRunID = "";
    for (const line of String(text || "").split(/\r?\n/)) {
        const created = line.match(
            /created run ([0-9a-f]+) \(workflow=([^\s)]+) gaggle=([^)]+)\)/,
        );
        if (created) {
            identity = { runId: created[1], workflow: created[2], gaggle: created[3] };
            lastRunID = created[1];
            events.push({
                seq: seq++,
                time: logEventTime(line, startedAt, "0s"),
                type: "run.started",
                runId: created[1],
            });
            continue;
        }
        const stageStarted = line.match(
            /stage ([^\s]+) started \(run=([0-9a-f]+), attempt=(\d+), elapsed=([^)]+)\)/,
        );
        if (stageStarted) {
            lastRunID = stageStarted[2];
            events.push({
                seq: seq++,
                time: logEventTime(line, startedAt, stageStarted[4]),
                type: "stage.started",
                runId: stageStarted[2],
                stage: stageStarted[1],
                attempt: Number(stageStarted[3]),
            });
            continue;
        }
        const stageFinished = line.match(
            /stage ([^\s]+) finished \(run=([0-9a-f]+), attempt=(\d+), status=([^,\s)]+), elapsed=([^)]+)\)/,
        );
        if (stageFinished) {
            lastRunID = stageFinished[2];
            events.push({
                seq: seq++,
                time: logEventTime(line, startedAt, stageFinished[5]),
                type: "stage.finished",
                runId: stageFinished[2],
                stage: stageFinished[1],
                attempt: Number(stageFinished[3]),
                status: stageFinished[4],
            });
            continue;
        }
        const paused = line.match(
            /waiting: run ([0-9a-f]+) paused at gate ([^\s)]+) \(elapsed=([^)]+)\)/,
        );
        if (paused) {
            lastRunID = paused[1];
            const previous = events[events.length - 1];
            if (previous?.type !== "gate.started" || previous.gate !== paused[2]) {
                events.push({
                    seq: seq++,
                    time: logEventTime(line, startedAt, paused[3]),
                    type: "gate.started",
                    runId: paused[1],
                    gate: paused[2],
                });
            }
            continue;
        }
        const finished = line.match(
            /run ([0-9a-f]+) finished(?:.*?status=([^,\s)]+))?/,
        );
        if (finished) {
            lastRunID = finished[1];
            events.push({
                seq: seq++,
                time: logEventTime(line, startedAt),
                type: "run.finished",
                runId: finished[1],
                status: finished[2] || "completed",
            });
        }
    }
    return { identity, runId: identity?.runId || lastRunID, events };
}

function workflowFromJobName(name) {
    return String(name || "").match(/^Run (.+) via Goobers$/i)?.[1] || "";
}

async function actionsRunWithJobs(resolved, actionsRunID) {
    const [run, jobsResponse] = await Promise.all([
        ghJSON(["api", `repos/${resolved.owner}/${resolved.repo}/actions/runs/${actionsRunID}`]),
        ghJSON(["api", `repos/${resolved.owner}/${resolved.repo}/actions/runs/${actionsRunID}/jobs?per_page=100`]),
    ]);
    return { run, jobs: jobsResponse.jobs || [] };
}

export function parseHostedProgress(check, actionsRunID) {
    const text = check?.output?.text || "";
    const start = text.indexOf(HOSTED_PROGRESS_START);
    const end = text.indexOf(HOSTED_PROGRESS_END, start + HOSTED_PROGRESS_START.length);
    if (start < 0 || end < 0) return null;
    try {
        const embedded = text.slice(start + HOSTED_PROGRESS_START.length, end)
            .trim()
            .replace(/^```json\s*/i, "")
            .replace(/\s*```$/, "");
        const payload = JSON.parse(embedded);
        if (payload.schema !== HOSTED_PROGRESS_SCHEMA ||
            String(payload.actionsRunId) !== String(actionsRunID) ||
            !payload.identity?.runId ||
            !Array.isArray(payload.events)) {
            return null;
        }
        return payload;
    } catch {
        return null;
    }
}

async function hostedProgressForRun(resolved, run, actionsRunID) {
    const matches = [];
    for (let page = 1; page <= 10; page++) {
        const response = await ghJSON([
            "api",
            `repos/${resolved.owner}/${resolved.repo}/commits/${run.head_sha}/check-runs?per_page=100&page=${page}`,
        ]);
        const checks = response.check_runs || [];
        matches.push(...checks
            .map((check) => ({ check, payload: parseHostedProgress(check, actionsRunID) }))
            .filter(({ payload }) => payload));
        if (checks.length < 100) break;
    }
    return matches.sort((a, b) =>
        Number(b.payload.revision || 0) - Number(a.payload.revision || 0))[0] || null;
}

async function hostedProgressDetail(resolved, run, jobs, actionsRunID, hosted, journalError) {
    const payload = hosted.payload;
    const events = projectedEvents(payload.events);
    events.unshift({
        seq: 0,
        time: run.run_started_at || run.created_at,
        type: "actions.run",
        externalRef: {
            provider: "github",
            kind: "actions-run",
            id: String(actionsRunID),
            url: payload.actionsRunUrl || run.html_url,
        },
    });
    const latest = [...payload.events].reverse().find((event) =>
        event.type === "stage.started" || event.type === "gate.started");
    const limitations = [
        "Showing the live Goobers Check Run contract until the final journal artifact is available.",
    ];
    if (payload.truncatedBefore) {
        limitations.push(`Earlier projected events before sequence ${payload.truncatedBefore} were truncated.`);
    }
    if (journalError) {
        limitations.push(`Journal artifact pending: ${journalError.message || journalError}`);
    }
    const operator = await projectOperator(resolved, "", payload.events, payload.phase);
    operator.diagnosticsLimitations = [
        ...(operator.diagnosticsLimitations || []),
        ...limitations,
    ];
    const phase = payload.phase || actionPhase(run);
    return {
        id: String(actionsRunID),
        goobersRunId: payload.identity.runId,
        workflow: payload.identity.workflow,
        workflowVersion: payload.identity.workflowVersion,
        gaggle: payload.identity.gaggle,
        trigger: payload.identity.trigger,
        phase,
        terminal: phase !== "running",
        startedAt: payload.identity.startedAt,
        lastActivityAt: payload.updatedAt || run.updated_at,
        graph: payload.graph || null,
        graphStatus: payload.graph ? "hosted-progress" : "unavailable",
        transitions: projectTransitions(payload.events, phase),
        events,
        operator,
        actionsRunId: String(actionsRunID),
        actionsRunUrl: payload.actionsRunUrl || run.html_url,
        partial: true,
        currentStage: latest?.stage || latest?.gate,
        hostedProgressRevision: payload.revision,
    };
}

async function runningJobLog(resolved, jobID) {
    try {
        const { stdout } = await execFileAsync("gh", [
            "api",
            `repos/${resolved.owner}/${resolved.repo}/actions/jobs/${jobID}/logs`,
        ], {
            timeout: 30000,
            maxBuffer: 50 * 1024 * 1024,
            windowsHide: true,
        });
        return stdout;
    } catch {
        return "";
    }
}

function timelineEvents(jobs, actionsRunID) {
    let seq = 1;
    return jobs.flatMap((job) => (job.steps || []).map((step) => ({
        seq: seq++,
        time: step.started_at || job.started_at,
        type: step.status === "completed" ? "actions.step.finished" : "actions.step.started",
        runId: String(actionsRunID),
        stage: step.name,
        status: step.conclusion || step.status,
        job: job.name,
    })));
}

async function loadPartialActionsRun(resolved, actionsRunID, journalError) {
    const { run, jobs } = await actionsRunWithJobs(resolved, actionsRunID);
    try {
        const hosted = await hostedProgressForRun(resolved, run, actionsRunID);
        if (hosted) {
            return await hostedProgressDetail(
                resolved, run, jobs, actionsRunID, hosted, journalError,
            );
        }
    } catch (err) {
        journalError = new Error(
            `${journalError?.message || journalError || "Journal unavailable"}; ` +
            `hosted progress lookup failed: ${err.message || err}`,
        );
    }
    const phase = actionPhase(run);
    const gooberJob = jobs.find((job) => /Goobers/i.test(job.name)) ||
        jobs.find((job) => job.status === "in_progress") ||
        jobs[0];
    const workflow = workflowFromJobName(gooberJob?.name) || run.name || resolved.workflow;
    const log = gooberJob ? await runningJobLog(resolved, gooberJob.id) : "";
    const parsed = parseGoobersLiveLog(log, gooberJob?.started_at || run.run_started_at);
    const events = parsed.events.length ? parsed.events : timelineEvents(jobs, actionsRunID);
    events.unshift({
        seq: 0,
        time: run.run_started_at || run.created_at,
        type: "actions.run",
        externalRef: {
            provider: "github",
            kind: "actions-run",
            id: String(actionsRunID),
            url: run.html_url,
        },
    });
    const graph = await findCachedWorkflowGraph(resolved, parsed.identity?.workflow || workflow);
    const latestGoobersEvent = [...parsed.events].reverse().find((event) =>
        event.type === "stage.started" || event.type === "gate.started");
    const limitations = [];
    if (!parsed.events.length) {
        limitations.push(
            "GitHub has not exposed the running job log through its API yet; " +
            "showing the live Actions job timeline until log data or the journal artifact is available.",
        );
    }
    if (!graph) {
        limitations.push("No cached pinned workflow graph is available from an earlier run.");
    }
    if (journalError) {
        limitations.push(`Journal artifact pending: ${journalError.message || journalError}`);
    }
    return {
        id: String(actionsRunID),
        goobersRunId: parsed.runId || undefined,
        workflow: parsed.identity?.workflow || workflow,
        gaggle: parsed.identity?.gaggle || `${resolved.owner}/${resolved.repo}`,
        trigger: { kind: triggerKind(run.event), ref: run.event },
        phase,
        terminal: phase !== "running",
        startedAt: parsed.events[0]?.time || gooberJob?.started_at || run.run_started_at,
        lastActivityAt: latestGoobersEvent?.time || run.updated_at,
        graph,
        graphStatus: graph ? "cached-partial" : "unavailable",
        transitions: projectTransitions(parsed.events, phase),
        events,
        operator: {
            liveness: "recent",
            trajectory: "running",
            claim: { leaseStatus: "unknown", providerMarker: "not-recorded" },
            potentialBlockers: [],
            diagnosticsLimitations: limitations,
        },
        actionsRunId: String(actionsRunID),
        actionsRunUrl: run.html_url,
        partial: true,
        currentStage: latestGoobersEvent?.stage || latestGoobersEvent?.gate,
    };
}

async function ensureRunJournal(resolved, actionsRunID) {
    const runRoot = path.join(actionsCacheRoot(resolved), String(actionsRunID));
    const existing = await findRunDirectories(runRoot);
    if (existing.length) return existing[0];

    const response = await ghJSON([
        "api",
        `repos/${resolved.owner}/${resolved.repo}/actions/runs/${actionsRunID}/artifacts?per_page=100`,
    ]);
    const artifacts = (response.artifacts || [])
        .filter((artifact) => !artifact.expired)
        .sort((a, b) => {
            const aGoobers = /goobers|journal/i.test(a.name) ? 1 : 0;
            const bGoobers = /goobers|journal/i.test(b.name) ? 1 : 0;
            return bGoobers - aGoobers;
        });
    if (!artifacts.length) {
        throw new Error("This Actions run has no unexpired artifacts.");
    }

    const failures = [];
    for (const artifact of artifacts) {
        const destination = path.join(runRoot, safeDirectoryName(artifact.name));
        await fs.mkdir(destination, { recursive: true });
        try {
            await execFileAsync("gh", [
                "run", "download", String(actionsRunID),
                "--repo", `${resolved.owner}/${resolved.repo}`,
                "--name", artifact.name,
                "--dir", destination,
            ], {
                timeout: 120000,
                maxBuffer: 20 * 1024 * 1024,
                windowsHide: true,
            });
        } catch (err) {
            failures.push(`${artifact.name}: ${String(err.stderr || err.message || err).trim()}`);
            continue;
        }
        const discovered = await findRunDirectories(destination);
        if (discovered.length) return discovered[0];
    }
    throw new Error(
        "The run artifacts did not contain a Goobers journal (run.yaml + events.jsonl)." +
        (failures.length ? ` Download errors: ${failures.join("; ")}` : ""),
    );
}

async function readTrace(runDirectory) {
    const runID = path.basename(runDirectory);
    const root = path.dirname(path.dirname(runDirectory));
    const { stdout } = await execFileAsync(GOOBERS_BIN, [
        "trace", "--json", runID, root,
    ], {
        timeout: 30000,
        maxBuffer: 50 * 1024 * 1024,
        windowsHide: true,
    });
    return JSON.parse(stdout);
}

function projectedEvents(events) {
    return (events || []).map((event) => {
        const projected = { ...event };
        if (event.type === "artifact.recorded" && event.ref) {
            projected.artifact = {
                ...event.ref,
                name: event.name,
                stage: event.stage,
                attempt: event.attempt,
            };
        }
        return projected;
    });
}

function projectTransitions(events, phase) {
    const transitions = [];
    let current = "";
    let lastFinished = "";
    const verdicts = new Map();
    for (const event of events) {
        const next = event.type === "stage.started"
            ? event.stage
            : event.type === "gate.started"
                ? event.gate
                : "";
        if (next) {
            if (current && current !== next) {
                transitions.push({
                    seq: event.seq,
                    source: current,
                    target: next,
                    verdict: verdicts.get(current) || undefined,
                });
            }
            current = next;
        }
        if (event.type === "stage.finished" && event.stage) lastFinished = event.stage;
        if (event.type === "gate.evaluated" && event.gate) {
            current = event.gate;
            verdicts.set(event.gate, event.verdict);
        }
        if (event.type === "run.finished") {
            transitions.push({
                seq: event.seq,
                source: current || lastFinished,
                status: event.status || phase,
                terminal: true,
            });
        }
    }
    return transitions;
}

async function artifactJSON(runDirectory, ref) {
    if (!runDirectory || !ref?.path) return null;
    try {
        return JSON.parse(await fs.readFile(path.join(runDirectory, ref.path), "utf8"));
    } catch {
        return null;
    }
}

async function projectOperator(resolved, runDirectory, events, phase) {
    const operator = {
        liveness: phase === "running" ? "recent" : "terminal",
        trajectory: phase === "running" ? "running" : "parked",
        claim: { leaseStatus: "none", providerMarker: "not-recorded" },
        potentialBlockers: [],
    };
    const referenceTitles = new Map();
    for (const event of events) {
        if (event.type === "stage.finished" && event.outputs?.id && event.outputs?.title) {
            referenceTitles.set(String(event.outputs.id), String(event.outputs.title));
            if (!operator.issue) {
                operator.issue = { number: String(event.outputs.id), title: String(event.outputs.title) };
            }
        }
        if (event.type === "ref.touched" && event.externalRef?.kind === "issue") {
            operator.issue = operator.issue || { number: String(event.externalRef.id) };
            if (String(operator.issue.number) === String(event.externalRef.id) && event.externalRef.url) {
                operator.issue.url = event.externalRef.url;
            }
            if (event.runner?.operation === "claim") operator.claim.providerMarker = "recorded";
        }
        if (event.type === "ref.touched" && event.externalRef?.kind === "pr") {
            operator.pullRequest = event.externalRef;
            operator.pullRequestTitle = referenceTitles.get(String(event.externalRef.id)) || "";
        }
        if (event.error) operator.latestError = event.error;
        if (event.type === "gate.evaluated" && event.gate === "review") {
            const verdict = await artifactJSON(runDirectory, event.ref);
            operator.review = {
                verdict: event.verdict,
                rationale: verdict?.rationale || verdict?.summary || "",
            };
        }
    }
    if (operator.latestError) {
        operator.potentialBlockers.push(
            `${operator.latestError.code}: ${operator.latestError.message}`,
        );
    }
    if (operator.issue?.number) {
        try {
            const issue = await ghJSON([
                "issue", "view", String(operator.issue.number),
                "--repo", `${resolved.owner}/${resolved.repo}`,
                "--json", "title,url",
            ]);
            operator.issue.title = issue.title || "";
            operator.issue.url = issue.url || operator.issue.url;
        } catch (err) {
            operator.diagnosticsLimitations = [
                ...(operator.diagnosticsLimitations || []),
                `issue title unavailable: ${err.message || err}`,
            ];
        }
    }
    if (operator.pullRequest?.provider === "github" && operator.pullRequest.id) {
        try {
            const pull = await ghJSON([
                "pr", "view", String(operator.pullRequest.id),
                "--repo", `${resolved.owner}/${resolved.repo}`,
                "--json", "body,title,url",
            ]);
            operator.pullRequestBody = pull.body || "";
            operator.pullRequestTitle = pull.title || operator.pullRequestTitle || "";
            operator.pullRequest.url = pull.url || operator.pullRequest.url;
        } catch (err) {
            operator.diagnosticsLimitations = [`pull request description unavailable: ${err.message || err}`];
        }
    }
    return operator;
}

export async function loadActionsRunDetail(resolved, actionsRunID) {
    let runDirectory;
    try {
        runDirectory = await ensureRunJournal(resolved, actionsRunID);
    } catch (err) {
        return await loadPartialActionsRun(resolved, actionsRunID, err);
    }
    const [trace, graphText] = await Promise.all([
        readTrace(runDirectory),
        fs.readFile(path.join(runDirectory, "inputs", "workflow-graph"), "utf8"),
    ]);
    const events = projectedEvents(trace.events || []);
    const identity = trace.identity;
    const phase = trace.phase || trace.state?.phase || "running";
    events.unshift({
        seq: 0,
        time: identity.startedAt,
        type: "actions.run",
        externalRef: {
            provider: "github",
            kind: "actions-run",
            id: String(actionsRunID),
            url: `https://github.com/${resolved.owner}/${resolved.repo}/actions/runs/${actionsRunID}`,
        },
    });
    const finished = [...events].reverse().find((event) => event.type === "run.finished");
    return {
        id: String(actionsRunID),
        goobersRunId: identity.runId,
        workflow: identity.workflow,
        workflowVersion: identity.workflowVersion,
        gaggle: identity.gaggle,
        trigger: identity.trigger,
        phase,
        terminal: phase !== "running",
        startedAt: identity.startedAt,
        finishedAt: finished?.time,
        durationMillis: finished ? new Date(finished.time) - new Date(identity.startedAt) : undefined,
        repassCount: trace.repasses || 0,
        retryCount: 0,
        graph: JSON.parse(graphText),
        graphStatus: "pinned",
        transitions: projectTransitions(events, phase),
        events,
        operator: await projectOperator(resolved, runDirectory, events, phase),
        actionsRunId: String(actionsRunID),
        actionsRunUrl: `https://github.com/${resolved.owner}/${resolved.repo}/actions/runs/${actionsRunID}`,
    };
}

async function journalResource(resolved, actionsRunID, selector) {
    const runDirectory = await ensureRunJournal(resolved, actionsRunID);
    const lines = (await fs.readFile(path.join(runDirectory, "events.jsonl"), "utf8"))
        .split(/\r?\n/)
        .filter(Boolean)
        .map((line) => JSON.parse(line));
    const event = selector(lines);
    if (!event?.ref?.path) throw new Error("Journal resource was not found.");
    const bytes = await fs.readFile(path.join(runDirectory, event.ref.path));
    return { bytes, event };
}

export async function loadActionsArtifact(resolved, actionsRunID, digest) {
    const { bytes, event } = await journalResource(
        resolved,
        actionsRunID,
        (events) => events.find((item) =>
            item.type === "artifact.recorded" && item.ref?.digest === digest),
    );
    const name = event.name || "";
    const mediaType = /\.json$/i.test(name)
        ? "application/json; charset=utf-8"
        : /\.(txt|log|md|ya?ml)$/i.test(name)
            ? "text/plain; charset=utf-8"
            : "application/octet-stream";
    return { bytes, mediaType };
}

export async function loadActionsTranscript(resolved, actionsRunID, seq) {
    const { bytes } = await journalResource(
        resolved,
        actionsRunID,
        (events) => events.find((item) =>
            item.type === "span.recorded" && String(item.seq) === String(seq)),
    );
    return { bytes, mediaType: "application/x-ndjson; charset=utf-8" };
}
