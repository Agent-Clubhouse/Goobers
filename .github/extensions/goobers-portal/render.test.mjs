import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { lintInnerHTMLAssignments } from "./innerhtml-lint.mjs";
import {
    configurationWarningKey,
    groupConfigurationWarnings,
    renderConfigurationWarnings,
} from "./configuration-warnings.mjs";
import {
    formatRunDetailTime,
    renderTelemetryInsights,
    renderCausalDiagnosis,
    renderExecutionWaterfall,
    renderHtml,
    renderFleetPortalLink,
    renderGooberChip,
    renderRunDetailSummary,
    renderRunEventItems,
    renderOperatorPanel,
    renderRunRowCells,
    renderSnapshotCard,
    renderStageDefinitionInspector,
    renderStageInspectorStatus,
    renderTransitions,
} from "./render.mjs";

test("causal diagnosis renders attempts and escaped failure breadcrumbs", () => {
    const html = renderCausalDiagnosis({
        events: [
            { type: "stage.started", stage: "build", attempt: 1 },
            { type: "stage.finished", stage: "build", attempt: 1, status: "failed", error: "<unsafe> failed" },
        ],
    });
    assert.match(html, /Attempt lineage/);
    assert.match(html, /build/);
    assert.match(html, /&lt;unsafe&gt;/);
});

test("execution waterfall renders retry timing, gaps, and unavailable timing", () => {
    const html = renderExecutionWaterfall({
        events: [
            { type: "stage.started", stage: "first", attempt: 1, time: "2026-08-27T00:00:00Z" },
            { type: "stage.finished", stage: "first", attempt: 1, status: "succeeded", time: "2026-08-27T00:00:01Z" },
            { type: "stage.started", stage: "retry", attempt: 2, time: "2026-08-27T00:00:03Z" },
            { type: "stage.finished", stage: "retry", attempt: 2, status: "failed", time: "2026-08-27T00:00:04Z" },
            { type: "stage.started", stage: "unknown", attempt: 1 },
        ],
    });
    assert.match(html, /retry/);
    assert.match(html, /Idle gaps: 2s/);
    assert.match(html, /timing unavailable/);
});

test("stage definition inspector renders non-gate fields and escaped YAML tabs", () => {
    const html = renderStageDefinitionInspector({
        name: "implement <unsafe>",
        kind: "agentic",
        goal: "Ship safely",
        owner: { gaggle: "core", name: "implementer" },
        capabilities: ["repo:push"],
        timeoutSeconds: 3600,
        retry: { maxAttempts: 2, backoffSeconds: 30 },
        policyActions: ["pr:open"],
        requiredCapabilities: ["linux"],
        onTimeout: "escalate",
        rawYaml: "goal: <unsafe>\n",
    });
    assert.match(html, /class="internal-tabs" role="tablist" aria-label="Stage config view"/);
    assert.match(html, /role="tab" data-tab="fields"[^>]*aria-selected="true"/);
    assert.match(html, /core\/implementer/);
    assert.match(html, /Policy actions/);
    assert.match(html, /Required runner capabilities/);
    assert.match(html, /On timeout/);
    assert.doesNotMatch(html, /<dt>Branches<\/dt>|Max repasses/);
    assert.match(html, /goal: &lt;unsafe&gt;/);
    assert.doesNotMatch(html, /implement <unsafe>|goal: <unsafe>/);
});

test("stage definition inspector renders gate fields and YAML view", () => {
    const html = renderStageDefinitionInspector({
        name: "review",
        kind: "gate",
        goal: "",
        evaluator: "agentic",
        capabilities: [],
        branches: { pass: "", "needs-changes": "implement" },
        maxRepasses: 3,
        rawYaml: "name: review\n",
    }, "yaml");
    assert.match(html, /agentic evaluator/);
    assert.match(html, /pass \u2192 \(terminal\), needs-changes \u2192 implement/);
    assert.match(html, /Max repasses<\/dt><dd>3/);
    assert.match(html, /id="stage-tab-yaml"[^>]*aria-selected="true"/);
    assert.match(html, /id="stage-panel-fields"[^>]* hidden/);
    assert.doesNotMatch(html, /Policy actions|Required runner capabilities|On timeout/);
});

