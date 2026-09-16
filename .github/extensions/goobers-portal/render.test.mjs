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
    filterWorkItems,
    formatWorkItemCost,
    formatWorkItemTimestamp,
    humanizeWorkItemOperation,
    INSIGHT_WINDOWS,
    costLookupRequestParams,
    costSummaryRequestParams,
    deriveExternalCostRows,
    insightPreviousWindowRange,
    insightRequestParams,
    insightScopeApiParams,
    insightScopeLabel,
    insightScopeOptionsFromStats,
    insightScopeValue,
    insightUsageForScope,
    insightWindowRange,
    isInInsightScope,
    parseInsightScope,
    renderTelemetryInsights,
    renderCausalDiagnosis,
    renderExecutionWaterfall,
    renderHtml,
    renderFleetPortalLink,
    renderGooberChip,
    renderInsightPanel,
    renderCostPanel,
    renderRunDetailSummary,
    renderRunEventItems,
    DIAGNOSTICS_INLINE_FILE_LIMIT,
    renderOperatorPanel,
    renderRunRowCells,
    renderSnapshotCard,
    renderStageDefinitionInspector,
    renderStageInspectorStatus,
    renderTransitions,
    renderWorkItemDetail,
    renderWorkItemList,
    workItemLabel,
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
        retry: {},
    });
    assert.match(deterministic, /Deterministic runtime/);
    assert.match(deterministic, /No retry declared/);
    assert.match(deterministic, /fail \(default\)/);
    assert.match(deterministic, /No YAML available/);
    assert.match(renderStageInspectorStatus("<unavailable>", { error: true }), /stage-inspector-error/);
    assert.doesNotMatch(renderStageInspectorStatus("<unavailable>", { error: true }), /role=/);
    assert.match(renderStageInspectorStatus("<unavailable>", { error: true }), /&lt;unavailable&gt;/);
    assert.match(renderStageDefinitionInspector({
        name: "review",
        kind: "gate",
        capabilities: [],
        maxRepasses: 0,
    }), /Max repasses<\/dt><dd>0/);
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

test("run event items auto-load small artifacts and require a button for large/unsized ones", () => {
    const small = renderRunEventItems([{
        seq: 1,
        artifact: { digest: "abc", name: "small.txt", size: DIAGNOSTICS_INLINE_FILE_LIMIT - 1 },
    }], "src", "run-1");
    assert.match(small, /data-artifact-mode="auto"/);
    assert.match(small, /data-artifact-state="pending"/);
    assert.doesNotMatch(small, /artifact-load-btn/);

    const large = renderRunEventItems([{
        seq: 2,
        artifact: { digest: "def", name: "large.txt", size: DIAGNOSTICS_INLINE_FILE_LIMIT },
    }], "src", "run-1");
    assert.match(large, /data-artifact-mode="manual"/);
    assert.match(large, /artifact-load-btn/);

    const unsized = renderRunEventItems([{
        seq: 3,
        artifact: { digest: "ghi", name: "unsized.txt" },
    }], "src", "run-1");
    assert.match(unsized, /data-artifact-mode="manual"/);

    const transcript = renderRunEventItems([{ name: "Agent transcript", seq: 4 }], "src", "run-1");
    assert.match(transcript, /data-artifact-mode="manual"/);
});

// formatDuration's sub-second branch: a run that finishes in under a second
// renders milliseconds rather than a rounded "0s", which is the difference
// between a useful number and one that reads as "instant".
test("telemetry insights render sub-second durations in milliseconds", () => {
    const html = renderTelemetryInsights({ executionMillis: 250 });
    assert.match(html, /250ms/);
    assert.doesNotMatch(html, /Execution time<\/div><div class="value">0s/);
});

test("telemetry insights render multi-second durations in seconds", () => {
    const html = renderTelemetryInsights({ executionMillis: 12000 });
    assert.match(html, /12s/);
});

// "Run duration" and "Repasses" already appear in the Summary tab; the
// Telemetry insights section should not repeat them.
test("telemetry insights omit Run duration and Repasses, which the Summary tab already shows", () => {
    const html = renderTelemetryInsights({
        startedAt: "2026-08-28T10:00:00Z",
        finishedAt: "2026-08-28T10:00:12Z",
        repassCount: 2,
    });
    assert.doesNotMatch(html, />Run duration</);
    assert.doesNotMatch(html, />Repasses</);
});

// ---- Insights tab ----

test("INSIGHT_WINDOWS matches the portal's InsightPage.tsx labels and values exactly", () => {
    assert.deepEqual(INSIGHT_WINDOWS, [
        { value: "24h", label: "Last 24 hours" },
        { value: "7d", label: "Last 7 days" },
        { value: "30d", label: "Last 30 days" },
        { value: "all", label: "All time" },
    ]);
});

