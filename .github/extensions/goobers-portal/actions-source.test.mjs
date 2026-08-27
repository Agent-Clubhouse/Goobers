import assert from "node:assert/strict";
import test from "node:test";

import { actionRunSummary, parseHostedProgress } from "./actions-source.mjs";

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