test("stage inspector fallbacks and status messages are escaped", () => {
    const deterministic = renderStageDefinitionInspector({
        name: "query",
        kind: "deterministic",
        capabilities: [],
    });
    assert.match(deterministic, /Deterministic runtime/);
    assert.match(deterministic, /No retry declared/);
    assert.match(deterministic, /fail \(default\)/);
    assert.match(deterministic, /No YAML available/);
    assert.match(renderStageInspectorStatus("<unavailable>", { error: true }), /role="alert"/);
    assert.match(renderStageInspectorStatus("<unavailable>", { error: true }), /&lt;unavailable&gt;/);
});

// #4567: the snapshot cards and run table interpolate values that originate
// outside the portal — an instance name read from instance.yaml, run IDs,
// workflow and gaggle names, and phases carried in run metadata. Before this
// they reached innerHTML raw. These cover the escaping directly, and the
// inlining test below covers the browser actually getting the escaped copy.

const HOSTILE = '<img src=x onerror=alert(1)>';
const MODEL_WARNING = {
    code: "MODEL002",
    severity: "warning",
    scope: "Goober/coder",
    explanation: "requested model is unavailable; using the harness default",
};

test("configuration warnings group by scope and remediation in deterministic order", () => {
    const warnings = [
        { ...MODEL_WARNING, scope: "Workflow/zeta", code: "VER003", explanation: "later" },
        { ...MODEL_WARNING, explanation: "second" },
        MODEL_WARNING,
    ];
    const groups = groupConfigurationWarnings(warnings);
    assert.equal(groups.length, 2);
    assert.equal(groups[0].scope, "Goober/coder");
    assert.deepEqual(groups[0].warnings.map((warning) => warning.explanation), [
        MODEL_WARNING.explanation,
        "second",
    ]);
    assert.equal(groups[0].remediation.key, "config-validate");
    assert.equal(groups[1].scope, "Workflow/zeta");
});

test("configuration warning dismissal survives unchanged refreshes and changed content reappears", () => {
    const dismissed = new Set([configurationWarningKey(MODEL_WARNING)]);
    const unchanged = renderConfigurationWarnings(
        [{ ...MODEL_WARNING }],
        "instance",
        { dismissedWarningKeys: dismissed },
    );
    assert.match(unchanged, /Warnings dismissed for this portal session/);
    assert.doesNotMatch(unchanged, /requested model is unavailable/);

    const changed = renderConfigurationWarnings(
        [{ ...MODEL_WARNING, explanation: "the configured model changed" }],
        "instance",
        { dismissedWarningKeys: dismissed },
    );
    assert.match(changed, /the configured model changed/);
    assert.match(changed, /1 active warning/);
});

test("configuration warning empty states differ by context", () => {
    assert.match(
        renderConfigurationWarnings([], "instance"),
        />No active configuration warnings\.<\/strong>/,
    );
    assert.match(
        renderConfigurationWarnings([], "workflow"),
        />No active configuration warnings for this workflow\.<\/strong>/,
    );
});

test("configuration warnings render collapsible groups and escaped dismiss controls", () => {
    const html = renderConfigurationWarnings([
        MODEL_WARNING,
        { ...MODEL_WARNING, explanation: HOSTILE },
    ], "workflow");
    assert.match(html, /<details class="configuration-warning-group" open>/);
    assert.match(html, /Dismiss all 2 warnings for Goober\/coder/);
    assert.match(html, /data-dismiss-warning=/);
    assert.match(html, /goobers validate/);
    assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
    assert.doesNotMatch(html, /<img/);
});