test("insight window range computes since/until for bounded windows and omits since for all time", () => {
    const now = new Date("2026-01-08T00:00:00Z");
    assert.deepEqual(insightWindowRange("24h", now), {
        since: "2026-01-07T00:00:00.000Z",
        until: "2026-01-08T00:00:00.000Z",
    });
    assert.deepEqual(insightWindowRange("7d", now), {
        since: "2026-01-01T00:00:00.000Z",
        until: "2026-01-08T00:00:00.000Z",
    });
    assert.deepEqual(insightWindowRange("all", now), { until: "2026-01-08T00:00:00.000Z" });
});

test("insight previous window range is the same-length window immediately before, and undefined for all time", () => {
    const now = new Date("2026-01-08T00:00:00Z");
    assert.deepEqual(insightPreviousWindowRange("24h", now), {
        since: "2026-01-06T00:00:00.000Z",
        until: "2026-01-07T00:00:00.000Z",
    });
    assert.equal(insightPreviousWindowRange("all", now), undefined);
});

test("insight scope parses, round-trips through its value encoding, and labels", () => {
    assert.deepEqual(parseInsightScope(""), { kind: "instance" });
    assert.deepEqual(parseInsightScope("instance"), { kind: "instance" });
    assert.deepEqual(parseInsightScope("bogus-value"), { kind: "instance" });
    assert.deepEqual(parseInsightScope("gaggle:core"), { kind: "gaggle", gaggle: "core" });
    assert.deepEqual(parseInsightScope("workflow:core|implementation"), {
        kind: "workflow",
        gaggle: "core",
        workflow: "implementation",
    });
    assert.deepEqual(parseInsightScope("workflow:missing-separator"), { kind: "instance" });

    for (const scope of [
        { kind: "instance" },
        { kind: "gaggle", gaggle: "core" },
        { kind: "workflow", gaggle: "core", workflow: "implementation" },
    ]) {
        assert.deepEqual(parseInsightScope(insightScopeValue(scope)), scope);
    }
    assert.equal(insightScopeLabel({ kind: "instance" }), "Instance");
    assert.match(insightScopeLabel({ kind: "gaggle", gaggle: "core" }), /Gaggle.*core/);
    assert.match(insightScopeLabel({ kind: "workflow", gaggle: "core", workflow: "implementation" }), /Workflow.*core \/ implementation/);
});

test("insight scope API params include only the identifying fields for each scope kind", () => {
    assert.deepEqual(insightScopeApiParams({ kind: "instance" }), {});
    assert.deepEqual(insightScopeApiParams({ kind: "gaggle", gaggle: "core" }), { gaggle: "core" });
    assert.deepEqual(
        insightScopeApiParams({ kind: "workflow", gaggle: "core", workflow: "implementation" }),
        { gaggle: "core", workflow: "implementation" },
    );
});

test("insight scope options are derived from the loaded stats' gaggles and runs", () => {
    const options = insightScopeOptionsFromStats({
        gaggles: [{ gaggle: "core" }, { gaggle: "extra" }],
        runs: [{ gaggle: "core", workflow: "implementation" }],
    });
    assert.deepEqual(options.map((o) => o.value), [
        "instance",
        "gaggle:core",
        "gaggle:extra",
        "workflow:core|implementation",
    ]);
});

test("insight request params combine scope, window, and trend math for a bounded window", () => {
    const now = new Date("2026-01-08T00:00:00Z");
    const params = insightRequestParams({ kind: "gaggle", gaggle: "core" }, "24h", now);
    assert.equal(params.gaggle, "core");
    assert.equal(params.since, "2026-01-07T00:00:00.000Z");
    assert.equal(params.until, "2026-01-08T00:00:00.000Z");
    assert.equal(params.trendSince, "2026-01-06T00:00:00.000Z");
    assert.equal(params.trendUntil, "2026-01-08T00:00:00.000Z");
    assert.equal(params.trendBuckets, 16);
    assert.equal(params.trendPreviousSince, "2026-01-06T00:00:00.000Z");
    assert.equal(params.trendPreviousUntil, "2026-01-07T00:00:00.000Z");
});

test("insight request params omit trend fields entirely for the all-time window", () => {
    const params = insightRequestParams({ kind: "instance" }, "all", new Date("2026-01-08T00:00:00Z"));
    assert.deepEqual(Object.keys(params).sort(), ["until"]);
});

