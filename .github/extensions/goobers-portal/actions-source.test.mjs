import assert from "node:assert/strict";
import test from "node:test";

import {
    actionRunSummary,
    parseGoobersLiveLog,
    parseHostedProgress,
    parseWorkflowURL,
    projectOperator,
} from "./actions-source.mjs";

test("parseWorkflowURL normalizes an Actions workflow URL", () => {
    assert.deepEqual(
        parseWorkflowURL("https://GitHub.com/octo/app/actions/workflows/release%20candidate.yml?query=ignored"),
        {
            owner: "octo",
            repo: "app",
            workflow: "release candidate.yml",
            workflowURL: "https://github.com/octo/app/actions/workflows/release%20candidate.yml",
        },
    );
});

test("parseWorkflowURL rejects non-workflow locations", () => {
    const invalid = [
        "not a URL",
        "https://example.com/octo/app/actions/workflows/ci.yml",
        "https://github.com/octo/app/actions/runs/123",
        "https://github.com/octo/app/actions/workflows",
    ];
    for (const value of invalid) {
        assert.throws(() => parseWorkflowURL(value), /GitHub Actions workflow URL|Expected https:\/\//);
    }
});

test("actionRunSummary includes an associated PR link and title", () => {
    const summary = actionRunSummary(
        { owner: "octo", repo: "app", workflow: "ci.yml" },
        { name: "CI" },
        {
            id: 123,
            status: "completed",
            conclusion: "success",
            event: "pull_request",
            display_title: "Custom workflow run name",
            pull_requests: [{ number: 42 }],
            run_started_at: "2026-08-27T01:00:00Z",
            updated_at: "2026-08-27T01:01:00Z",
            html_url: "https://github.com/octo/app/actions/runs/123",
        },
        {
            title: "Fix the frobnicator",
            url: "https://github.com/octo/app/pull/42",
        },
    );

    assert.equal(summary.operator.pullRequestTitle, "Fix the frobnicator");
    assert.equal(summary.operator.pullRequest.url, "https://github.com/octo/app/pull/42");
});

test("actionRunSummary projects every Actions conclusion and trigger kind", () => {
    const cases = [
        { status: "in_progress", conclusion: null, event: "push", phase: "running", trigger: "webhook" },
        { status: "completed", conclusion: "success", event: "workflow_dispatch", phase: "completed", trigger: "manual" },
        { status: "completed", conclusion: "failure", event: "repository_dispatch", phase: "failed", trigger: "manual" },
        { status: "completed", conclusion: "timed_out", event: "schedule", phase: "failed", trigger: "schedule" },
        { status: "completed", conclusion: "startup_failure", event: "issues", phase: "failed", trigger: "item" },
        { status: "completed", conclusion: "cancelled", event: "push", phase: "aborted", trigger: "webhook" },
        { status: "completed", conclusion: "skipped", event: "push", phase: "aborted", trigger: "webhook" },
        { status: "completed", conclusion: "stale", event: "push", phase: "aborted", trigger: "webhook" },
        { status: "completed", conclusion: "neutral", event: "push", phase: "escalated", trigger: "webhook" },
    ];
    for (const [index, current] of cases.entries()) {
        const summary = actionRunSummary(
            { owner: "octo", repo: "app", workflow: "ci.yml" },
            {},
            {
                id: index + 1,
                status: current.status,
                conclusion: current.conclusion,
                event: current.event,
                created_at: "2026-09-12T01:00:00Z",
                updated_at: "2026-09-12T01:01:00Z",
                pull_requests: [],
            },
        );
        assert.equal(summary.phase, current.phase, current.conclusion);
        assert.equal(summary.trigger.kind, current.trigger, current.event);
        assert.equal(summary.terminal, current.phase !== "running", current.conclusion);
        assert.equal(summary.workflow, "ci.yml");
        assert.equal(summary.startedAt, "2026-09-12T01:00:00Z");
        assert.equal(summary.operator, undefined);
    }
});

test("parseHostedProgress accepts the versioned fenced payload", () => {
    const payload = {
        schema: "goobers.dev/hosted-progress/v1",
        revision: 7,
        actionsRunId: "123",
        identity: { runId: "abc" },
        events: [],
    };
    const check = {
        output: {
            text: [
                "<!-- goobers-progress:v1 -->",
                "```json",
                JSON.stringify(payload),
                "```",
                "<!-- /goobers-progress:v1 -->",
            ].join("\n"),
        },
    };
    assert.deepEqual(parseHostedProgress(check, "123"), payload);
});

test("parseHostedProgress rejects another Actions run", () => {
    const check = {
        output: {
            text: "<!-- goobers-progress:v1 -->\n" +
                '{"schema":"goobers.dev/hosted-progress/v1","actionsRunId":"456",' +
                '"identity":{"runId":"abc"},"events":[]}\n' +
                "<!-- /goobers-progress:v1 -->",
        },
    };
    assert.equal(parseHostedProgress(check, "123"), null);
});

test("parseHostedProgress rejects malformed and incomplete contracts", () => {
    const wrap = (value) => ({
        output: {
            text: `<!-- goobers-progress:v1 -->\n${value}\n<!-- /goobers-progress:v1 -->`,
        },
    });
    const invalid = [
        undefined,
        { output: { text: "<!-- goobers-progress:v1 -->" } },
        wrap("not json"),
        wrap(JSON.stringify({ schema: "unknown", actionsRunId: "123", identity: { runId: "abc" }, events: [] })),
        wrap(JSON.stringify({ schema: "goobers.dev/hosted-progress/v1", actionsRunId: "123", events: [] })),
        wrap(JSON.stringify({ schema: "goobers.dev/hosted-progress/v1", actionsRunId: "123", identity: { runId: "abc" }, events: {} })),
    ];
    for (const check of invalid) assert.equal(parseHostedProgress(check, "123"), null);
});

test("parseGoobersLiveLog projects lifecycle events and elapsed timestamps", () => {
    const startedAt = "2026-09-12T01:00:00Z";
    const parsed = parseGoobersLiveLog([
        "created run abc123 (workflow=review gaggle=octo/app)",
        "stage plan started (run=abc123, attempt=2, elapsed=1.5s)",
        "stage plan finished (run=abc123, attempt=2, status=completed, elapsed=2m3s)",
        "waiting: run abc123 paused at gate approval (elapsed=2m4s)",
        "waiting: run abc123 paused at gate approval (elapsed=2m5s)",
        "2026-09-12T01:03:00Z run abc123 finished status=completed",
    ].join("\n"), startedAt);

    assert.deepEqual(parsed.identity, {
        runId: "abc123",
        workflow: "review",
        gaggle: "octo/app",
    });
    assert.equal(parsed.runId, "abc123");
    assert.deepEqual(parsed.events.map((event) => event.type), [
        "run.started",
        "stage.started",
        "stage.finished",
        "gate.started",
        "run.finished",
    ]);
    assert.equal(parsed.events[1].time, "2026-09-12T01:00:01.500Z");
    assert.equal(parsed.events[2].time, "2026-09-12T01:02:03.000Z");
    assert.equal(parsed.events[4].time, "2026-09-12T01:03:00Z");
});

test("parseGoobersLiveLog retains an observed run without a creation line", () => {
    const parsed = parseGoobersLiveLog(
        "run abc123 finished",
        "2026-09-12T01:00:00Z",
    );
    assert.equal(parsed.identity, null);
    assert.equal(parsed.runId, "abc123");
    assert.equal(parsed.events[0].status, "completed");
});

test("Actions-backed terminal runs project a terminal trajectory", async () => {
    const resolved = { owner: "octo", repo: "app" };
    for (const phase of ["completed", "failed", "aborted", "escalated"]) {
        const operator = await projectOperator(resolved, "", [], phase);
        assert.equal(operator.liveness, "terminal", phase);
        assert.equal(operator.trajectory, "terminal", phase);
        assert.notEqual(operator.trajectory, "parked", phase);
    }
});