test("compact portal puts attention before activity and keeps source status outside tabs", () => {
    const html = renderHtml("compact");
    assert.match(html, /data-tab="attention"[^>]*>Overview<\/button>/);
    assert.ok(html.indexOf('id="needs-you"') < html.indexOf('id="cards"'));
    assert.ok(html.indexOf('id="freshness"') < html.indexOf('id="dashboard"'));
    assert.match(html, /aria-label="Goobers source"/);
    assert.match(html, /Skip to content/);
    assert.match(html, /More filters and saved views/);
    assert.match(html, /class="table-scroll" role="region" aria-label="Runs" tabindex="0"/);
    assert.match(html, /class="table-scroll" role="region" aria-label="Workflows" tabindex="0"/);
});

test("attention controls can grow instead of clipping at narrow widths", () => {
    const html = renderHtml("compact");
    const rule = html.match(/\.attention-item \{([^}]+)\}/)[1];
    assert.match(rule, /min-height: 84px/);
    assert.doesNotMatch(rule, /(?:^|[;\s])height:|overflow: hidden/);
});

test("Fleet portal link is rendered from selected instance status", () => {
    assert.match(
        renderFleetPortalLink({ associated: true, canonicalUri: "https://fleet.example.com/", connectionState: "connected" }),
        /id="fleet-portal-link"[^>]*href="https:\/\/fleet\.example\.com\/"[^>]*target="_blank"[^>]*rel="noopener noreferrer"[^>]*>Open Fleet portal \(connected\)/,
    );
    assert.equal(renderFleetPortalLink({ associated: false, canonicalUri: "https://fleet.example.com/" }), "");
    assert.equal(renderFleetPortalLink({ associated: true, canonicalUri: "javascript:alert(1)" }), "");
    assert.doesNotMatch(renderHtml("fleet"), /Save link|fleetPortalUrl/);
});

test("goober chips add compact stage identity without losing escaping", () => {
    const html = renderGooberChip('implement <unsafe>', { kind: "stage" });
    assert.match(html, /class="goober-chip"/);
    assert.match(html, /data-kind="stage"/);
    assert.match(html, /🛠/);
    assert.match(html, /implement &lt;unsafe&gt;/);
    assert.doesNotMatch(html, /<unsafe>/);
});

test("run status badges preserve visible text and escape phase attributes", () => {
    const html = renderRunRowCells({ phase: '" onmouseover="alert(1)', runId: "run" });
    assert.match(html, /data-phase="&quot; onmouseover=&quot;alert\(1\)"/);
    assert.doesNotMatch(html, /data-phase="" onmouseover/);
    assert.match(renderRunRowCells({ phase: "failed" }), /data-phase="failed">failed<\/span>/);
});

test("snapshot cards escape an untrusted instance name", () => {
    const html = renderSnapshotCard("Instance", HOSTILE);
    assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
    assert.doesNotMatch(html, /<img/);
    assert.match(html, /<div class="label">Instance<\/div>/);
});

test("snapshot cards preserve ordinary numeric values", () => {
    assert.match(renderSnapshotCard("Recent runs", 12), /<div class="value">12<\/div>/);
});