test("isInInsightScope filters by gaggle and workflow identity per scope kind", () => {
    const instance = { kind: "instance" };
    const gaggle = { kind: "gaggle", gaggle: "core" };
    const workflow = { kind: "workflow", gaggle: "core", workflow: "implementation" };
    assert.equal(isInInsightScope(instance, { gaggle: "anything" }), true);
    assert.equal(isInInsightScope(gaggle, { gaggle: "core" }), true);
    assert.equal(isInInsightScope(gaggle, { gaggle: "other" }), false);
    assert.equal(isInInsightScope(workflow, { gaggle: "core", workflow: "implementation" }), true);
    assert.equal(isInInsightScope(workflow, { gaggle: "core", workflow: "other" }), false);
});

test("insightUsageForScope matches the usage entry tagged with the scope's own kind and identity", () => {
    const stats = {
        usage: [
            { scope: "instance", costUSD: 1 },
            { scope: "gaggle", gaggle: "core", costUSD: 2 },
            { scope: "gaggle", gaggle: "other", costUSD: 3 },
        ],
    };
    assert.equal(insightUsageForScope(stats, { kind: "instance" }).costUSD, 1);
    assert.equal(insightUsageForScope(stats, { kind: "gaggle", gaggle: "core" }).costUSD, 2);
    assert.equal(insightUsageForScope(stats, { kind: "gaggle", gaggle: "missing" }), undefined);
});

// A realistic instance-scope fixture exercising every major TelemetryStatsResult
// section the panel renders: outcome breakdown, curation/ready-pool health,
// credit assignment, usage, cost trend, and stage hotspots.
const INSTANCE_STATS_FIXTURE = {
    gaggles: [
        { gaggle: "core", totalRuns: 20, completedRuns: 16, failedRuns: 3, otherRuns: 1, infraFailedRuns: 1, successRate: 0.8421 },
        { gaggle: "extra", totalRuns: 5, completedRuns: 5, failedRuns: 0, otherRuns: 0, infraFailedRuns: 0, successRate: 1 },
    ],
    runs: [],
    stages: [
        {
            gaggle: "core", workflow: "implementation", stage: "implement",
            p50DurationMs: 4200, p95DurationMs: 9800, avgDurationMs: 5000, durationSamples: 12,
        },
    ],
    usage: [
        {
            scope: "instance", totalAttempts: 25, p50Tokens: 1200, p95Tokens: 4300,
            costUSD: 12.34, p50CostUSD: 0.4, p95CostUSD: 1.1, costSamples: 25,
            retryWasteAttempts: 2, retryWasteTokens: 900, retryWasteCostUSD: 0.75,
        },
    ],
    models: [],
    creditAssignment: [
        {
            gaggle: "core", workflow: "implementation", stage: "implement", kind: "stage",
            failureShare: 0.62, failureRuns: 3, escalationRuns: 1, retryWasteAttempts: 2,
        },
    ],
    causalCredit: [],
    graphAnalytics: {},
    promotionSignals: [],
    promotionCandidates: [],
    curation: { runs: 4, ready: 2, needsHuman: 1, closed: 1, everRecorded: true },
    readyPool: {
        depth: 3, starved: false, oldestAgeSeconds: 120, averageClaimAgeSeconds: 45,
        inFlightClaimSamples: 1, averageInFlightClaimAgeSeconds: 30,
        bounceRate: 0.1, bounceEverRecorded: true,
        forwardCurationThroughput: 2, implementationDemand: 3,
        sampleEverRecorded: true,
    },
    trend: [
        { since: "2026-01-06T00:00:00Z", until: "2026-01-06T12:00:00Z", usage: [{ scope: "instance", costUSD: 3, p50Tokens: 900, costSamples: 5 }] },
        { since: "2026-01-06T12:00:00Z", until: "2026-01-07T00:00:00Z", usage: [{ scope: "instance", costUSD: 4, p50Tokens: 1000, costSamples: 6 }] },
        { since: "2026-01-07T00:00:00Z", until: "2026-01-07T12:00:00Z", usage: [{ scope: "instance", costUSD: 2.5, p50Tokens: 800, costSamples: 4 }] },
        { since: "2026-01-07T12:00:00Z", until: "2026-01-08T00:00:00Z", usage: [{ scope: "instance", costUSD: 2.85, p50Tokens: 850, costSamples: 5 }] },
    ],
    trendPrevious: { since: "2026-01-06T00:00:00Z", until: "2026-01-07T00:00:00Z", usage: [{ scope: "instance", costUSD: 7 }] },
};

