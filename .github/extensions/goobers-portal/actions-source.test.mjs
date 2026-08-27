import assert from "node:assert/strict";
import test from "node:test";

import { parseHostedProgress } from "./actions-source.mjs";

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