test("run rows escape every field the run itself controls", () => {
    const html = renderRunRowCells(
        {
            runId: '"><script>alert(1)</script>',
            workflow: HOSTILE,
            gaggle: "a & b",
            trigger: { kind: HOSTILE },
            phase: "<b>running</b>",
        },
        { startedAt: HOSTILE, lastActivityAt: "'" },
    );
    for (const raw of ["<script>", "<img", "<b>running</b>"]) {
        assert.ok(!html.includes(raw), `unescaped ${raw} in ${html}`);
    }
    assert.match(html, /&quot;&gt;&lt;script&gt;/);
    assert.match(html, /a &amp; b/);
    assert.match(html, /&lt;b&gt;running&lt;\/b&gt;/);
    assert.match(html, /&#39;/);
});

test("run rows keep pre-escaped association and actions markup as markup", () => {
    const html = renderRunRowCells(
        { runId: "r1" },
        {
            actionsLink: '<a class="actions-run-link" href="https://example.test">Action</a>',
            associations: '<div class="run-associations">linked</div>',
        },
    );
    assert.match(html, /<a class="actions-run-link" href="https:\/\/example.test">Action<\/a>/);
    assert.match(html, /<div class="run-associations">linked<\/div>/);
});

test("run rows fall back without throwing on an empty run", () => {
    const html = renderRunRowCells(undefined, undefined);
    assert.match(html, /data-open-run=""/);
    assert.match(html, /aria-label="Copy run id" title="Copy run id">&#128203;<\/button>/);
    assert.ok(!html.includes("undefined"), html);
});

test("run detail summary escapes every metadata field", () => {
    const html = renderRunDetailSummary({
        id: HOSTILE,
        workflow: HOSTILE,
        workflowVersion: '"><script>version</script>',
        gaggle: HOSTILE,
        phase: HOSTILE,
        terminal: true,
        repassCount: HOSTILE,
        retryCount: HOSTILE,
        trigger: { kind: HOSTILE },
        startedAt: HOSTILE,
        finishedAt: HOSTILE,
        events: [{ type: "stage.finished", stage: HOSTILE }],
        transitions: [{ terminal: true, status: HOSTILE }],
    });

    test("run detail summary renders current and active goober chips", () => {
        const html = renderRunDetailSummary({
            id: "run",
            workflow: "implementation",
            currentStage: "implement",
            activeStages: [{ name: "implement", goober: "implementer" }],
        });
        assert.match(html, /Current stage/);
        assert.match(html, /Active goobers/);
        assert.match(html, /class="goober-chip"/);
        assert.match(html, /implementer/);
    });
    assert.doesNotMatch(html, /<img|<script>/);
    assert.match(html, /&lt;img/);
    assert.match(html, /&lt;script&gt;version/);
});

test("run event items escape hostile metadata and preserve safe links", () => {
    const html = renderRunEventItems([{
        seq: HOSTILE,
        type: HOSTILE,
        stage: HOSTILE,
        status: HOSTILE,
        time: HOSTILE,
        artifact: { digest: '"><script>digest</script>', name: HOSTILE, size: HOSTILE },
        externalRef: { url: "https://example.test/run", provider: HOSTILE, kind: HOSTILE, id: HOSTILE },
        outputs: { value: HOSTILE },
    }], HOSTILE, HOSTILE);
    assert.doesNotMatch(html, /<img|<script>/);
    assert.match(html, /&lt;img/);
    assert.match(html, /href="https:\/\/example\.test\/run"/);
    assert.match(html, /%3Cimg%20src%3Dx/);
});

test("run transitions escape hostile sequence, verdict, and state metadata", () => {
    const html = renderTransitions([{
        seq: HOSTILE,
        source: HOSTILE,
        target: HOSTILE,
        verdict: HOSTILE,
        status: HOSTILE,
        terminal: false,
    }]);
    assert.doesNotMatch(html, /<img/);
    assert.match(html, /&lt;img/);
    assert.doesNotMatch(html, /class="[^"]*<img/);
});

test("operator panel escapes hostile issue, liveness, trajectory, and review metadata", () => {
    const html = renderOperatorPanel({
        issue: { number: HOSTILE, title: HOSTILE },
        pullRequest: { provider: HOSTILE, kind: HOSTILE, id: HOSTILE },
        liveness: HOSTILE,
        trajectory: HOSTILE,
        review: { verdict: HOSTILE, rationale: HOSTILE },
        latestError: { code: HOSTILE, message: HOSTILE },
        potentialBlockers: [HOSTILE],
    });
    assert.doesNotMatch(html, /<img/);
    assert.match(html, /&lt;img/);
    assert.doesNotMatch(html, /class="[^"]*<img/);
});

test("run-detail time fallback escapes values when date conversion throws", () => {
    const hostile = { toString() { return HOSTILE; }, valueOf() { throw new Error("no conversion"); } };
    const formatted = formatRunDetailTime(hostile);
    assert.doesNotMatch(formatted, /<img/);
    assert.match(formatted, /&lt;img/);
});