test("renderInsightPanel renders outcome, curation, credit, usage, trend, and stage sections for instance scope", () => {
    const html = renderInsightPanel(INSTANCE_STATS_FIXTURE, { kind: "instance" }, "24h");
    // Outcome breakdown: per-gaggle rows plus the recomputed instance summary.
    assert.match(html, /Success and failure/);
    assert.match(html, /core/);
    assert.match(html, /extra/);
    // Curation / ready-pool health (instance-scope only).
    assert.match(html, /Ready-pool health/);
    assert.match(html, />3</);
    // Highest-contributing nodes (credit assignment).
    assert.match(html, /Highest-contributing nodes/);
    assert.match(html, /implement/);
    // Usage / tokens and retry waste.
    assert.match(html, /Tokens and retry waste/);
    assert.match(html, /\$12\.34/);
    // Cost trend: only the most recent 8 (bucket count for 24h) buckets show, all 4 fixture buckets included here.
    assert.match(html, /Cost over time/);
    assert.match(html, /\$3\.00/);
    // Slowest stages.
    assert.match(html, /Slowest stages/);
    assert.match(html, /9\.8s/);
});

test("renderInsightPanel scopes outcome and stage sections down to a workflow, and hides curation health", () => {
    const html = renderInsightPanel(INSTANCE_STATS_FIXTURE, { kind: "workflow", gaggle: "core", workflow: "implementation" }, "24h");
    assert.doesNotMatch(html, /Ready-pool health/);
    assert.match(html, /Slowest stages/);
    assert.match(html, /implement/);
});

test("renderInsightPanel shows the all-time trend note instead of bucketed data", () => {
    const html = renderInsightPanel(INSTANCE_STATS_FIXTURE, { kind: "instance" }, "all");
    assert.match(html, /bounded time window/);
});

test("renderInsightPanel renders an explicit empty state when nothing is recorded for the scope", () => {
    const emptyStats = {
        gaggles: [], runs: [], stages: [], usage: [], creditAssignment: [],
        curation: { runs: 0, ready: 0, needsHuman: 0, closed: 0, everRecorded: false },
        readyPool: {},
    };
    const html = renderInsightPanel(emptyStats, { kind: "instance" }, "24h");
    assert.match(html, /No telemetry in this window/);
});

test("renderInsightPanel reports no stats loaded yet before any fetch completes", () => {
    const html = renderInsightPanel(null, { kind: "instance" }, "24h");
    assert.match(html, /No telemetry loaded yet/);
});

const WORKFLOW_SCOPED_STATS_FIXTURE = {
    gaggles: [{ gaggle: "core", totalRuns: 6, completedRuns: 5, failedRuns: 1, otherRuns: 0, infraFailedRuns: 0, successRate: 0.8333 }],
    runs: [
        { gaggle: "core", workflow: "implementation", totalRuns: 6, completedRuns: 5, failedRuns: 1, otherRuns: 0, successRate: 0.8333 },
        { gaggle: "core", workflow: "other-workflow", totalRuns: 2, completedRuns: 2, failedRuns: 0, otherRuns: 0, successRate: 1 },
    ],
    stages: [],
    usage: [],
    creditAssignment: [],
    curation: { runs: 3, ready: 0, needsHuman: 0, closed: 0, everRecorded: false },
    readyPool: {},
};

test("renderInsightPanel derives the gaggle-scope outcome breakdown from stats.runs", () => {
    const html = renderInsightPanel(WORKFLOW_SCOPED_STATS_FIXTURE, { kind: "gaggle", gaggle: "core" }, "24h");
    assert.match(html, /Success and failure/);
    assert.match(html, /core \/ implementation/);
    assert.match(html, /core \/ other-workflow/);
});

test("renderInsightPanel derives the workflow-scope outcome summary from stats.runs", () => {
    const html = renderInsightPanel(
        WORKFLOW_SCOPED_STATS_FIXTURE,
        { kind: "workflow", gaggle: "core", workflow: "implementation" },
        "24h",
    );
    assert.match(html, /Success and failure/);
    assert.match(html, /insight-outcome-summary/);
});

test("renderInsightPanel labels an unrecorded ready pool as never recorded rather than a measured zero", () => {
    const html = renderInsightPanel(WORKFLOW_SCOPED_STATS_FIXTURE, { kind: "instance" }, "24h");
    assert.match(html, /Ready-pool health/);
    assert.match(html, /Never recorded/);
});

test("renderInsightPanel escapes hostile gaggle and stage identifiers", () => {
    const html = renderInsightPanel(
        {
            gaggles: [{ gaggle: "<img src=x onerror=alert(1)>", totalRuns: 1, completedRuns: 1, failedRuns: 0, otherRuns: 0, infraFailedRuns: 0, successRate: 1 }],
            runs: [], stages: [], usage: [], creditAssignment: [],
            curation: {}, readyPool: {},
        },
        { kind: "instance" },
        "24h",
    );
    assert.doesNotMatch(html, /<img src=x/);
    assert.match(html, /&lt;img/);
});

test("the browser script inlines every Insights helper and render function", () => {
    const page = renderHtml("inst-1");
    for (const name of [
        "INSIGHT_WINDOWS", "parseInsightScope", "insightScopeValue", "insightScopeLabel",
        "insightRequestParams", "isInInsightScope", "renderInsightPanel",
    ]) {
        assert.match(page, new RegExp("const " + name + " = "), `${name} was not inlined into the browser script`);
    }
    assert.ok(!page.includes("escapeAssociationHtml"), "escapeAssociationHtml leaked into the Insights panel");
});

// ---- Cost tab ----

test("cost request params use the selected window and bound all-time attribution to 90 days", () => {
    const now = new Date("2026-01-08T00:00:00Z");
    assert.deepEqual(costSummaryRequestParams("7d", now), {
        scope: "summary",
        since: "2026-01-01T00:00:00.000Z",
        until: "2026-01-08T00:00:00.000Z",
    });
    assert.deepEqual(costSummaryRequestParams("all", now), {
        scope: "summary",
        since: "2025-10-10T00:00:00.000Z",
        until: "2026-01-08T00:00:00.000Z",
    });
});

test("cost lookup params map PR and issue lookups to telemetry cost scopes", () => {
    const now = new Date("2026-01-08T00:00:00Z");
    assert.deepEqual(costLookupRequestParams("pr", "github", "5183", "24h", now), {
        provider: "github",
        scope: "pr",
        id: "5183",
        since: "2026-01-07T00:00:00.000Z",
        until: "2026-01-08T00:00:00.000Z",
    });
    assert.equal(costLookupRequestParams("issue", "ado", "42", "24h", now).scope, "issue");
});

const COST_STATS_FIXTURE = {
    gaggles: [{ gaggle: "core" }, { gaggle: "extra" }],
    runs: [{ gaggle: "core", workflow: "implementation" }],
    stages: [],
    usage: [
        {
            scope: "instance", totalAttempts: 20, costUSD: 15.5, p50CostUSD: 0.7,
            p95CostUSD: 1.8, costSamples: 18, p50Tokens: 1200, p95Tokens: 4400,
            retryWasteAttempts: 2, retryWasteCostUSD: 1.25, retryWasteTokens: 900,
        },
        {
            scope: "gaggle", gaggle: "core", totalAttempts: 12, costUSD: 10,
            p50CostUSD: 0.6, p95CostUSD: 1.4, costSamples: 10, p50Tokens: 1000, p95Tokens: 3000,
            retryWasteAttempts: 1, retryWasteCostUSD: 0.5, retryWasteTokens: 400,
        },
    ],
    trend: [
        {
            since: "2026-01-01T00:00:00Z",
            until: "2026-01-02T00:00:00Z",
            usage: [{ scope: "instance", costUSD: 7, p50Tokens: 800, costSamples: 8 }],
        },
        {
            since: "2026-01-02T00:00:00Z",
            until: "2026-01-03T00:00:00Z",
            usage: [{ scope: "instance", costUSD: 8.5, p50Tokens: 900, costSamples: 10 }],
        },
    ],
    trendPrevious: {
        usage: [{ scope: "instance", costUSD: 5 }],
    },
};

const COST_RESULT_FIXTURE = {
    provider: "github",
    scope: "summary",
    since: "2026-01-01T00:00:00Z",
    until: "2026-01-08T00:00:00Z",
    pullRequests: [
        {
            provider: "github",
            repository: "Agent-Clubhouse/Goobers",
            url: "https://github.com/Agent-Clubhouse/Goobers/pull/5183",
            externalKind: "pr",
            externalId: "5183",
            nativeTotals: [{ unit: "usd", value: 4.2, estimated: false }],
            normalizedTotals: [{ unit: "aiCredits", value: 420, estimated: false }],
            coverage: { totalRuns: 2, measuredRuns: 2, totalAttempts: 3, measuredAttempts: 3, complete: true, lowerBound: false },
            models: [
                {
                    model: "gpt-test",
                    measuredAttempts: 3,
                    nativeTotals: [{ unit: "usd", value: 4.2, estimated: false }],
                    normalizedTotals: [{ unit: "aiCredits", value: 420, estimated: false }],
                },
            ],
            runs: [{ runId: "run-1" }, { runId: "run-2" }],
        },
    ],
    issues: [
        {
            provider: "github",
            externalKind: "issue",
            externalId: "99",
            nativeTotals: [{ unit: "aiCredits", value: 12, estimated: true }],
            normalizedTotals: [{ unit: "usd", value: 1.5, estimated: true }],
            coverage: { totalRuns: 2, measuredRuns: 1, totalAttempts: 4, measuredAttempts: 2, complete: false, lowerBound: true },
            models: [],
            runs: [{ runId: "run-3" }],
        },
    ],
};