test("innerHTML lint rejects direct unescaped property interpolation", async () => {
    const source = await readFile(new URL("./render.mjs", import.meta.url), "utf8");
    assert.deepEqual(lintInnerHTMLAssignments(source), []);
    assert.deepEqual(
        lintInnerHTMLAssignments('element.innerHTML = "<p>" + run.workflow + "</p>";'),
        ["line 1: innerHTML concatenates unescaped run.workflow"],
    );
    assert.deepEqual(
        lintInnerHTMLAssignments('element.innerHTML = "<p>" + escapeHtml(run.workflow) + "</p>";'),
        [],
    );
    assert.deepEqual(
        lintInnerHTMLAssignments('element.innerHTML = "<p>" + workflow + "</p>";'),
        ["line 1: innerHTML concatenates unescaped workflow"],
    );
    for (const mutation of [
        'element.innerHTML = "<p>" + String(run.workflow) + "</p>";',
        'element.innerHTML = "<p>" + (run.workflow ? run.workflow : "") + "</p>";',
        'element.innerHTML = `<p>${run.workflow}</p>`;',
        'let runDetailHtml = "<p>" + run.workflow; element.innerHTML = runDetailHtml;',
        'let runDetailHtml = "<p>"; runDetailHtml += run.workflow; element.innerHTML = runDetailHtml;',
        'let rowMarkup = "<p>"; rowMarkup += run.workflow; element.innerHTML = rowMarkup;',
    ]) {
        assert.equal(lintInnerHTMLAssignments(mutation).length, 1, mutation);
    }
    assert.deepEqual(
        lintInnerHTMLAssignments('let runDetailHtml = "<p>" + escapeHtml(run.workflow); element.innerHTML = runDetailHtml;'),
        [],
    );
});