test("deriveExternalCostRows normalizes PR and issue cost aggregates", () => {
    const rows = deriveExternalCostRows(COST_RESULT_FIXTURE);
    assert.deepEqual(rows.map((row) => row.label), ["PR #5183", "Issue #99"]);
    assert.equal(rows[0].native, "$4.20");
    assert.equal(rows[1].native, "12 credits est.");
    assert.equal(rows[1].lowerBound, true);
    assert.deepEqual(rows[0].runs, ["run-1", "run-2"]);
});

test("renderCostPanel renders summary, trend, instance rollup, external breakdown, and lookup result", () => {
    const lookup = { ...COST_RESULT_FIXTURE, scope: "pr", externalId: "5183", issues: [] };
    const html = renderCostPanel(COST_STATS_FIXTURE, COST_RESULT_FIXTURE, { kind: "instance" }, "7d", lookup);
    assert.match(html, /Cost summary/);
    assert.match(html, /\$15\.50/);
    assert.match(html, /Cost trend/);
    assert.match(html, /Cost by gaggle/);
    assert.match(html, /core/);
    assert.match(html, /Cost by pull request and issue/);
    assert.match(html, /PR #5183/);
    assert.match(html, /Issue #99/);
    assert.match(html, /Lookup result/);
    assert.match(html, /lower bound/);
    assert.match(html, /Attribution is instance-wide/);
});

test("renderCostPanel orders mixed native and normalized costs by comparable USD value", () => {
    const html = renderCostPanel(COST_STATS_FIXTURE, {
        pullRequests: [{
            provider: "github",
            externalKind: "pr",
            externalId: "400",
            nativeTotals: [{ unit: "usd", value: 400, estimated: false }],
            normalizedTotals: [{ unit: "aiCredits", value: 40000, estimated: false }],
            coverage: { totalRuns: 1, measuredRuns: 1, totalAttempts: 1, measuredAttempts: 1, complete: true, lowerBound: false },
            models: [],
            runs: [],
        }],
        issues: [{
            provider: "github",
            externalKind: "issue",
            externalId: "1",
            nativeTotals: [{ unit: "aiCredits", value: 12, estimated: true }],
            normalizedTotals: [{ unit: "usd", value: 0.01, estimated: true }],
            coverage: { totalRuns: 1, measuredRuns: 1, totalAttempts: 1, measuredAttempts: 1, complete: true, lowerBound: false },
            models: [],
            runs: [],
        }],
    }, { kind: "instance" }, "7d");
    assert.ok(html.indexOf("PR #400") < html.indexOf("Issue #1"));
});

test("renderCostPanel hides instance rollup outside instance scope and renders empty states", () => {
    const scoped = renderCostPanel(COST_STATS_FIXTURE, { pullRequests: [], issues: [] }, { kind: "gaggle", gaggle: "core" }, "all");
    assert.doesNotMatch(scoped, /Cost by gaggle/);
    assert.match(scoped, /bounded time window/);
    assert.match(scoped, /No pull request or issue cost was attributed/);

    const empty = renderCostPanel(null, null, { kind: "instance" }, "7d");
    assert.match(empty, /No cost telemetry loaded yet/);

    const costsOnly = renderCostPanel(null, COST_RESULT_FIXTURE, { kind: "instance" }, "7d");
    assert.match(costsOnly, /Selected-scope usage, trend, and instance rollup are unavailable/);
    assert.match(costsOnly, /PR #5183/);

    const lookupOnly = renderCostPanel(null, null, { kind: "instance" }, "7d", { ...COST_RESULT_FIXTURE, pullRequests: [], scope: "issue" });
    assert.match(lookupOnly, /Attributed pull request and issue costs could not be loaded/);
    assert.match(lookupOnly, /Lookup result/);
    assert.match(lookupOnly, /Issue #99/);
});

test("renderCostPanel escapes hostile external cost payloads", () => {
    const hostile = {
        pullRequests: [{
            provider: "github",
            repository: HOSTILE,
            url: "javascript:alert(1)",
            externalKind: "pr",
            externalId: HOSTILE,
            nativeTotals: [],
            normalizedTotals: [],
            coverage: { totalRuns: 0, measuredRuns: 0, totalAttempts: 0, measuredAttempts: 0, complete: false, lowerBound: false },
            models: [{ model: HOSTILE, measuredAttempts: 1, normalizedTotals: [] }],
            runs: [{ runId: HOSTILE }],
        }],
        issues: [],
    };
    const html = renderCostPanel(COST_STATS_FIXTURE, hostile, { kind: "instance" }, "7d");
    assert.doesNotMatch(html, /<img src=x/);
    assert.doesNotMatch(html, /javascript:alert/);
    assert.match(html, /&lt;img/);
});

test("renderCostPanel makes bounded all-time attribution explicit", () => {
    const html = renderCostPanel(
        COST_STATS_FIXTURE,
        { ...COST_RESULT_FIXTURE, boundedAllTime: true },
        { kind: "instance" },
        "all",
    );
    assert.match(html, /all-time attribution is capped at 90 days/);
});

test("renderCostPanel caps external rows and per-row model details", () => {
    const manyModels = Array.from({ length: 5 }, (_, index) => ({
        model: "model-" + index,
        measuredAttempts: 1,
        nativeTotals: [],
        normalizedTotals: [{ unit: "aiCredits", value: index + 1, estimated: false }],
    }));
    const manyRows = Array.from({ length: 30 }, (_, index) => ({
        provider: "github",
        externalKind: "pr",
        externalId: String(index + 1),
        nativeTotals: [{ unit: "usd", value: index, estimated: false }],
        normalizedTotals: [{ unit: "aiCredits", value: index, estimated: false }],
        coverage: { totalRuns: 1, measuredRuns: 1, totalAttempts: 1, measuredAttempts: 1, complete: true, lowerBound: false },
        models: index === 29 ? manyModels : [],
        runs: [],
    }));
    const html = renderCostPanel(COST_STATS_FIXTURE, { pullRequests: manyRows, issues: [] }, { kind: "instance" }, "7d");
    assert.match(html, /\+5 more work items/);
    assert.match(html, /model-0/);
    assert.match(html, /\+2 more/);
    assert.doesNotMatch(html, /PR #1</);
});

test("renderHtml includes Cost tab controls and inlines every Cost helper", () => {
    const page = renderHtml("inst-1");
    assert.match(page, /dashboard-tab-cost/);
    assert.match(page, /id="cost-lookup-id"/);
    for (const name of [
        "costSummaryRequestParams", "costLookupRequestParams", "deriveExternalCostRows",
        "renderCostPanel",
    ]) {
        assert.match(page, new RegExp("const " + name + " = "), `${name} was not inlined into the browser script`);
    }
    assert.ok(!page.includes("escapeAssociationHtml"), "escapeAssociationHtml leaked into the Cost panel");
});

// ---- Work Items tab ----

const WORK_ITEM_PAGE_FIXTURE = {
    items: [{
        provider: "github",
        repository: "acme/app",
        kind: "pr",
        externalId: "42",
        actionCount: 2,
        lastOperation: "request-review",
        lastActionAt: "2026-09-15T00:00:00Z",
        lastRunId: "run-2",
        gaggle: "core",
        workflow: "implementation",
    }, {
        provider: "github",
        repository: "acme/service",
        kind: "issue",
        externalId: "77",
        actionCount: 1,
        lastOperation: "comment",
        lastActionAt: "2026-09-14T00:00:00Z",
        lastRunId: "run-1",
        gaggle: "tools",
        workflow: "triage",
    }],
    hasMore: true,
};

const WORK_ITEM_DETAIL_FIXTURE = {
    provider: "github",
    repository: "acme/app",
    kind: "pr",
    externalId: "42",
    url: "https://github.com/acme/app/pull/42",
    cost: {
        costUSD: 1.25,
        totalRuns: 2,
        measuredRuns: 1,
        totalAttempts: 3,
        measuredAttempts: 2,
        lowerBound: true,
    },
    relatedPullRequests: [{
        provider: "github",
        repository: "acme/app",
        kind: "pr",
        externalId: "43",
        url: "https://github.com/acme/app/pull/43",
    }],
    actions: [{
        runId: "run-2",
        sequence: 9,
        operation: "merge",
        occurredAt: "2026-09-15T00:00:00Z",
        gaggle: "core",
        workflow: "merge-review",
        runStatus: "completed",
    }, {
        runId: "run-1",
        sequence: 4,
        operation: "comment",
        occurredAt: "2026-09-14T00:00:00Z",
        gaggle: "core",
        workflow: "implementation",
        runStatus: "completed",
    }],
    truncated: true,
};

test("work item helpers format labels, operations, costs, and local filters", () => {
    assert.equal(workItemLabel("acme/app", "42"), "acme/app#42");
    assert.equal(workItemLabel("", "42"), "#42");
    assert.equal(humanizeWorkItemOperation("request-review"), "Request Review");
    assert.equal(humanizeWorkItemOperation(""), "Provider action");
    assert.equal(formatWorkItemCost({ costUSD: 1.25 }), "$1.25");
    assert.equal(formatWorkItemCost({ nanoAIU: 1200 }), "1,200 nano-AIU");
    assert.equal(formatWorkItemCost(null), "Not attributed");
    assert.equal(formatWorkItemTimestamp("0001-01-01T00:00:00Z"), "\u2014");
    assert.deepEqual(
        filterWorkItems(WORK_ITEM_PAGE_FIXTURE.items, "core", "app#42").map((item) => item.externalId),
        ["42"],
    );
    assert.deepEqual(filterWorkItems(WORK_ITEM_PAGE_FIXTURE.items, "tools", "77").map((item) => item.externalId), ["77"]);
});

test("renderWorkItemList renders rows, metadata, overflow, and explicit empty states", () => {
    const html = renderWorkItemList(WORK_ITEM_PAGE_FIXTURE);
    assert.match(html, /Open PR #42 in acme\/app/);
    assert.match(html, /Request Review/);
    assert.match(html, /implementation/);
    assert.match(html, /Showing the 200 most recently actioned work items/);
    assert.match(
        renderWorkItemList(WORK_ITEM_PAGE_FIXTURE, "missing"),
        /No confirmed provider actions match.*Only the 200 most recently actioned work items are searched/,
    );
    assert.match(renderWorkItemList(null), /No work items loaded yet/);
});

test("renderWorkItemDetail renders cost coverage, related links, action filtering, and truncation", () => {
    const html = renderWorkItemDetail(WORK_ITEM_DETAIL_FIXTURE, "comment");
    assert.match(html, /acme\/app#42/);
    assert.match(html, /\$1\.25/);
    assert.match(html, /Lower bound; some usage is unmeasured/);
    assert.match(html, /1\/2 runs/);
    assert.match(html, /Open pull request/);
    assert.match(html, /Related PR acme\/app#43/);
    assert.match(html, /Action history for acme\/app#42/);
    assert.match(html, />Comment</);
    assert.doesNotMatch(html, /<strong>Merge<\/strong>/);
    assert.match(html, /data-work-item-run="run-1"/);
    assert.match(html, /Showing the 200 most recent actions/);
    assert.match(renderWorkItemDetail(WORK_ITEM_DETAIL_FIXTURE, "missing"), /No actions match this type/);
});

test("work item renderers escape hostile values and reject unsafe links", () => {
    const hostilePage = {
        items: [{
            provider: HOSTILE,
            repository: HOSTILE,
            kind: "issue",
            externalId: HOSTILE,
            actionCount: 1,
            lastOperation: HOSTILE,
            gaggle: HOSTILE,
            workflow: HOSTILE,
        }],
        hasMore: false,
    };
    const list = renderWorkItemList(hostilePage);
    assert.doesNotMatch(list, /<img src=x/);
    assert.match(list, /&lt;img/);

    const detail = renderWorkItemDetail({
        ...WORK_ITEM_DETAIL_FIXTURE,
        repository: HOSTILE,
        externalId: HOSTILE,
        url: "javascript:alert(1)",
        relatedPullRequests: [{
            repository: HOSTILE,
            externalId: HOSTILE,
            url: "javascript:alert(2)",
        }],
        actions: [{ ...WORK_ITEM_DETAIL_FIXTURE.actions[0], operation: HOSTILE }],
    });
    assert.doesNotMatch(detail, /<img src=x|javascript:alert/);
    assert.match(detail, /&lt;img/);
});

test("renderHtml includes Work Items controls and inlines every Work Items helper", () => {
    const page = renderHtml("inst-1");
    assert.match(page, /dashboard-tab-work-items/);
    assert.match(page, /id="work-item-search"/);
    assert.match(page, /id="work-item-content"/);
    for (const name of [
        "workItemLabel", "humanizeWorkItemOperation", "formatWorkItemTimestamp",
        "formatWorkItemCost", "filterWorkItems", "renderWorkItemList", "renderWorkItemDetail",
    ]) {
        assert.match(page, new RegExp("const " + name + " = "), `${name} was not inlined into the browser script`);
    }
    assert.ok(!page.includes("escapeAssociationHtml"), "escapeAssociationHtml leaked into the Work Items panel");
});