test("the browser script receives the escaping helpers, not raw interpolation", () => {
    const page = renderHtml("inst-1");
    // The helpers are inlined verbatim for the client to call...
    assert.match(page, /const renderSnapshotCard = function renderSnapshotCard/);
    assert.match(page, /const renderConfigurationWarnings = function renderConfigurationWarnings/);
    assert.match(page, /const renderRunRowCells = function renderRunRowCells/);
    assert.match(page, /const renderRunDetailSummary = function renderRunDetailSummary/);
    assert.match(page, /const renderRunEventItems = function renderRunEventItems/);
    assert.match(page, /const renderTransitions = function renderTransitions/);
    assert.match(page, /const renderOperatorPanel = function renderOperatorPanel/);
    // ...remapped onto the client's own escapeHtml, so no stale identifier
    // survives to throw at runtime.
    assert.ok(!page.includes("escapeAssociationHtml"), "escapeAssociationHtml leaked into the page");
    assert.ok(!page.includes("escapeWarningHtml"), "escapeWarningHtml leaked into the page");
    // ...and the call sites go through them rather than concatenating.
    assert.match(page, /div\.innerHTML = renderSnapshotCard\(label, value\);/);
    assert.match(page, /tr\.innerHTML = renderRunRowCells\(r, \{/);
    assert.match(page, /let html = renderRunDetailSummary\(r, \{ actionsLink \}\);/);
    // The pre-fix raw forms are gone.
    assert.ok(!page.includes('"<td>" + (r.workflow || "")'), "raw workflow interpolation still present");
    assert.ok(!page.includes('"<td><code>" + runId'), "raw runId interpolation still present");
    assert.doesNotMatch(page, /\+\s*r\.[A-Za-z_$]/, "raw run-detail interpolation still present");
});

// #4892's escaping rewrite added a linked/unlinked fork at every association
// and left the unlinked side untested, which dropped branch coverage below the
// ratchet. These cover the fallback halves: the branch taken when there is no
// safe URL to link to is exactly the one that must still escape its label.

test("operator panel falls back to plain labels when no safe ref exists", () => {
    const html = renderOperatorPanel(
        {
            issue: { number: 42, title: "<script>alert(1)</script>" },
            pullRequest: {
                provider: "github",
                kind: "pull-request",
                id: "7",
                url: "javascript:alert(1)",
            },
            pullRequestTitle: "PR <b>title</b>",
        },
        [],
    );
    assert.match(html, /#42/);
    assert.match(html, /github pull-request #7: PR &lt;b&gt;title&lt;\/b&gt;/);
    assert.doesNotMatch(html, /<a href=/);
    // The unlinked path must still escape — it is the branch an attacker
    // reaches by simply not supplying a resolvable URL.
    assert.match(html, /&lt;script&gt;/);
    assert.doesNotMatch(html, /<script>alert/);
});

test("operator panel links an issue and pull request when refs resolve", () => {
    const html = renderOperatorPanel(
        {
            issue: { number: 42, title: "Issue title" },
            pullRequest: {
                provider: "github",
                kind: "pull-request",
                id: "7",
                url: "https://example.com/pull/7",
            },
            pullRequestTitle: "PR title",
        },
        [
            { id: "42", kind: "issue", url: "https://example.com/issues/42" },
        ],
    );
    assert.match(html, /href="https:\/\/example\.com\/issues\/42"/);
    assert.match(html, /href="https:\/\/example\.com\/pull\/7"/);
    assert.match(html, /github pull-request #7: PR title/);
    assert.equal((html.match(/rel="noopener noreferrer"/g) || []).length, 2);
});

test("operator panel renders an escaped pull request description", () => {
    const html = renderOperatorPanel({
        pullRequest: {
            provider: "github",
            kind: "pull-request",
            id: "7",
            url: "https://example.com/pull/7",
        },
        pullRequestTitle: "PR",
        pullRequestBody: "body with <img src=x onerror=1>",
    });
    assert.match(html, /Pull request description/);
    assert.match(html, /&lt;img/);
    assert.doesNotMatch(html, /<img src=x/);
});

test("execution waterfall reports absence rather than rendering an empty chart", () => {
    assert.match(renderExecutionWaterfall({}), /No execution waterfall is available yet/);
    assert.match(renderExecutionWaterfall({ attempts: [] }), /No execution waterfall is available yet/);
});

test("execution waterfall adds retry take labels and blocked gate cues", () => {
    const html = renderExecutionWaterfall({
        events: [
            { type: "stage.started", stage: "implement", attempt: 2, time: "2026-08-27T00:00:00Z" },
            { type: "stage.finished", stage: "implement", attempt: 2, status: "failed", time: "2026-08-27T00:00:01Z" },
            { type: "gate.started", stage: "review-gate", attempt: 1, time: "2026-08-27T00:00:02Z" },
            { type: "gate.finished", stage: "review-gate", attempt: 1, status: "blocked", time: "2026-08-27T00:00:03Z" },
        ],
    });
    assert.match(html, /take 2/);
    assert.match(html, /🔒 gate/);
    assert.match(html, /class="goober-chip"/);
});

test("run event items add a transcript link only for transcript events", () => {
    const withTranscript = renderRunEventItems(
        [{ name: "Agent transcript", seq: 3 }],
        "src",
        "run-1",
    );
    assert.match(withTranscript, /run-transcript\?source=src/);
    assert.match(withTranscript, /seq=3/);

    const withoutTranscript = renderRunEventItems([{ name: "build", seq: 4 }], "src", "run-1");
    assert.doesNotMatch(withoutTranscript, /run-transcript/);
});

// formatDuration's sub-second branch: a run that finishes in under a second
// renders milliseconds rather than a rounded "0s", which is the difference
// between a useful number and one that reads as "instant".
test("telemetry insights render sub-second durations in milliseconds", () => {
    const html = renderTelemetryInsights({
        startedAt: "2026-08-28T10:00:00.000Z",
        finishedAt: "2026-08-28T10:00:00.250Z",
    });
    assert.match(html, /250ms/);
    assert.doesNotMatch(html, /Run duration<\/div><div class="value">0s/);
});

test("telemetry insights render multi-second durations in seconds", () => {
    const html = renderTelemetryInsights({
        startedAt: "2026-08-28T10:00:00Z",
        finishedAt: "2026-08-28T10:00:12Z",
    });
    assert.match(html, /12s/);
});
