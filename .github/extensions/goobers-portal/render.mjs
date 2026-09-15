// Renders the goobers-portal HTML shell. Kept out of extension.mjs so the
// wiring file stays focused on SDK plumbing.

import {
  createSnapshotFetcher,
  decodeStreamEvent,
  decodeViewState,
  encodeViewState,
  asString,
  deriveAttemptLineage,
  deriveFailureBreadcrumbs,
  deriveFreshnessState,
  deriveTelemetryInsights,
  numericValue,
  explicitMeasure,
  measureFromPayload,
  filterTranscriptEntries,
  isInvalidCursorError,
  mergeRunPage,
  normalizeViewFilters,
  shouldApplyRestoredFilters,
} from "./ux.mjs";
import {
  configurationWarningKey,
  groupConfigurationWarnings,
  renderConfigurationWarnings,
  sortConfigurationWarnings,
  warningRemediation,
} from "./configuration-warnings.mjs";

function escapeAssociationHtml(value) {
    return String(value).replace(/[&<>"']/g, (character) => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
    })[character]);
}

function updateFleetPanel(data) {
  const fleet = data.fleet || {};
  const fleetPanelEl = document.getElementById("fleet-panel");
  const href = safeExternalUrl(fleet.canonicalUri);
  fleetPanelEl.replaceChildren();
  if (fleet.associated && href) {
    fleetPanelEl.hidden = false;
    const label = document.createElement("strong");
    label.textContent = "Fleet";
    const meta = document.createElement("span");
    meta.className = "muted";
    meta.textContent = fleet.displayName || fleet.fleetId || "Associated";
    const link = document.createElement("a");
    link.id = "fleet-portal-link";
    link.className = "actions-run-link";
    link.href = href;
    link.target = "_blank";
    link.rel = "noopener noreferrer";
    link.textContent = "Open Fleet portal" + (fleet.connectionState ? " (" + fleet.connectionState + ")" : "") + " \u2197";
    fleetPanelEl.append(label, meta, link);
    return;
  }
  if (data.instance?.fleetEnrolled || fleet.associated) {
    fleetPanelEl.hidden = false;
    const label = document.createElement("strong");
    label.textContent = "Fleet";
    const meta = document.createElement("span");
    meta.className = "muted";
    meta.textContent = "This instance is enrolled, but its Fleet portal URL is not available from the selected source.";
    fleetPanelEl.append(label, meta);
    return;
  }
  if (fleet.reason) {
    fleetPanelEl.hidden = false;
    fleetPanelEl.textContent = "Fleet status unavailable: " + fleet.reason;
    return;
  }
  fleetPanelEl.hidden = true;
}

function safeAssociationUrl(value) {
    try {
        const url = new URL(value);
        return url.protocol === "http:" || url.protocol === "https:" ? url.href : "";
    } catch {
        return "";
    }
}

export function gooberAvatar(value) {
    const text = String(value || "").toLowerCase();
    if (/review|verify|judge/.test(text)) return "🧐";
    if (/open-pr|opener|pr/.test(text)) return "🚪";
    if (/implement|build|fix|code/.test(text)) return "🛠";
    if (/curate|triage|plan|proposal/.test(text)) return "🧭";
    if (/merge|land|ship/.test(text)) return "🚀";
    if (/test|validate/.test(text)) return "🧪";
    if (/docs|document/.test(text)) return "📚";
    if (/gate|approval|approve|blocked|lock/.test(text)) return "🔒";
    return "✨";
}

export function renderGooberChip(value, options = {}) {
    const label = String(value || "").trim();
    if (!label) return "";
    const kind = options.kind ? ' data-kind="' + escapeAssociationHtml(options.kind) + '"' : "";
    return '<span class="goober-chip"' + kind + ' title="' + escapeAssociationHtml(label) + '">' +
        '<span class="goober-avatar" aria-hidden="true">' + escapeAssociationHtml(gooberAvatar(label)) + "</span>" +
        '<span class="goober-label">' + escapeAssociationHtml(label) + "</span></span>";
}

export function renderRunAssociations(operator) {
    const root = operator && operator.operator ? operator.operator : operator;
    const links = [];
    const seen = new Set();
    function canonicalRefUrl(href) {
        const url = new URL(href);
        url.hash = "";
        if (["github.com", "www.github.com"].includes(url.hostname) && !url.port &&
            /^\/[^/]+\/[^/]+\/(issues|pull)\/\d+\/?$/i.test(url.pathname)) {
            url.protocol = "https:";
            url.hostname = "github.com";
            url.pathname = url.pathname.toLowerCase().replace(/\/$/, "");
        }
        return url.href;
    }
    function addLink(kind, item, title) {
        if (!item) return;
        const href = safeAssociationUrl(item?.url || item?.htmlUrl || item?.webUrl);
        if (!href) return;
        const identity = item.number ?? item.id ?? item.externalId ?? "";
        const key = kind + ":" + canonicalRefUrl(href);
        if (seen.has(key)) return;
        seen.add(key);
        const status = String(item.state || item.status || item.phase || "").trim();
        const statusAttr = status ? ' data-status="' + escapeAssociationHtml(status.toLowerCase()) + '"' : "";
        const shortLabel = (identity ? "#" + identity : "") + (title ? ": " + String(title).trim() : "");
        const label = kind + (shortLabel ? " " + shortLabel : "");
        links.push('<a class="run-association-link work-chip" data-kind="' + escapeAssociationHtml(kind.toLowerCase()) +
            '"' + statusAttr + ' href="' + escapeAssociationHtml(href) +
            '" target="_blank" rel="noopener noreferrer" title="' + escapeAssociationHtml(label) + '">' +
            '<span class="work-chip-kind">' + escapeAssociationHtml(kind) + "</span> " +
            '<span class="work-chip-label">' + escapeAssociationHtml(shortLabel || kind) + "</span>" +
            (status ? ' <span class="work-chip-status">' + escapeAssociationHtml(status) + "</span>" : "") +
            "</a>");
    }
    function addRef(ref) {
        const kind = String(ref?.kind || ref?.type || "").toLowerCase();
        const identity = ref?.number ?? ref?.id ?? ref?.externalId ?? "";
        if (["issue", "work-item", "workitem"].includes(kind)) {
            const issueTitle = [
                root?.issue,
                operator?.issue,
                root?.workItem,
                operator?.workItem,
            ].find((item) => String(item?.number ?? item?.id ?? item?.externalId ?? "") === String(identity))?.title;
            addLink("Issue", ref, ref?.title || issueTitle);
        }
        if (["pr", "pull-request", "pullrequest"].includes(kind)) {
            const prTitle = [
                [root?.pullRequest, root?.pullRequestTitle || root?.pullRequest?.title],
                [operator?.pullRequest, operator?.pullRequestTitle || operator?.pullRequest?.title],
            ].find(([item]) => String(item?.number ?? item?.id ?? item?.externalId ?? "") === String(identity))?.[1];
            addLink("PR", ref, ref?.title || prTitle);
        }
    }
    [
        root?.issue,
        operator?.issue,
        root?.workItem,
        operator?.workItem,
    ].forEach((issue) => addLink("Issue", issue, issue?.title));
    [
        [root?.pullRequest, root?.pullRequestTitle || root?.pullRequest?.title],
        [operator?.pullRequest, operator?.pullRequestTitle || operator?.pullRequest?.title],
    ].forEach(([pullRequest, title]) => addLink("PR", pullRequest, title));
    [
        ...(Array.isArray(root?.refs) ? root.refs : []),
        ...(Array.isArray(operator?.refs) ? operator.refs : []),
        ...(Array.isArray(root?.externalRefs) ? root.externalRefs : []),
        ...(Array.isArray(operator?.externalRefs) ? operator.externalRefs : []),
    ].forEach(addRef);
    return links.length ? '<div class="run-associations">' + links.join("") + "</div>" : "\u2014";
}

function renderFullRunId(value) {
    const runId = String(value || "");
    return runId ? ' data-full-run-id="' + escapeAssociationHtml(runId) + '" title="' + escapeAssociationHtml(runId) + '"' : "";
}

export function renderRunIdControl(runId, options = {}) {
    const fullId = renderFullRunId(runId);
    const label = options.label || "Run id";
    return '<span class="run-id-control">' +
        '<button type="button" class="table-link" data-open-run="' + escapeAssociationHtml(runId || "") + '"' +
        fullId + ' aria-label="Open ' + escapeAssociationHtml(label) + '">' + escapeAssociationHtml(label) + "</button>" +
        '<button type="button" class="copy-run-id" data-copy-run-id="' + escapeAssociationHtml(runId || "") + '"' +
        fullId + ' aria-label="Copy run id" title="Copy run id">&#128203;</button>' +
        "</span>";
}

export function renderFleetPortalLink(fleet) {
    const href = safeAssociationUrl(fleet?.canonicalUri);
    if (!fleet?.associated || !href) return "";
    const status = fleet.connectionState ? " (" + fleet.connectionState + ")" : "";
    return '<a id="fleet-portal-link" class="actions-run-link" href="' + escapeAssociationHtml(href) +
        '" target="_blank" rel="noopener noreferrer">Open Fleet portal' +
        escapeAssociationHtml(status) + " &#8599;</a>";
}

// The snapshot cards and the run table interpolate values the portal does not
// control — an instance name from instance.yaml, run IDs, workflow and gaggle
// names, and phases all originate in a repository or a run's own metadata
// (#4567). Building those rows here, rather than inline in the browser
// script, is what makes them unit-testable against hostile input: both
// functions are inlined into the page verbatim via .toString(), with
// escapeAssociationHtml remapped onto the client's escapeHtml.

export function renderSnapshotCard(label, value) {
    return '<div class="label">' + escapeAssociationHtml(label) +
        '</div><div class="value">' + escapeAssociationHtml(value) + "</div>";
}

// parts.actionsLink and parts.associations are already-built HTML from
// safeExternalUrl and renderRunAssociations, which escape their own inputs;
// every value read off the run itself is escaped here.
export function renderRunRowCells(run, parts) {
    const cell = (value) => "<td>" + escapeAssociationHtml(value) + "</td>";
    const runId = (run && (run.runId || run.id)) || "";
    const extra = (parts && parts.actionsLink) || "";
    const associations = (parts && parts.associations) || "\u2014";
    return "<td>" + renderRunIdControl(runId) + extra + "</td>" +
        cell((run && run.workflow) || "") +
        cell((run && run.gaggle) || "") +
        cell((run && run.trigger && run.trigger.kind) || "\u2014") +
        '<td><span class="phase" data-phase="' + escapeAssociationHtml((run && run.phase) || "") + '">' + escapeAssociationHtml((run && run.phase) || "") + "</span></td>" +
        "<td>" + associations + "</td>" +
        cell((parts && parts.startedAt) || "") +
        cell((parts && parts.lastActivityAt) || "");
}

export function formatRunDetailTime(value) {
    if (!value) return "\u2014";
    try {
        return escapeAssociationHtml(new Date(value).toLocaleString());
    } catch {
        return escapeAssociationHtml(value);
    }
}

// Render the complete run metadata shell at a testable escaping boundary.
// parts.actionsLink is the only trusted markup; its call site constructs it
// from a protocol-checked and escaped URL.
export function renderRunDetailSummary(run = {}, parts = {}) {
    const transitions = Array.isArray(run.transitions) ? run.transitions : [];
    const events = Array.isArray(run.events) ? run.events : [];
    const finalTransition = [...transitions].reverse().find((transition) => transition && transition.terminal);
    const finishedStages = new Set(
        events.filter((event) => event && event.type === "stage.finished" && event.stage).map((event) => event.stage),
    );
    const workflow = escapeAssociationHtml(run.workflow || "");
    const workflowVersion = run.workflowVersion ? " v" + escapeAssociationHtml(run.workflowVersion) : "";
    const duration = Number.isFinite(Number(run.durationMillis)) && Number(run.durationMillis) > 0
        ? Math.round(Number(run.durationMillis) / 1000) + "s"
        : "\u2014";
    const activeStages = Array.isArray(run.activeStages) ? run.activeStages : [];
    const activeStageChips = activeStages
        .map((stage) => [
            renderGooberChip(stage?.goober, { kind: "goober" }),
            renderGooberChip(stage?.name || stage?.stage, { kind: "stage" }),
        ].filter(Boolean).join(" "))
        .filter(Boolean)
        .join(" ");
    const values = [
        ["Workflow", workflow + workflowVersion],
        ["Gaggle", escapeAssociationHtml(run.gaggle || "")],
        ["Phase", '<span class="phase" data-phase="' + escapeAssociationHtml(run.phase || "") + '">' + escapeAssociationHtml(run.phase || "") + "</span>"],
        ...(run.currentStage ? [["Current stage", renderGooberChip(run.currentStage, { kind: "stage" })]] : []),
        ...(activeStageChips ? [["Active goobers", activeStageChips]] : []),
        ["Final state", escapeAssociationHtml((finalTransition && (finalTransition.status || finalTransition.verdict)) || (run.terminal ? run.phase : "in progress"))],
        ["Completed stages", escapeAssociationHtml(finishedStages.size)],
        ["Started", formatRunDetailTime(run.startedAt)],
        ["Finished", formatRunDetailTime(run.finishedAt)],
        ["Duration", escapeAssociationHtml(duration)],
        ["Repasses", escapeAssociationHtml(run.repassCount ?? 0)],
        ["Retries", escapeAssociationHtml(run.retryCount ?? 0)],
        ["Trigger", escapeAssociationHtml((run.trigger && run.trigger.kind) || "")],
    ];
    const grid = values.map(([label, value]) => '<div class="kv"><div class="label">' +
        escapeAssociationHtml(label) + '</div><div class="value">' + value + "</div></div>").join("");
    return '<div class="run-header"><h2>' + workflow + "</h2>" +
        renderRunIdControl(run.id || run.runId || "") + (parts.actionsLink || "") + "</div>" +
        '<div class="internal-tabs" role="tablist" aria-label="Run detail sections">' +
        '<button id="run-tab-summary" role="tab" data-tab="summary" aria-controls="run-panel-summary">Summary</button>' +
        '<button id="run-tab-execution" role="tab" data-tab="execution" aria-controls="run-panel-execution">Execution</button>' +
        '<button id="run-tab-diagnostics" role="tab" data-tab="diagnostics" aria-controls="run-panel-diagnostics">Diagnostics</button>' +
        '<button id="run-tab-actions" role="tab" data-tab="actions" aria-controls="run-panel-actions">Actions</button>' +
        '</div><section id="run-panel-summary" role="tabpanel" aria-labelledby="run-tab-summary">' +
        '<div class="kv-grid">' + grid + "</div>";
}

function stageKindLabel(kind) {
    if (kind === "agentic") return "Agentic task";
    if (kind === "deterministic") return "Deterministic task";
    if (kind === "gate") return "Gate";
    if (kind === "parallel") return "Parallel";
    return "Stage";
}

function stageActor(stage) {
    if (stage.kind === "gate") {
        return stage.evaluator ? `${stage.evaluator} evaluator` : "Evaluator not declared";
    }
    if (stage.owner) return `${stage.owner.gaggle}/${stage.owner.name}`;
    if (stage.kind === "deterministic") return "Deterministic runtime";
    return "Owner not declared";
}

function stageProperty(label, value) {
    return "<div><dt>" + escapeAssociationHtml(label) + "</dt><dd>" +
        escapeAssociationHtml(value) + "</dd></div>";
}

export function renderStageDefinitionInspector(stage = {}, view = "fields") {
    const yamlSelected = view === "yaml";
    const capabilities = Array.isArray(stage.capabilities) && stage.capabilities.length
        ? stage.capabilities.join(", ")
        : "None declared";
    const retry = stage.retry?.maxAttempts !== undefined
        ? `${stage.retry.maxAttempts} attempt${stage.retry.maxAttempts === 1 ? "" : "s"}, ${stage.retry.backoffSeconds ?? 0}s backoff`
        : "No retry declared";
    const properties = [
        stageProperty(stage.kind === "gate" ? "Evaluator" : "Owner", stageActor(stage)),
        stageProperty("Capabilities", capabilities),
        stageProperty("Timeout", stage.timeoutSeconds ? `${stage.timeoutSeconds}s` : "Default"),
        stageProperty("Retry", retry),
    ];
    if (stage.kind === "gate") {
        const branches = stage.branches && Object.keys(stage.branches).length
            ? Object.entries(stage.branches)
                .map(([outcome, target]) => `${outcome} \u2192 ${target || "(terminal)"}`)
                .join(", ")
            : "None declared";
        properties.push(stageProperty("Branches", branches));
        properties.push(stageProperty("Max repasses", stage.maxRepasses ?? "Inherited"));
    } else {
        properties.push(stageProperty(
            "Policy actions",
            Array.isArray(stage.policyActions) && stage.policyActions.length
                ? stage.policyActions.join(", ")
                : "None declared",
        ));
        properties.push(stageProperty(
            "Required runner capabilities",
            Array.isArray(stage.requiredCapabilities) && stage.requiredCapabilities.length
                ? stage.requiredCapabilities.join(", ")
                : "None declared",
        ));
        properties.push(stageProperty("On timeout", stage.onTimeout || "fail (default)"));
    }
    return '<aside class="stage-definition-panel" aria-label="' +
        escapeAssociationHtml((stage.name || "Stage") + " definition") + '">' +
        '<div class="stage-inspector-heading"><span class="stage-kind-badge" data-kind="' +
        escapeAssociationHtml(stage.kind || "") + '">' + escapeAssociationHtml(stageKindLabel(stage.kind)) +
        "</span><h3>" + escapeAssociationHtml(stage.name || "Unnamed stage") + "</h3></div>" +
        '<p class="stage-inspector-description">' +
        escapeAssociationHtml(stage.goal || "No stage goal declared.") + "</p>" +
        '<div class="internal-tabs" role="tablist" aria-label="Stage config view">' +
        '<button id="stage-tab-fields" type="button" role="tab" data-tab="fields" aria-controls="stage-panel-fields"' +
        ' aria-selected="' + String(!yamlSelected) + '">Fields</button>' +
        '<button id="stage-tab-yaml" type="button" role="tab" data-tab="yaml" aria-controls="stage-panel-yaml"' +
        ' aria-selected="' + String(yamlSelected) + '">Raw YAML</button></div>' +
        '<section id="stage-panel-fields" role="tabpanel" aria-labelledby="stage-tab-fields"' +
        (yamlSelected ? " hidden" : "") + '><dl class="property-list">' + properties.join("") + "</dl></section>" +
        '<section id="stage-panel-yaml" role="tabpanel" aria-labelledby="stage-tab-yaml"' +
        (yamlSelected ? "" : " hidden") + '><pre class="code-block">' +
        escapeAssociationHtml(stage.rawYaml || "No YAML available.") + "</pre></section></aside>";
}

export function renderStageInspectorStatus(message, options = {}) {
    const role = options.error ? "alert" : "status";
    const className = options.error ? "stage-inspector-state stage-inspector-error" : "stage-inspector-state";
    return '<div class="' + className + '" role="' + role + '">' +
        escapeAssociationHtml(message) + "</div>";
}

export function renderRunEventItems(displayedEvents = [], sourceId = "", runId = "", options = {}) {
    const formatTime = options.formatTime || formatRunDetailTime;
    const safeUrl = options.safeUrl || safeAssociationUrl;
    return displayedEvents.map((event = {}) => {
        const status = event.status || event.verdict || event.decision || "";
        const stage = event.stage ? ' \u00b7 ' + renderGooberChip(event.stage, { kind: event.type && String(event.type).startsWith("gate.") ? "gate" : "stage" }) : "";
        const summary = '<summary><span class="event-seq">#' + escapeAssociationHtml(event.seq ?? "") +
            "</span><code>" + escapeAssociationHtml(event.type || "") + "</code><span>" + stage +
            (status ? " \u00b7 " + escapeAssociationHtml(status) : "") +
            '</span><span class="event-time">' + formatTime(event.time) + "</span></summary>";
        const artifacts = [];
        if (event.artifact) artifacts.push(event.artifact);
        for (const artifact of event.artifacts || []) artifacts.push(artifact);
        const seenDigests = new Set();
        const artifactLinks = artifacts.filter((artifact) => {
            if (!artifact || !artifact.digest || seenDigests.has(artifact.digest)) return false;
            seenDigests.add(artifact.digest);
            return true;
        }).map((artifact) => {
            const href = "/api/run-artifact?source=" + encodeURIComponent(sourceId) +
                "&id=" + encodeURIComponent(runId) + "&digest=" + encodeURIComponent(artifact.digest);
            const label = artifact.name || artifact.digest;
            return '<a href="' + href + '" target="_blank" rel="noopener">' +
                escapeAssociationHtml(label) + " (" + escapeAssociationHtml(artifact.size ?? "") + " bytes)</a>";
        });
        if (event.name && String(event.name).toLowerCase().includes("transcript")) {
            const href = "/api/run-transcript?source=" + encodeURIComponent(sourceId) +
                "&id=" + encodeURIComponent(runId) + "&seq=" + encodeURIComponent(event.seq);
            artifactLinks.push('<a href="' + href + '" target="_blank" rel="noopener">Agent transcript / messages</a>');
        }
        const externalUrl = event.externalRef && safeUrl(event.externalRef.url);
        const refHtml = externalUrl
            ? '<p>External: <a href="' + escapeAssociationHtml(externalUrl) +
              '" target="_blank" rel="noopener noreferrer">' +
              escapeAssociationHtml((event.externalRef.provider || "") + " " + (event.externalRef.kind || "") + " #" + (event.externalRef.id || "")) +
              "</a></p>"
            : "";
        const details = {};
        for (const key of ["outputs", "runner", "error", "rationale", "reason", "completeness", "raw"]) {
            if (event[key] !== undefined && event[key] !== null && event[key] !== "") details[key] = event[key];
        }
        const detailsHtml = Object.keys(details).length
            ? "<pre>" + escapeAssociationHtml(JSON.stringify(details, null, 2)) + "</pre>"
            : "";
        const linksHtml = artifactLinks.length
            ? '<div class="artifact-links">' + artifactLinks.join(" \u00b7 ") + "</div>"
            : "";
        return "<details>" + summary + '<div class="event-body">' + refHtml + detailsHtml + linksHtml + "</div></details>";
    }).join("");
}

export function renderTransitions(transitions = []) {
    if (!transitions.length) return '<p class="muted">No transitions recorded.</p>';
    const items = transitions.map((transition = {}) => {
        const verdict = transition.verdict ? escapeAssociationHtml(transition.verdict) : "";
        const verdictClass = verdict ? " badge-" + verdict : "";
        const arrow = transition.terminal ? "\u25a0" : "\u2192";
        const verdictText = verdict
            ? ' <span class="' + verdictClass.trim() + '">[' + verdict +
              (transition.repass ? ", repass" : "") + "]</span>"
            : "";
        const target = transition.target
            ? " " + arrow + " " + renderGooberChip(transition.target, { kind: "stage" })
            : (transition.status ? " (" + escapeAssociationHtml(transition.status) + ")" : "");
        return '<li><span class="seq">#' + escapeAssociationHtml(transition.seq ?? "") +
            "</span>" + renderGooberChip(transition.source || "", { kind: "stage" }) +
            target + verdictText + "</li>";
    });
    return '<ul class="transitions-list">' + items.join("") + "</ul>";
}

export function renderOperatorPanel(operator, refs = [], options = {}) {
    if (!operator) return "";
    const safeUrl = options.safeUrl || safeAssociationUrl;
    const parts = [];
    if (operator.issue) {
        const issueNumber = String(operator.issue.number ?? "");
        const issueRef = refs.find((ref) =>
            String(ref.id) === issueNumber &&
            ["issue", "work-item", "workitem"].includes(String(ref.kind || "").toLowerCase()) &&
            safeUrl(ref.url),
        );
        const issueLabel = escapeAssociationHtml("#" + issueNumber + " " + (operator.issue.title || ""));
        parts.push(["Issue", issueRef
            ? '<a href="' + escapeAssociationHtml(safeUrl(issueRef.url)) +
              '" target="_blank" rel="noopener noreferrer">' + issueLabel + "</a>"
            : issueLabel]);
    }
    if (operator.pullRequest) {
        const pullUrl = safeUrl(operator.pullRequest.url);
        const pullTitle = String(operator.pullRequestTitle || "").trim();
        const pullLabel = escapeAssociationHtml(
            (operator.pullRequest.provider || "") + " " + (operator.pullRequest.kind || "") + " #" +
            (operator.pullRequest.id || "") + (pullTitle ? ": " + pullTitle : ""),
        );
        parts.push(["Pull request", pullUrl
            ? '<a href="' + escapeAssociationHtml(pullUrl) +
              '" target="_blank" rel="noopener noreferrer">' + pullLabel + "</a>"
            : pullLabel]);
    }
    if (operator.liveness) parts.push(["Liveness", escapeAssociationHtml(operator.liveness)]);
    if (operator.trajectory) parts.push(["Trajectory", escapeAssociationHtml(operator.trajectory)]);
    if (operator.latestError) {
        parts.push(["Latest error", "<code>" + escapeAssociationHtml(operator.latestError.code || "") +
            "</code> " + escapeAssociationHtml(operator.latestError.message || "")]);
    }
    let markup = '<div class="kv-grid">' + parts.map(([label, value]) =>
        '<div class="kv' + (label === "Latest error" ? " kv-wide" : "") +
        '"><div class="label">' + escapeAssociationHtml(label) +
        '</div><div class="value">' + value + "</div></div>").join("") + "</div>";
    if (operator.review) {
        const verdict = escapeAssociationHtml(operator.review.verdict || "");
        markup += '<h3>Review</h3><p><span class="badge-' + verdict + '">' + verdict + "</span></p>";
        if (operator.review.rationale) {
            markup += '<p class="rationale">' + escapeAssociationHtml(operator.review.rationale) + "</p>";
        }
    }
    if (operator.pullRequest && operator.pullRequestBody) {
        markup += '<h3>Pull request description</h3><div class="pr-description">' +
            escapeAssociationHtml(operator.pullRequestBody) + "</div>";
    }
    if (operator.potentialBlockers && operator.potentialBlockers.length) {
        markup += '<h3>Potential blockers</h3><ul class="blockers-list">' +
            operator.potentialBlockers.map((blocker) => "<li>" + escapeAssociationHtml(blocker) + "</li>").join("") +
            "</ul>";
    }
    return markup;
}

export function renderGraphLegend() {
    return '<div class="graph-legend" aria-label="Workflow state legend">' +
        '<strong>State legend:</strong> ' +
        ['pending', 'running', 'succeeded', 'failed', 'skipped', 'blocked']
            .map((state) => '<span class="legend-chip ' + state + '"><span class="legend-swatch"></span>' + state.charAt(0).toUpperCase() + state.slice(1) + '</span>')
            .join(" ") +
        '</div>';
}

export function renderCausalDiagnosis(run = {}) {
    const diagnosis = deriveAttemptLineage(run);
    const breadcrumbs = deriveFailureBreadcrumbs(run);
    const attempts = diagnosis.attempts || [];
    const trace = attempts.length
        ? attempts.map((entry) => '<li>' + renderGooberChip(entry.stage, { kind: entry.kind || "stage" }) +
            ' · ' + (entry.attempt > 1 ? 'take ' + entry.attempt : 'attempt ' + entry.attempt) +
            ' · ' + escapeAssociationHtml(entry.status || 'pending') + '</li>').join("")
        : '<li class="muted">No stage attempt lineage is recorded yet.</li>';
    const breadcrumbHtml = breadcrumbs.length
        ? breadcrumbs.map((entry) => '<li><strong>' + escapeAssociationHtml(entry.label) + ':</strong> ' + escapeAssociationHtml(entry.detail || '') + (entry.attempt ? ' (attempt ' + entry.attempt + ')' : '') + '</li>').join("")
        : '<li class="muted">No failure breadcrumbs are available.</li>';
    return '<div class="causal-diagnosis">' +
        '<div class="kv-grid">' +
        '<div class="kv"><div class="label">Attempt lineage</div><div class="value"><ul class="causal-list">' + trace + '</ul></div></div>' +
        '<div class="kv kv-wide"><div class="label">Failure breadcrumb</div><div class="value"><ul class="causal-list">' + breadcrumbHtml + '</ul></div></div>' +
        '</div>' +
        '</div>';
}

export function renderExecutionWaterfall(run = {}) {
    const diagnosis = deriveAttemptLineage(run);
    const entries = diagnosis.attempts || [];
    if (!entries.length) {
        return '<p class="muted">No execution waterfall is available yet.</p>';
    }
    const timestamps = entries.flatMap((entry) => [entry.start, entry.end])
        .map((value) => value ? new Date(value).getTime() : NaN)
        .filter((value) => Number.isFinite(value));
    const startMs = timestamps.length ? Math.min(...timestamps) : 0;
    const endMs = timestamps.length ? Math.max(...timestamps) : 0;
    const duration = Math.max(1, endMs - startMs);
    const timelineAvailable = timestamps.length > 0;
    const rows = entries.map((entry) => {
        const itemStart = entry.start ? new Date(entry.start).getTime() : NaN;
        const itemEnd = entry.end ? new Date(entry.end).getTime() : itemStart;
        const knownTiming = Number.isFinite(itemStart) && Number.isFinite(itemEnd) && itemEnd >= itemStart;
        const left = knownTiming && timelineAvailable ? ((itemStart - startMs) / duration) * 100 : 0;
        const width = knownTiming && timelineAvailable ? Math.max(2, ((itemEnd - itemStart) / duration) * 100) : 0;
        const kind = entry.kind || "stage";
        const retry = entry.attempt > 1 ? " retry" : "";
        const blocked = String(entry.status || "").toLowerCase() === "blocked";
        const attemptLabel = entry.attempt > 1 ? "take " + entry.attempt : "attempt " + entry.attempt;
        const kindLabel = blocked && kind === "gate" ? "🔒 gate" : kind;
        const timing = knownTiming
            ? Math.round((itemEnd - itemStart) / 1000) + "s"
            : "timing unavailable";
        return '<div class="waterfall-row ' + (knownTiming ? "" : "unknown-timing") + '"><div class="waterfall-stage">' +
            renderGooberChip(entry.stage, { kind }) + ' · ' + escapeAssociationHtml(kindLabel) + ' · ' +
            escapeAssociationHtml(attemptLabel) + (retry ? ' · retry' : '') +
            '</div><div class="waterfall-bar-wrap"><span class="waterfall-bar ' +
            escapeAssociationHtml(entry.status || 'pending') + retry + '" style="left:' + left + '%; width:' + width + '%"></span></div><div class="waterfall-status">' +
            escapeAssociationHtml(entry.status || 'pending') + ' · ' + timing + '</div></div>';
    }).join("");
    const intervals = entries.map((entry) => [new Date(entry.start || "").getTime(), new Date(entry.end || "").getTime()])
        .filter(([start, end]) => Number.isFinite(start) && Number.isFinite(end) && end >= start)
        .sort((a, b) => a[0] - b[0]);
    const idleGaps = intervals.slice(1).map((interval, index) => interval[0] - intervals[index][1])
        .filter((gap) => gap > 0);
    const gapHtml = idleGaps.length
        ? '<p class="muted">Idle gaps: ' + idleGaps.map((gap) => Math.round(gap / 1000) + 's').join(", ") + '</p>'
        : "";
    return '<div class="execution-waterfall" aria-label="Execution waterfall">' + (timestamps.length ? '<p class="muted">Timeline: ' + new Date(startMs).toISOString() + ' – ' + new Date(endMs).toISOString() + '</p>' : '<p class="muted">No timestamps are available; bars show execution order only.</p>') + gapHtml + rows + '</div>';
}

export function renderTelemetryInsights(run = {}) {
    const insights = deriveTelemetryInsights(run);
    const formatDuration = (value) => {
        if (value === null) return "Unknown";
        if (value < 1000) return value + "ms";
        return Math.round(value / 1000) + "s";
    };
    const formatMeasure = (measure) => measure.value === 0 ? "0 " + measure.unit : measure.value.toLocaleString() + " " + measure.unit;
    const durationRows = [
        ["Run duration", formatDuration(insights.duration.totalMillis)],
        ["Queue wait", formatDuration(insights.duration.queueMillis)],
        ["Execution time", formatDuration(insights.duration.executionMillis)],
    ];
    const countRows = [
        ["Failures", insights.counts.failures === null ? "Unknown" : String(insights.counts.failures)],
        ["Repasses", insights.counts.repasses === null ? "Unknown" : String(insights.counts.repasses)],
    ];
    const rows = (items) => items.map(([label, value]) =>
        '<div class="kv"><div class="label">' + escapeAssociationHtml(label) + '</div><div class="value">' + escapeAssociationHtml(value) + "</div></div>",
    ).join("");
    const visibleHotspots = insights.hotspots.slice(0, 5);
    const omittedHotspots = insights.hotspots.length - visibleHotspots.length;
    const hotspotRows = insights.hotspots.length
        ? visibleHotspots.map((item) => "<li><strong>" + escapeAssociationHtml(item.stage) +
            "</strong>: " + item.failures + " failures, " + item.retries + " retries</li>").join("") +
            (omittedHotspots ? '<li class="muted">+' + omittedHotspots + " more hotspots.</li>" : "")
        : '<li class="muted">Unknown: no stage attempt telemetry.</li>';
    const usage = insights.usage.length
        ? insights.usage.map((item) => [item.label, formatMeasure(item)])
        : [["Model usage", "Unknown (no model or usage units)"]];
    if (insights.model) usage.unshift(["Model", insights.model]);
    const budgets = insights.budgets.length
        ? insights.budgets.map((item) => [item.label, formatMeasure(item)])
        : [["Budgets", "Unknown (no explicit budget units)"]];
    return '<div class="telemetry-insights" aria-label="Telemetry insights">' +
        '<div class="kv-grid">' + rows(durationRows) + rows(countRows) + rows(usage) + rows(budgets) + "</div>" +
        "<h3>Stage and retry hotspots</h3><ul class=\"causal-list\">" + hotspotRows + "</ul></div>";
}

export function renderHtml(instanceId, themePreference = "system", persistedFilters = {}) {
  const initialFilters = JSON.stringify(persistedFilters).replaceAll("<", "\\u003c");
    return `<!doctype html>
<html data-theme-preference="${themePreference}">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>Goobers Portal</title>
<script>
  (function () {
    var root = document.documentElement;
    var preference = root.dataset.themePreference || "system";
    var systemDark = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
    root.dataset.portalTheme = preference === "system" ? (systemDark ? "dark" : "light") : preference;
  })();
</script>
<style>
  :root {
    color-scheme: light dark;
  }
  :root[data-portal-theme="light"] {
    color-scheme: light;
    --background-color-default: #ffffff;
    --background-color-hover: #f6f8fa;
    --border-color-default: #d0d7de;
    --text-color-default: #1f2328;
    --text-color-muted: #656d76;
    --color-focus-outline: #0969da;
    --true-color-blue: #0969da;
    --true-color-blue-muted: #ddf4ff;
    --true-color-green: #1a7f37;
    --true-color-green-muted: #dafbe1;
    --true-color-red: #cf222e;
    --true-color-red-muted: #ffebe9;
    --true-color-yellow: #9a6700;
  }
  :root[data-portal-theme="dark"] {
    color-scheme: dark;
    --background-color-default: #0d1117;
    --background-color-hover: #161b22;
    --border-color-default: #30363d;
    --text-color-default: #f0f6fc;
    --text-color-muted: #8b949e;
    --color-focus-outline: #58a6ff;
    --true-color-blue: #58a6ff;
    --true-color-blue-muted: #1f6feb55;
    --true-color-green: #3fb950;
    --true-color-green-muted: #23863655;
    --true-color-red: #f85149;
    --true-color-red-muted: #da363355;
    --true-color-yellow: #d29922;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    background: var(--background-color-default, #ffffff);
    color: var(--text-color-default, #1f2328);
    font-family: var(--font-sans, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif);
    font-size: var(--text-body-medium, 14px);
    line-height: var(--leading-body-medium, 20px);
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border-color-default, #d0d7de);
    flex-wrap: wrap;
  }
  h1 {
    font-size: var(--text-title-medium, 18px);
    font-weight: var(--font-weight-semibold, 600);
    margin: 0;
  }
  .toolbar { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  main { padding: 16px; min-width: 0; }
  .skip-link { position: absolute; top: -100px; left: 12px; z-index: 10; }
  .skip-link:focus { top: 12px; padding: 8px; background: var(--background-color-default, #fff); }
  .source-context { display: flex; gap: 8px 16px; align-items: center; flex-wrap: wrap; margin-bottom: 12px; }
  #source-context { font-weight: 600; overflow-wrap: anywhere; }
  .section-description { margin: 0 0 12px; color: var(--text-color-muted, #656d76); }
  .table-link { padding: 0; border: 0; background: transparent; color: var(--true-color-blue, #0969da); text-align: left; }
  .table-link:hover { background: transparent; text-decoration: underline; }
  .sort-button { border: 0; padding: 0; background: transparent; color: inherit; }
  .table-scroll { max-width: 100%; overflow-x: auto; }
  .table-scroll table { white-space: nowrap; }
  .toolbar > *, .add-form > * { max-width: 100%; }
  .fleet-panel {
    display: flex;
    gap: 8px 12px;
    align-items: center;
    flex-wrap: wrap;
    padding: 10px 12px;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    margin-bottom: 12px;
  }
  .fleet-panel[hidden] { display: none; }
  #source-select { max-width: min(100%, 360px); }
  .muted { color: var(--text-color-muted, #656d76); }
  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(130px, 1fr));
    gap: 12px;
    margin-bottom: 20px;
  }
  .card {
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    padding: 12px 14px;
  }
  .card .label { color: var(--text-color-muted, #656d76); font-size: 12px; }
  .card .value { font-size: 22px; font-weight: 600; margin-top: 4px; }
  table { width: 100%; border-collapse: collapse; margin-bottom: 24px; }
  th, td {
    text-align: left;
    padding: 6px 10px;
    border-bottom: 1px solid var(--border-color-default, #d0d7de);
    font-size: 13px;
  }
  th { color: var(--text-color-muted, #656d76); font-weight: 500; }
  code {
    font-family: var(--font-mono, "SFMono-Regular", Consolas, "Liberation Mono", monospace);
    font-size: var(--text-code-inline, 12px);
    background: var(--border-color-default, #d0d7de22);
    padding: 1px 4px;
    border-radius: 4px;
  }
  button, select, input {
    font: inherit;
    padding: 6px 10px;
    border-radius: 6px;
    border: 1px solid var(--border-color-default, #d0d7de);
    background: var(--background-color-default, #fff);
    color: var(--text-color-default, #1f2328);
  }
  button { cursor: pointer; }
  button:hover { background: var(--border-color-default, #d0d7de33); }
  :focus-visible {
    outline: 2px solid var(--color-focus-outline, #0969da);
    outline-offset: 2px;
  }
  .phase {
    display: inline-block;
    padding: 1px 8px;
    border-radius: 999px;
    font-size: 12px;
    background: var(--border-color-default, #d0d7de33);
  }
  .phase[data-phase="running"] { color: var(--true-color-blue, #0969da); background: var(--true-color-blue-muted, #ddf4ff); }
  .phase[data-phase="completed"], .phase[data-phase="succeeded"] { color: var(--true-color-green, #1a7f37); background: var(--true-color-green-muted, #dafbe1); }
  .phase[data-phase="failed"], .phase[data-phase="escalated"] { color: var(--true-color-red, #cf222e); background: var(--true-color-red-muted, #ffebe9); }
  .phase[data-phase="blocked"], .phase[data-phase="awaiting-human"] { color: var(--true-color-yellow, #9a6700); }
  #error { color: var(--true-color-red, #cf222e); margin-bottom: 12px; white-space: pre-wrap; overflow-wrap: anywhere; }
  #workflow-run-status { color: var(--true-color-green, #1a7f37); margin-bottom: 12px; }
  #workflow-run-status:empty { display: none; }
  #empty-state { padding: 32px 0; }
  #empty-state ol { padding-left: 20px; }
  #needs-you { margin-bottom: 20px; }
  .configuration-warning-section {
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    padding: 12px;
    margin: 0 0 20px;
    min-width: 260px;
    white-space: normal;
  }
  .configuration-warning-heading,
  .configuration-warning-group summary,
  .configuration-warning-identity,
  .configuration-warning-group-actions {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px 12px;
    flex-wrap: wrap;
  }
  .configuration-warning-heading h2 { margin: 0; }
  .section-count,
  .configuration-warning-group-count,
  .warning-severity {
    color: var(--text-color-muted, #656d76);
    font-size: 12px;
  }
  .configuration-warning-groups { display: grid; gap: 10px; }
  .configuration-warning-group {
    margin: 0;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    overflow: hidden;
  }
  .configuration-warning-group summary {
    padding: 9px 10px;
    color: var(--text-color-default, #1f2328);
    background: var(--border-color-default, #d0d7de22);
  }
  .configuration-warning-group-content { padding: 10px; }
  .configuration-warning-group-actions { justify-content: flex-end; }
  .configuration-warning-remediation {
    margin: 0 0 10px;
    color: var(--text-color-muted, #656d76);
    font-size: 12px;
  }
  .configuration-warning-list { display: grid; gap: 8px; }
  .configuration-warning {
    border-left: 4px solid var(--true-color-yellow, #9a6700);
    border-radius: 6px;
    background: var(--border-color-default, #d0d7de22);
    padding: 9px 10px;
  }
  .configuration-warning p { margin: 6px 0; overflow-wrap: anywhere; }
  .warning-code {
    color: var(--true-color-red, #cf222e);
    background: var(--true-color-red-muted, #ffebe9);
    font-weight: var(--font-weight-semibold, 600);
  }
  .configuration-warning-dismiss {
    padding: 3px 8px;
    color: var(--text-color-muted, #656d76);
    font-size: 12px;
  }
  .configuration-warning-empty {
    display: grid;
    gap: 2px;
    color: var(--text-color-muted, #656d76);
    padding-top: 8px;
  }
  .configuration-warning-empty strong { color: var(--text-color-default, #1f2328); }
  .configuration-warning-cell { min-width: 320px; vertical-align: top; }
  .configuration-warning-cell .configuration-warning-section { margin: 0; }
  .configuration-warning-cell .configuration-warning-heading h2 { font-size: 13px; }
  .attention-list { display: grid; gap: 8px; }
  .attention-item {
    display: grid;
    grid-template-columns: minmax(110px, auto) 1fr auto;
    gap: 8px 12px;
    align-items: start;
    box-sizing: border-box;
    min-height: 84px;
    border: 1px solid var(--true-color-red-muted, #cf222e66);
    border-left: 4px solid var(--true-color-red, #cf222e);
    border-radius: 6px;
    padding: 9px 12px;
  }
  .attention-item.is-expanded { height: auto; min-height: 84px; }
  .attention-item a { color: inherit; }
  .attention-reason {
    min-width: 0;
    overflow-wrap: anywhere;
    display: -webkit-box;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
    overflow: hidden;
  }
  .attention-item.is-expanded .attention-reason {
    display: block;
    overflow: visible;
  }
  .attention-action {
    display: flex;
    align-items: center;
    gap: 6px;
    color: var(--text-color-muted, #656d76);
    font-size: 12px;
    flex-wrap: wrap;
  }
  .run-id-control { display: inline-flex; align-items: center; gap: 6px; }
  .copy-run-id {
    padding: 2px 7px;
    font-size: 12px;
    color: var(--text-color-muted, #656d76);
  }
  .copy-run-id.copied {
    color: var(--true-color-green, #1a7f37);
    border-color: var(--true-color-green-muted, #1a7f3766);
    animation: copy-pop 240ms ease-out;
  }
  .goober-chip {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    max-width: 100%;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 999px;
    padding: 2px 7px;
    background: var(--border-color-default, #d0d7de22);
    font-size: 12px;
    white-space: nowrap;
    vertical-align: middle;
  }
  .goober-avatar { line-height: 1; }
  .goober-label { overflow: hidden; text-overflow: ellipsis; }
  .freshness {
    color: var(--text-color-muted, #656d76);
    font-size: 12px;
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .freshness::before {
    content: "";
    width: 8px;
    height: 8px;
    border-radius: 999px;
    background: var(--text-color-muted, #656d76);
    opacity: 0.55;
  }
  .freshness[data-freshness="live"]::before {
    background: var(--true-color-green, #1a7f37);
    opacity: 1;
    animation: freshness-pulse 1.8s ease-in-out infinite;
  }
  .freshness[data-freshness="stale"]::before {
    background: var(--true-color-yellow, #9a6700);
    opacity: 0.65;
  }
  .freshness[data-freshness="offline"]::before {
    background: var(--text-color-muted, #656d76);
    opacity: 0.35;
  }
  @keyframes freshness-pulse {
    0%, 100% { transform: scale(0.86); box-shadow: 0 0 0 0 rgba(26,127,55,0.35); }
    50% { transform: scale(1.12); box-shadow: 0 0 0 5px rgba(26,127,55,0); }
  }
  @keyframes copy-pop {
    0% { transform: scale(0.9); }
    60% { transform: scale(1.16); }
    100% { transform: scale(1); }
  }
  @media (prefers-reduced-motion: reduce) {
    .freshness[data-freshness="live"]::before,
    .copy-run-id.copied {
      animation: none;
    }
  }
  @media (max-width: 640px) {
    .attention-item { grid-template-columns: 1fr; gap: 6px; min-height: 118px; }
    .attention-item.is-expanded { min-height: 118px; }
    main { padding: 10px; }
    table { display: block; overflow-x: auto; white-space: nowrap; }
    .toolbar { width: 100%; }
    #source-select { flex: 1; min-width: 0; }
    .waterfall-row { grid-template-columns: minmax(0, 1fr); gap: 4px; }
    .graph-toolbar { flex-wrap: wrap; }
    .graph-help { width: 100%; }
  }
  @media (max-width: 900px) {
    .stage-definition-layout { grid-template-columns: minmax(0, 1fr); }
  }
  #start-daemon-bar {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
    margin-bottom: 12px;
    padding: 8px 10px;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 6px;
  }
  #start-daemon-msg { color: var(--text-color-muted, #656d76); font-size: 13px; }
  section h2 {
    font-size: var(--text-body-large, 15px);
    margin: 0 0 8px 0;
  }
  .add-form {
    display: flex;
    gap: 6px;
    align-items: center;
    margin-bottom: 12px;
    flex-wrap: wrap;
  }
  .add-form input { flex: 1; min-width: 160px; }
  dialog {
    width: min(680px, calc(100vw - 32px));
    max-height: calc(100vh - 48px);
    color: var(--text-color-default, #1f2328);
    background: var(--background-color-default, #fff);
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 10px;
    padding: 0;
  }
  dialog::backdrop { background: #0008; }
  .directory-dialog-header,
  .directory-dialog-footer {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 12px;
  }
  .directory-dialog-header { border-bottom: 1px solid var(--border-color-default, #d0d7de); }
  .directory-dialog-footer {
    border-top: 1px solid var(--border-color-default, #d0d7de);
    justify-content: flex-end;
  }
  #directory-current { flex: 1; min-width: 0; }
  #directory-list {
    min-height: 220px;
    max-height: 50vh;
    overflow: auto;
    padding: 8px;
  }
  .directory-entry {
    display: block;
    width: 100%;
    border: 0;
    border-radius: 4px;
    text-align: left;
    padding: 7px 9px;
  }
  details { margin-bottom: 16px; }
  summary { cursor: pointer; color: var(--text-color-muted, #656d76); font-size: 13px; }
  #run-view { display: none; }
  #run-view .back { margin-bottom: 12px; }
  .run-header { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; margin-bottom: 4px; }
  .run-header h2 { margin: 0; font-size: var(--text-title-medium, 18px); }
  .actions-run-link {
    display: inline-flex;
    align-items: center;
    padding: 4px 9px;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 6px;
    color: var(--true-color-blue, #0969da);
    text-decoration: none;
  }
  .actions-run-link:hover { background: var(--background-color-hover, #f6f8fa); }
  .run-associations { display: flex; flex-direction: column; align-items: flex-start; gap: 4px; min-width: 180px; }
  .run-association-link {
    color: inherit;
    max-width: 320px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    text-decoration: none;
  }
  .run-association-link:hover { text-decoration: underline; }
  .work-chip {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    max-width: 320px;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 999px;
    padding: 3px 8px;
    background: var(--background-color-default, #fff);
  }
  .work-chip-kind {
    color: var(--true-color-blue, #0969da);
    font-size: 10px;
    font-weight: var(--font-weight-semibold, 600);
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .work-chip-label {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .work-chip-status {
    color: var(--text-color-muted, #656d76);
    font-size: 10px;
    border-left: 1px solid var(--border-color-default, #d0d7de);
    padding-left: 5px;
    text-transform: lowercase;
  }
  .work-chip[data-status="open"], .work-chip[data-status="running"] {
    border-color: var(--true-color-green-muted, #1a7f3766);
  }
  .work-chip[data-status="closed"], .work-chip[data-status="merged"], .work-chip[data-status="completed"] {
    border-color: var(--true-color-purple-muted, #8250df66);
  }
  .kv-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 10px; margin: 12px 0 20px; }
  .kv { border: 1px solid var(--border-color-default, #d0d7de); border-radius: 8px; padding: 10px 12px; }
  .kv-wide { grid-column: 1 / -1; }
  .kv .label { color: var(--text-color-muted, #656d76); font-size: 12px; }
  .kv .value { font-size: 14px; margin-top: 2px; word-break: break-word; }
  .kv .value a { color: inherit; }
  .stage-definition-layout {
    display: grid;
    grid-template-columns: minmax(0, 2fr) minmax(280px, 1fr);
    gap: 12px;
    align-items: start;
  }
  .stage-definition-panel,
  .stage-inspector-state {
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    padding: 12px;
    background: var(--background-color-default, #fff);
    min-width: 0;
  }
  .stage-inspector-state { color: var(--text-color-muted, #656d76); }
  .stage-inspector-error { border-color: var(--true-color-red-muted, #cf222e66); color: var(--true-color-red, #cf222e); }
  .stage-inspector-heading { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
  .stage-inspector-heading h3 { margin: 0; font-size: var(--text-title-medium, 18px); }
  .stage-kind-badge {
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 999px;
    padding: 2px 7px;
    color: var(--text-color-muted, #656d76);
    font-size: 11px;
  }
  .stage-kind-badge[data-kind="gate"] { border-color: var(--true-color-purple-muted, #8250df66); }
  .stage-kind-badge[data-kind="agentic"] { border-color: var(--true-color-blue-muted, #0969da66); }
  .stage-kind-badge[data-kind="deterministic"] { border-color: var(--true-color-green-muted, #1a7f3766); }
  .stage-inspector-description { color: var(--text-color-muted, #656d76); overflow-wrap: anywhere; }
  .property-list { display: grid; gap: 0; margin: 0; }
  .property-list > div { padding: 8px 0; border-bottom: 1px solid var(--border-color-default, #d0d7de); }
  .property-list > div:last-child { border-bottom: 0; }
  .property-list dt { color: var(--text-color-muted, #656d76); font-size: 12px; }
  .property-list dd { margin: 2px 0 0; overflow-wrap: anywhere; }
  .code-block {
    max-height: 520px;
    margin: 0;
    overflow: auto;
    white-space: pre;
    font-size: 12px;
  }
  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    padding: 0;
    margin: -1px;
    overflow: hidden;
    clip: rect(0, 0, 0, 0);
    white-space: nowrap;
    border: 0;
  }
  .graph-panel {
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    overflow: hidden;
    background: var(--background-color-default, #fff);
  }
  .graph-toolbar {
    min-height: 38px;
    padding: 5px 8px;
    border-bottom: 1px solid var(--border-color-default, #d0d7de);
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .graph-toolbar button { min-width: 30px; padding: 3px 8px; }
  .graph-zoom-value { min-width: 48px; text-align: center; font-variant-numeric: tabular-nums; }
  .graph-help { margin-left: auto; font-size: 12px; }
  #graph-svg {
    display: block;
    width: 100%;
    height: clamp(300px, 55vh, 600px);
    background: var(--background-color-default, #fff);
    cursor: grab;
    touch-action: none;
    user-select: none;
  }
  #graph-svg.is-panning { cursor: grabbing; }
  .stage-node { cursor: pointer; outline: none; }
  .stage-node:hover .node-rect,
  .stage-node:focus .node-rect,
  .stage-node.selected .node-rect {
    stroke: var(--true-color-blue, #0969da);
    stroke-width: 3;
  }
  @media (forced-colors: active) {
    .stage-node:focus .node-rect { stroke: CanvasText; }
  }
  .node-rect { fill: var(--border-color-default, #d0d7de33); stroke: var(--border-color-default, #d0d7de); }
  .node-rect.visited { fill: var(--true-color-blue-muted, #ddf4ff); stroke: var(--true-color-blue, #0969da); }
  .node-rect.pending { fill: var(--background-color-default, #ffffff); stroke: var(--border-color-default, #d0d7de); }
  .node-rect.completed, .node-rect.succeeded { fill: var(--true-color-green-muted, #dafbe1); stroke: var(--true-color-green, #1a7f37); }
  .node-rect.running { fill: var(--true-color-blue-muted, #ddf4ff); stroke: var(--true-color-blue, #0969da); stroke-width: 2; }
  .node-rect.failed { fill: var(--true-color-red-muted, #ffebe9); stroke: var(--true-color-red, #cf222e); }
  .node-rect.blocked { fill: var(--true-color-yellow, #9a670033); stroke: var(--true-color-yellow, #9a6700); }
  .node-rect.skipped { fill: var(--border-color-default, #d0d7de22); stroke: var(--text-color-muted, #656d76); }
  .node-rect.terminal { stroke-width: 3; }
  .node-label { font-size: 10px; fill: var(--text-color-default, #1f2328); }
  .graph-legend { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-top: 10px; color: var(--text-color-muted, #656d76); font-size: 12px; }
  .legend-chip { display: inline-flex; align-items: center; gap: 6px; border: 1px solid var(--border-color-default, #d0d7de); border-radius: 999px; padding: 3px 8px; }
  .legend-swatch { width: 10px; height: 10px; border-radius: 3px; display: inline-block; background: var(--border-color-default, #d0d7de); }
  .legend-chip.pending .legend-swatch { background: var(--background-color-default, #fff); border: 1px solid var(--border-color-default, #d0d7de); }
  .legend-chip.running .legend-swatch { background: var(--true-color-blue-muted, #ddf4ff); border: 1px solid var(--true-color-blue, #0969da); }
  .legend-chip.succeeded .legend-swatch { background: var(--true-color-green-muted, #dafbe1); border: 1px solid var(--true-color-green, #1a7f37); }
  .legend-chip.failed .legend-swatch { background: var(--true-color-red-muted, #ffebe9); border: 1px solid var(--true-color-red, #cf222e); }
  .legend-chip.skipped .legend-swatch { background: var(--border-color-default, #d0d7de22); border: 1px solid var(--text-color-muted, #656d76); }
  .legend-chip.blocked .legend-swatch { background: var(--true-color-yellow, #9a670033); border: 1px solid var(--true-color-yellow, #9a6700); }
  .causal-diagnosis { margin: 12px 0 20px; }
  .causal-list { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
  .execution-waterfall { display: grid; gap: 8px; margin-top: 8px; }
  .waterfall-row { display: grid; grid-template-columns: minmax(140px, 210px) minmax(140px, 1fr) minmax(70px, 110px); gap: 10px; align-items: center; }
  .waterfall-stage { font-size: 12px; }
  .waterfall-bar-wrap { position: relative; height: 14px; border: 1px solid var(--border-color-default, #d0d7de); border-radius: 999px; background: var(--background-color-hover, #f6f8fa); overflow: hidden; }
  .waterfall-bar { position: absolute; top: 1px; bottom: 1px; left: 0; border-radius: 999px; }
  .waterfall-bar.pending { background: var(--border-color-default, #d0d7de); }
  .waterfall-bar.running { background: var(--true-color-blue-muted, #ddf4ff); border: 1px solid var(--true-color-blue, #0969da); }
  .waterfall-bar.succeeded { background: var(--true-color-green-muted, #dafbe1); border: 1px solid var(--true-color-green, #1a7f37); }
  .waterfall-bar.failed { background: var(--true-color-red-muted, #ffebe9); border: 1px solid var(--true-color-red, #cf222e); }
  .waterfall-bar.blocked { background: var(--true-color-yellow, #9a670033); border: 1px solid var(--true-color-yellow, #9a6700); }
  .waterfall-bar.skipped { background: var(--border-color-default, #d0d7de22); border: 1px solid var(--text-color-muted, #656d76); }
  .waterfall-bar.retry { box-shadow: 0 0 0 2px var(--true-color-yellow, #9a6700) inset; }
  .unknown-timing .waterfall-bar { opacity: 0.45; }
  .waterfall-status { font-size: 12px; color: var(--text-color-muted, #656d76); text-transform: capitalize; }
  .node-status { font-size: 8px; fill: var(--text-color-muted, #656d76); }
  .edge-traversed { stroke: var(--true-color-blue, #0969da); stroke-width: 2; fill: none; }
  .edge { stroke: var(--border-color-default, #d0d7de); stroke-width: 1; fill: none; }
  .edge-label { font-size: 8px; fill: var(--text-color-muted, #656d76); paint-order: stroke; stroke: var(--background-color-default, #fff); stroke-width: 3px; }
  .badge-pass { color: var(--true-color-green, #1a7f37); }
  .badge-fail, .badge-escalate { color: var(--true-color-red, #cf222e); }
  .badge-needs-changes { color: var(--true-color-yellow, #9a6700); }
  .transitions-list { list-style: none; margin: 0; padding: 0; max-height: 320px; overflow-y: auto; border: 1px solid var(--border-color-default, #d0d7de); border-radius: 8px; }
  .transitions-list li { padding: 6px 12px; border-bottom: 1px solid var(--border-color-default, #d0d7de); font-size: 13px; display: flex; gap: 8px; align-items: baseline; flex-wrap: wrap; }
  .transitions-list li:last-child { border-bottom: none; }
  .transitions-list .seq { color: var(--text-color-muted, #656d76); font-size: 11px; min-width: 34px; }
  .clickable-row { cursor: pointer; }
  .clickable-row:hover { background: var(--border-color-default, #d0d7de22); }
  .rationale { white-space: pre-wrap; font-size: 13px; }
  .pr-description {
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    padding: 12px;
    font-family: var(--font-sans, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif);
    font-size: 13px;
  }
  .blockers-list { margin: 4px 0 0; padding-left: 18px; }
  .external-refs { display: flex; flex-wrap: wrap; gap: 6px; margin: 8px 0 16px; }
  .external-refs a { font-size: 12px; border: 1px solid var(--border-color-default, #d0d7de); border-radius: 999px; padding: 3px 9px; color: inherit; text-decoration: none; }
  .external-refs a:hover { text-decoration: underline; }
  .event-list { border: 1px solid var(--border-color-default, #d0d7de); border-radius: 8px; max-height: 560px; overflow-y: auto; }
  .event-list details { margin: 0; border-bottom: 1px solid var(--border-color-default, #d0d7de); }
  .event-list details:last-child { border-bottom: none; }
  .event-list summary { padding: 7px 10px; display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
  .event-list .event-body { padding: 0 12px 10px 46px; font-size: 12px; }
  .event-list .event-body pre { max-height: 280px; overflow: auto; white-space: pre-wrap; word-break: break-word; background: var(--border-color-default, #d0d7de22); padding: 8px; border-radius: 6px; }
  .event-seq { color: var(--text-color-muted, #656d76); min-width: 34px; }
  .event-time { color: var(--text-color-muted, #656d76); margin-left: auto; }
  .artifact-links { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 6px; }
  .artifact-links a { color: inherit; }
  .filters-bar { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; margin-bottom: 10px; }
  .filters-bar select, .filters-bar input { font-size: 12px; padding: 4px 8px; }
  .native-multi-filter { display: none; }
  .multi-filter { position: relative; min-width: 154px; }
  .multi-filter-button {
    width: 100%;
    min-height: 32px;
    display: inline-flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    background: var(--background-color-default, #ffffff);
    color: var(--text-color-default, #1f2328);
    font-size: 12px;
    padding: 5px 9px;
    cursor: pointer;
  }
  .multi-filter-button::after { content: "▾"; color: var(--text-color-muted, #656d76); }
  .multi-filter-button:disabled { cursor: not-allowed; opacity: 0.62; }
  .multi-filter-menu {
    position: absolute;
    z-index: 30;
    top: calc(100% + 4px);
    left: 0;
    min-width: 100%;
    max-width: min(320px, calc(100vw - 32px));
    max-height: 260px;
    overflow: auto;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    background: var(--background-color-default, #ffffff);
    box-shadow: 0 8px 24px rgba(0,0,0,0.16);
    padding: 6px;
  }
  .multi-filter-option {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 6px 8px;
    border-radius: 6px;
    font-size: 12px;
    white-space: nowrap;
    cursor: pointer;
  }
  .multi-filter-option:hover { background: var(--background-color-hover, rgba(130,130,130,0.12)); }
  .multi-filter-empty { padding: 6px 8px; font-size: 12px; color: var(--text-color-muted, #656d76); }
  .advanced-filters { flex-basis: 100%; margin: 0; }
  .advanced-filters summary { padding: 8px 0; }
  th[data-sort] { cursor: pointer; user-select: none; }
  th[data-sort]:hover { color: var(--text-color-default, #1f2328); }
  th[data-sort] .sort-arrow { font-size: 10px; margin-left: 4px; color: var(--text-color-muted, #656d76); }
  #workflows-table tbody tr.clickable-row { cursor: pointer; }
  #workflows-table tbody tr.clickable-row:hover { background: var(--background-color-hover, rgba(130,130,130,0.12)); }
  .enabled-cell .enabled-wrap {
    display: inline-flex;
    align-items: center;
    gap: 8px;
  }
  .workflow-state {
    font-size: 11px;
    padding: 3px 10px;
    border-radius: 999px;
    border: 1px solid var(--border-color-default, #d0d7de);
    /* Fixed box so "Enabled" / "Disabled" / "Saving…" don't shove the
       adjacent button to a different x-offset on every row. */
    box-sizing: border-box;
    min-width: 84px;
    text-align: center;
  }
  .workflow-state.is-enabled {
    color: var(--true-color-green, #1a7f37);
    border-color: var(--true-color-green-muted, #1a7f3766);
  }
  .workflow-state.is-disabled {
    color: var(--text-color-muted, #656d76);
  }
  .workflow-state.is-pending {
    color: var(--text-color-muted, #656d76);
    font-style: italic;
  }
  .workflow-toggle {
    /* Square, centered box: the play and stop glyphs have different advance
       widths and would otherwise render at different sizes and offsets. */
    box-sizing: border-box;
    width: 26px;
    height: 22px;
    padding: 0;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 12px;
    line-height: 1;
    border-radius: 6px;
    border: 1px solid var(--border-color-default, #d0d7de);
  }
  /* Emoji supply their own color, so the state tint lives on the border. */
  .workflow-toggle.is-stop {
    border-color: var(--true-color-red-muted, #cf222e66);
  }
  .workflow-toggle.is-start {
    border-color: var(--true-color-green-muted, #1a7f3766);
  }
  .workflow-toggle[disabled] {
    opacity: 0.55;
  }
  .workflow-run-now {
    /* Reuse the toggle's square icon-button box so the two controls align. */
    box-sizing: border-box;
    width: 26px;
    height: 22px;
    padding: 0;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 12px;
    line-height: 1;
    border-radius: 6px;
    border: 1px solid var(--true-color-blue-muted, #0969da66);
  }
  .workflow-run-now[disabled] {
    opacity: 0.6;
  }
  .run-actions { border: 1px solid var(--border-color-default, #d0d7de); padding: 12px; border-radius: 6px; }
  .run-actions-grid { display: flex; gap: 8px; flex-wrap: wrap; align-items: end; }
  .run-actions label { display: flex; flex-direction: column; gap: 4px; font-size: 12px; }
  .run-action-confirmation { margin-top: 10px; }
  .internal-tabs {
    display: flex;
    gap: 4px;
    margin: 12px 0;
    border-bottom: 1px solid var(--border-color-default, #d0d7de);
    overflow-x: auto;
  }
  .internal-tabs [role="tab"] {
    flex: 0 0 auto;
    border: 0;
    border-bottom: 2px solid transparent;
    border-radius: 6px 6px 0 0;
    background: transparent;
    padding: 8px 12px;
  }
  .internal-tabs [role="tab"][aria-selected="true"] {
    border-bottom-color: var(--true-color-blue, #0969da);
    color: var(--true-color-blue, #0969da);
    font-weight: 600;
  }
  [role="tabpanel"] { min-width: 0; }
  [role="tabpanel"][hidden] { display: none !important; }
</style>
</head>
<body>
<a class="skip-link" href="#main-content">Skip to content</a>
<header>
  <h1>Goobers Portal</h1>
  <div class="toolbar">
    <select id="theme-select" aria-label="Color theme" title="Color theme">
      <option value="system">System theme</option>
      <option value="light">Light theme</option>
      <option value="dark">Dark theme</option>
    </select>
    <select id="source-select" aria-label="Goobers source"><option value="">No sources yet</option></select>
    <input id="run-jump" type="text" placeholder="Run ID" aria-label="Jump to a run" style="max-width: 180px;" />
    <button id="run-jump-button" type="button">Jump</button>
    <button id="refresh">Refresh</button>
  </div>
</header>
<main id="main-content" tabindex="-1">
  <p id="source-context" class="muted"></p>
  <p id="freshness" class="freshness" role="status">Connecting</p>
  <details id="add-source-details">
    <summary>Connect a source&hellip;</summary>
    <div class="add-form">
      <input id="local-root" aria-label="Local instance root" placeholder="Local instance root path (e.g. C:\\\\path\\\\to\\\\instance)" />
      <button id="browse-local" title="Browse folders">&#128193; Browse</button>
      <button id="add-local">Add local</button>
    </div>
    <div class="add-form">
      <input id="remote-url" aria-label="Remote control-plane URL" placeholder="Remote control-plane URL (e.g. http://10.0.0.5:8080)" />
      <input id="remote-token" placeholder="Bearer token (optional)" style="flex: 0 0 200px" />
      <button id="add-remote">Add remote</button>
    </div>
    <div class="add-form">
      <input id="github-workflow-url" aria-label="GitHub Actions workflow URL" placeholder="GitHub Actions workflow URL (https://github.com/owner/repo/actions/workflows/file.yml)" />
      <button id="add-github">Connect to GitHub</button>
    </div>
  </details>
  <div id="fleet-panel" class="fleet-panel" hidden></div>
  <dialog id="directory-dialog">
    <div class="directory-dialog-header">
      <button id="directory-parent" title="Parent directory">&larr;</button>
      <select id="directory-roots" aria-label="Drive or filesystem root"></select>
      <input id="directory-current" readonly aria-label="Current directory" />
    </div>
    <div id="directory-list" aria-label="Folders"></div>
    <div class="directory-dialog-footer">
      <button id="directory-cancel">Cancel</button>
      <button id="directory-choose">Choose this folder</button>
    </div>
  </dialog>
  <div id="error" role="alert"></div>
  <div id="workflow-run-status" role="status"></div>
  <div id="start-daemon-bar" style="display:none">
    <span id="start-daemon-msg"></span>
    <button id="start-daemon">Start daemon</button>
  </div>
  <div id="empty-state" class="muted" style="display:none">
    <p>No Goobers source connected yet. The portal only shows real instances or persisted Actions journals.</p>
    <ol>
      <li>To see a <strong>local</strong> instance, run <code>goobers up</code> against an existing instance root, then add that root's path above.</li>
      <li>To see a <strong>remote</strong> control plane, add its base URL above.</li>
      <li>To inspect <strong>GitHub Actions</strong> history, connect a workflow URL. Uploaded Goobers journals are downloaded through your authenticated <code>gh</code> session.</li>
    </ol>
  </div>
  <div id="dashboard" style="display:none">
    <div class="internal-tabs" role="tablist" aria-label="Dashboard sections">
      <button id="dashboard-tab-attention" role="tab" data-tab="attention" aria-controls="dashboard-panel-attention">Overview</button>
      <button id="dashboard-tab-workflows" role="tab" data-tab="workflows" aria-controls="dashboard-panel-workflows">Workflows</button>
      <button id="dashboard-tab-runs" role="tab" data-tab="runs" aria-controls="dashboard-panel-runs">Runs</button>
    </div>
    <section id="dashboard-panel-attention" role="tabpanel" aria-labelledby="dashboard-tab-attention">
      <div id="needs-you" aria-labelledby="needs-you-heading">
        <h2 id="needs-you-heading">Needs attention</h2>
        <p class="section-description">Review blockers and decisions before exploring workflow activity.</p>
        <div id="attention-list"></div>
      </div>
      <div id="instance-configuration-warnings" data-warning-context="instance"></div>
      <h2>Activity at a glance</h2>
      <div class="cards" id="cards"></div>
    </section>
    <section id="dashboard-panel-workflows" role="tabpanel" aria-labelledby="dashboard-tab-workflows" hidden>
      <h2>Workflows</h2>
      <p class="section-description">Select a workflow to explore its runs. Trigger controls affect future automatic runs only.</p>
      <div class="table-scroll" role="region" aria-label="Workflows" tabindex="0">
      <table id="workflows-table">
        <thead>
          <tr><th>Workflow</th><th>Gaggle</th><th>Trigger</th><th>In flight</th><th>Max</th><th>Run</th><th>Enabled</th><th>Warnings</th></tr>
        </thead>
        <tbody></tbody>
      </table>
      </div>
    </section>
    <section id="dashboard-panel-runs" role="tabpanel" aria-labelledby="dashboard-tab-runs" hidden>
      <h2>Runs</h2>
      <div class="filters-bar" id="runs-filters">
        <select id="filter-gaggle" class="native-multi-filter" multiple data-all-label="All gaggles" aria-label="Gaggles" title="Select one or more gaggles"><option value="">All gaggles</option></select>
        <select id="filter-workflow" class="native-multi-filter" multiple data-all-label="All workflows" aria-label="Workflows" title="Select one or more workflows"><option value="">All workflows</option></select>
        <select id="filter-phase" class="native-multi-filter" multiple data-all-label="All phases" aria-label="Phases" title="Select one or more phases">
          <option value="running">running</option>
          <option value="completed">completed</option>
          <option value="failed">failed</option>
          <option value="aborted">aborted</option>
          <option value="escalated">escalated</option>
        </select>
        <select id="filter-trigger" class="native-multi-filter" multiple data-all-label="All triggers" aria-label="Triggers" title="Select one or more triggers">
          <option value="manual">manual</option>
          <option value="schedule">schedule</option>
          <option value="item">item</option>
          <option value="webhook">webhook</option>
        </select>
        <label class="filter-toggle"><input id="filter-show-no-work" type="checkbox" /> Show no-work</label>
        <button id="filters-clear" type="button">Reset</button>
        <details class="advanced-filters">
          <summary>More filters and saved views</summary>
          <div class="filters-bar">
        <input id="filter-stage" type="text" placeholder="Stage" title="Stage name (daemon sources)" />
        <select id="filter-outcome" class="native-multi-filter" multiple data-all-label="All outcomes" aria-label="Outcomes" title="Select one or more outcomes">
          <option value="finished">finished</option>
          <option value="terminal">terminal</option>
          <option value="success">success</option>
          <option value="failure">failure</option>
          <option value="other">other</option>
        </select>
        <select id="filter-population" class="native-multi-filter" multiple data-all-label="All populations" aria-label="Populations" title="Select one or more populations">
          <option value="attempts">attempts</option>
          <option value="measured">measured</option>
          <option value="token-measured">token measured</option>
          <option value="premium-measured">premium measured</option>
          <option value="cost-measured">cost measured</option>
          <option value="retry-waste">retry waste</option>
        </select>
        <select id="saved-filter-presets" aria-label="Saved filters">
          <option value="">Saved filters</option>
        </select>
        <button id="save-filter-preset" type="button">Save</button>
        <input id="filter-since" type="datetime-local" title="Since" />
        <input id="filter-until" type="datetime-local" title="Until" />
          </div>
        </details>
      </div>
      <div class="table-scroll" role="region" aria-label="Runs" tabindex="0">
      <table id="runs-table">
        <thead>
          <tr>
            <th data-sort="id">Run</th>
            <th data-sort="workflow">Workflow</th>
            <th data-sort="gaggle">Gaggle</th>
            <th data-sort="trigger">Trigger</th>
            <th data-sort="phase">Phase</th>
            <th>Associated work</th>
            <th data-sort="startedAt">Started</th>
            <th data-sort="lastActivityAt">Last activity</th>
          </tr>
        </thead>
        <tbody></tbody>
      </table>
      </div>
      <div style="margin-top: 10px; display: flex; justify-content: flex-end;">
        <button id="runs-load-more" type="button" style="display:none">Load more</button>
      </div>
    </section>
  </div>
  <div id="run-view">
    <button class="back" id="run-back">&larr; Back to runs</button>
    <div id="run-error" role="alert" style="color: var(--true-color-red, #cf222e);"></div>
    <div id="run-status" aria-live="polite"></div>
    <div id="run-content"></div>
  </div>
</main>
<script>
(function () {
  const errorElRaw = document.getElementById("error");
  // Wrap #error so transient failure messages (e.g. "run now" rejections)
  // survive for a minimum window even though the live SSE stream can trigger
  // loadSnapshot()/renderSnapshot() at any moment, which otherwise blanks the
  // element out from under the user before they can read it. Stickiness is
  // scoped to the source it was raised for: switching sources always clears
  // it immediately rather than leaving a stale error visible for instance A
  // while instance B is now selected.
  let errorStickyUntil = 0;
  let errorStickySourceId = null;
  const ERROR_STICKY_MS = 8000;
  const errorEl = {
    get textContent() {
      return errorElRaw.textContent;
    },
    set textContent(value) {
      if (value === "") {
        const sourceChanged = errorStickySourceId !== null && errorStickySourceId !== sourceSelect.value;
        if (sourceChanged || Date.now() >= errorStickyUntil) {
          errorElRaw.textContent = "";
          errorStickySourceId = null;
        }
        return;
      }
      errorElRaw.textContent = value;
      errorStickyUntil = Date.now() + ERROR_STICKY_MS;
      errorStickySourceId = sourceSelect.value;
    },
  };
  // Distinct ID from the run-detail drilldown's #run-status live region
  // (used for approve/reject/retry action feedback) to avoid getElementById
  // colliding with that pre-existing element.
  const workflowRunStatusElRaw = document.getElementById("workflow-run-status");
  let runStatusClearTimer = null;
  const RUN_STATUS_DISPLAY_MS = 8000;
  function setRunStatus(message) {
    if (runStatusClearTimer) {
      clearTimeout(runStatusClearTimer);
      runStatusClearTimer = null;
    }
    workflowRunStatusElRaw.textContent = message;
    if (message) {
      runStatusClearTimer = window.setTimeout(() => {
        workflowRunStatusElRaw.textContent = "";
        runStatusClearTimer = null;
      }, RUN_STATUS_DISPLAY_MS);
    }
  }
  const emptyEl = document.getElementById("empty-state");
  const dashboardEl = document.getElementById("dashboard");
  const cardsEl = document.getElementById("cards");
  const attentionListEl = document.getElementById("attention-list");
  const instanceWarningsEl = document.getElementById("instance-configuration-warnings");
  const fleetPanelEl = document.getElementById("fleet-panel");
  const freshnessEl = document.getElementById("freshness");
  const workflowsBody = document.querySelector("#workflows-table tbody");
  const runsBody = document.querySelector("#runs-table tbody");
  const sourceSelect = document.getElementById("source-select");
  const runJumpInput = document.getElementById("run-jump");
  const runJumpButton = document.getElementById("run-jump-button");
  const themeSelect = document.getElementById("theme-select");
  const directoryDialog = document.getElementById("directory-dialog");
  const directoryCurrent = document.getElementById("directory-current");
  const directoryList = document.getElementById("directory-list");
  const directoryRoots = document.getElementById("directory-roots");
  const directoryParent = document.getElementById("directory-parent");
  const systemTheme = window.matchMedia("(prefers-color-scheme: dark)");
  let lastCapabilities = {};
  let lastUpdatedAt = null;
  let eventSource = null;
  let reconnectAttemptCount = 0;
  let reconnectTimer = null;
  let freshnessTimer = null;
  let liveConnectionEstablished = false;
  const dismissedAttention = new Map();
  const dismissedConfigurationWarnings = new Set();
  const expandedAttention = new Set();
  let activeDashboardTab = "attention";
  let activeRunTab = "summary";
  let selectedRunId = "";
  let runRequestSequence = 0;
  let snapshotRequestSequence = 0;
  let snapshotSourceId = null;
  let lastSnapshot = null;
  let sourceSelectionEpoch = 0;
  let stageInspectorRequestSequence = 0;
  let selectedStageName = "";
  let activeStageInspectorView = "fields";
  let restoredRunId = new URLSearchParams(window.location.search).get("run") || "";
  // gaggle/workflow -> desired enabled state, for toggles the daemon hasn't
  // confirmed yet. Kept outside the render pass so the "Saving…" label survives
  // background-poll re-renders.
  const pendingToggles = new Map();
  const pendingWorkflowRuns = new Set();
  const workflowRunRequests = new Map();
  const workflowUndo = new Map();
  const pendingRunActions = new Map();
  const workflowDetailCache = new Map();

  function portalRequestError(err) {
    const message = String(err && err.message ? err.message : err);
    if (message === "Failed to fetch" || message.includes("NetworkError")) {
      return "Portal extension connection was lost. Close and reopen this canvas to reconnect.";
    }
    return message;
  }

  function internalTabsFor(root) {
    return [...root.querySelectorAll('.internal-tabs [role="tab"][data-tab]')]
      .filter((tab) => tab.closest(".internal-tabs")?.parentElement === root);
  }

  function activateInternalTab(root, name, focus = false) {
    const tabs = internalTabsFor(root);
    const selected = tabs.find((tab) => tab.dataset.tab === name) || tabs[0];
    if (!selected) return;
    for (const tab of tabs) {
      const active = tab === selected;
      tab.setAttribute("aria-selected", String(active));
      tab.tabIndex = active ? 0 : -1;
      const panel = root.querySelector("#" + tab.getAttribute("aria-controls"));
      if (panel) panel.hidden = !active;
    }
    if (root === dashboardEl) activeDashboardTab = selected.dataset.tab;
    else if (root === runContentEl) activeRunTab = selected.dataset.tab;
    else if (root.matches(".stage-definition-panel")) activeStageInspectorView = selected.dataset.tab;
    if (focus) selected.focus();
  }

  function initInternalTabs(root, initial) {
    const tabs = internalTabsFor(root);
    tabs.forEach((tab, index) => {
      tab.addEventListener("click", () => activateInternalTab(root, tab.dataset.tab));
      tab.addEventListener("keydown", (event) => {
        let next = index;
        if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
        else if (event.key === "ArrowLeft") next = (index - 1 + tabs.length) % tabs.length;
        else if (event.key === "Home") next = 0;
        else if (event.key === "End") next = tabs.length - 1;
        else return;
        event.preventDefault();
        activateInternalTab(root, tabs[next].dataset.tab, true);
      });
    });
    activateInternalTab(root, initial);
  }

  initInternalTabs(dashboardEl, activeDashboardTab);
  document.getElementById("remote-token").type = "password";
  document.getElementById("remote-token").setAttribute("aria-label", "Remote API bearer token");
  document.querySelectorAll("#runs-filters select[title], #runs-filters input[title]").forEach((input) =>
    input.setAttribute("aria-label", input.title));

  function applyThemePreference(preference) {
    const normalized = ["system", "light", "dark"].includes(preference) ? preference : "system";
    const resolved = normalized === "system" ? (systemTheme.matches ? "dark" : "light") : normalized;
    document.documentElement.dataset.themePreference = normalized;
    document.documentElement.dataset.portalTheme = resolved;
    themeSelect.value = normalized;
  }

  applyThemePreference(document.documentElement.dataset.themePreference || "system");
  systemTheme.addEventListener("change", () => {
    if (themeSelect.value === "system") applyThemePreference("system");
  });
  themeSelect.addEventListener("change", async () => {
    const previous = document.documentElement.dataset.themePreference || "system";
    const preference = themeSelect.value;
    applyThemePreference(preference);
    try {
      const response = await fetch("/api/preferences", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ theme: preference }),
      });
      if (!response.ok) throw new Error("preference update failed");
    } catch (err) {
      applyThemePreference(previous);
      errorEl.textContent = "Could not save theme preference: " + (err.message || err);
    }
  });

  function workflowEnabledState(snapshot, gaggle, name) {
    const w = (snapshot.workflows || []).find(
      (x) => (x.identity?.gaggle || x.gaggle) === gaggle && x.identity?.name === name,
    );
    if (!w) return null;
    const nonManual = (w.triggers || []).filter((t) => (t.type || t.kind) !== "manual");
    if (nonManual.length === 0) return null;
    return nonManual.some((t) => t.enabled !== false);
  }

  // Apply a toggle and hold the pending state until the daemon's own read model
  // reports the new value. A 2xx from the PUT only means the write landed — the
  // config reload that makes it visible is asynchronous, so returning early
  // would flash a stale label and let the next poll appear to revert it.
  async function toggleWorkflow(gaggle, name, desired) {
    const sourceId = sourceSelect.value;
    const key = gaggle + "/" + name;
    // renderSnapshot() clears #error, so failures must be re-applied after the
    // final re-render rather than set before it.
    let failure = "";
    pendingToggles.set(key, desired);
    await loadSnapshot();
    try {
      if (sourceId !== sourceSelect.value) return;
      const res = await fetch("/api/set-workflow-enabled", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ source: sourceId, gaggle, workflow: name, enabled: desired }),
      });
      const data = await res.json();
      if (!data.ok) {
        failure = "Failed to update " + name + ": " + (data.reason || "unknown error");
      } else {
        const deadline = Date.now() + 30000;
        let confirmed = false;
        while (Date.now() < deadline) {
          if (sourceId !== sourceSelect.value) return;
          const snap = await fetchSnapshot();
          if (snap && snap.connected && workflowEnabledState(snap, gaggle, name) === desired) {
            confirmed = true;
            break;
          }
          await new Promise((r) => setTimeout(r, 1000));
        }
        if (!confirmed) {
          failure =
            "Saved " + name + ", but the daemon still reports the old state after 30s \u2014 it may not have reloaded.";
        } else {
          workflowUndo.set(key, { enabled: !desired });
        }
      }
    } catch (err) {
      failure = "Failed to update " + name + ": " + (err.message || err);
    } finally {
      if (sourceId === sourceSelect.value) {
        pendingToggles.delete(key);
        await loadSnapshot();
        if (failure) errorEl.textContent = failure;
      }
    }
  }

  function runNowNeedsForce(data) {
    const code = String(data?.code || "").toLowerCase();
    const reason = String(data?.reason || "").toLowerCase();
    return code === "trigger_rejected" &&
      (reason.includes("conditions: budget") || reason.includes("conditions: daily-budget"));
  }

  async function runWorkflowNow(gaggle, name) {
    const sourceId = sourceSelect.value;
    const sourceEpoch = sourceSelectionEpoch;
    const key = gaggle + "/" + name;
    const requestToken = Symbol();
    const isCurrent = () => sourceId === sourceSelect.value &&
      sourceEpoch === sourceSelectionEpoch && workflowRunRequests.get(key) === requestToken;
    workflowRunRequests.set(key, requestToken);
    let failure = "";
    pendingWorkflowRuns.add(key);
    try {
      await loadSnapshot();
      let force = false;
      while (isCurrent()) {
        const res = await fetch("/api/run-workflow-now", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ source: sourceId, gaggle, workflow: name, force }),
        });
        const data = await res.json();
        if (!isCurrent()) return;
        if (!data.ok) {
          if (!force && runNowNeedsForce(data)) {
            const proceed = window.confirm(
              name + " has already spent its cadence budget. Run it now anyway with --force?",
            );
            if (!isCurrent()) return;
            if (proceed) {
              // The force retry belongs to the same operation, not a new request generation.
              force = true;
              continue;
            }
          }
          failure = "Failed to run " + name + ": " + (data.reason || "unknown error");
        } else {
          const runId = data.result?.runId || data.result?.acceptanceId || data.result?.requestId;
          setRunStatus(runId
            ? "Triggered " + name + " (" + runId + ")"
            : "Triggered " + name);
        }
        break;
      }
    } catch (err) {
      failure = "Failed to run " + name + ": " + (err.message || err);
    } finally {
      if (isCurrent()) {
        pendingWorkflowRuns.delete(key);
        await loadSnapshot();
        if (isCurrent()) {
          if (failure) errorEl.textContent = failure;
          workflowRunRequests.delete(key);
        }
      }
    }
  }
  const startBarEl = document.getElementById("start-daemon-bar");
  const startMsgEl = document.getElementById("start-daemon-msg");
  const startBtn = document.getElementById("start-daemon");

  // Offer to launch "goobers up" whenever the selected source is a local
  // instance root with no live daemon — both the hard-disconnected case and
  // the degraded standalone (read-off-disk) case.
  let startInFlight = false;

  function updateStartBar(data) {
    if (startInFlight) return;
    const source = data.source;
    const needsDaemon = !data.connected || data.mode === "standalone";
    if (!source || !needsDaemon) {
      startBarEl.style.display = "none";
      return;
    }
    startBarEl.style.display = "flex";
    if (source.kind === "remote") {
      startMsgEl.textContent =
        "This is a remote control plane. The portal can't start a daemon on another host \u2014 run goobers up there.";
      startBtn.style.display = "none";
      return;
    }
    startBtn.style.display = "";
    startBtn.disabled = false;
    startBtn.textContent = "Start daemon";
    startMsgEl.textContent =
      data.mode === "standalone"
        ? "No daemon is running for this instance \u2014 showing read-only data from disk."
        : "No daemon is running for this instance root.";
  }

  startBtn.addEventListener("click", async () => {
    const sourceId = sourceSelect.value;
    if (!sourceId) return;
    startInFlight = true;
    startBtn.disabled = true;
    startBtn.textContent = "Starting\u2026";
    startMsgEl.textContent = "Launching goobers up \u2014 startup usually takes ~15s\u2026";
    let failure = "";
    try {
      const res = await fetch("/api/start-daemon", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ source: sourceId }),
      });
      const data = await res.json();
      if (!data.ok) failure = "Could not start daemon: " + (data.reason || "unknown error");
    } catch (err) {
      failure = "Could not start daemon: " + (err && err.message ? err.message : String(err));
    } finally {
      startInFlight = false;
      startBtn.disabled = false;
      startBtn.textContent = "Start daemon";
      await refreshAll();
      // refreshAll() re-renders and clears #error, so re-apply the failure after.
      if (failure) errorEl.textContent = failure;
    }
  });

  function fmtTime(v) {
    if (!v) return "\u2014";
    try { return escapeHtml(new Date(v).toLocaleString()); } catch { return escapeHtml(v); }
  }

  function renderAttention(items, runs) {
    const attention = items || [];
    if (!attention.length) {
      attentionListEl.replaceChildren(Object.assign(document.createElement("p"), {
        className: "muted", textContent: "Nothing currently needs attention.",
      }));
      return;
    }
    const visible = attention.filter((item) => dismissedAttention.get(item.id) !== item.key);
    if (!visible.length) {
      attentionListEl.replaceChildren(Object.assign(document.createElement("p"), {
        className: "muted", textContent: "Nothing currently needs attention.",
      }));
      return;
    }
    const markup = '<div class="attention-list">' + visible.map((item, index) => {
      const run = (runs || []).find((candidate) => (candidate.runId || candidate.id) === item.id);
      const runLabel = escapeHtml(item.id || "unknown run");
      const stage = item.stage ? " · " + renderGooberChip(item.stage, { kind: "stage" }) : "";
      const elapsed = item.elapsedMillis == null ? "" : " · " + Math.round(item.elapsedMillis / 60000) + "m";
      const expanded = expandedAttention.has(item.id);
      const detailsId = "attention-details-" + index;
      return '<div class="attention-item' + (expanded ? " is-expanded" : "") + '" data-attention-item="' +
        escapeHtml(item.id) + '">' +
        '<strong>' + escapeHtml(item.phase) + '</strong>' +
        '<span class="attention-reason" id="' + detailsId + '">' +
        (run ? '<a href="#run=' + encodeURIComponent(item.id) + '" data-attention-run="' + escapeHtml(item.id) + '">' : "") +
        '<code>' + runLabel + '</code> ' + escapeHtml(item.workflow) + stage + escapeHtml(elapsed) +
        (run ? "</a>" : "") + '<br />' + escapeHtml(item.reason) + '</span>' +
        '<span class="attention-action">' + escapeHtml(item.nextAction) +
        ' <button type="button" data-expand-attention="' + escapeHtml(item.id) +
        '" aria-expanded="' + String(expanded) + '" aria-controls="' + detailsId + '">' +
        (expanded ? "Less" : "More") + '</button>' +
        ' <button type="button" data-dismiss-attention="' + escapeHtml(item.id) +
        '" aria-label="Dismiss attention for ' + runLabel + '">Dismiss</button></span></div>';
    }).join("") + "</div>";
    if (attentionListEl.innerHTML !== markup) attentionListEl.innerHTML = markup;
    attentionListEl.querySelectorAll("[data-attention-run]").forEach((link) =>
      link.addEventListener("click", (event) => {
        event.preventDefault();
        openRun(link.dataset.attentionRun);
      }));
    attentionListEl.querySelectorAll("[data-dismiss-attention]").forEach((button) =>
      button.addEventListener("click", () => {
        const item = attention.find((candidate) => candidate.id === button.dataset.dismissAttention);
        if (item) dismissedAttention.set(item.id, item.key);
        renderAttention(attention, runs);
      }));
    attentionListEl.querySelectorAll("[data-expand-attention]").forEach((button) =>
      button.addEventListener("click", () => {
        const id = button.dataset.expandAttention;
        const card = button.closest("[data-attention-item]");
        const expanded = !expandedAttention.has(id);
        if (expanded) expandedAttention.add(id);
        else expandedAttention.delete(id);
        card?.classList.toggle("is-expanded", expanded);
        button.setAttribute("aria-expanded", String(expanded));
        button.textContent = expanded ? "Less" : "More";
      }));
  }

  async function loadSources() {
    const [res, selectedRes] = await Promise.all([
      fetch("/api/sources"),
      fetch("/api/selected-source"),
    ]);
    const [data, selected] = await Promise.all([res.json(), selectedRes.json()]);
    const sources = data.sources || [];
    const prevValue = sourceSelect.value;
    sourceSelect.innerHTML = "";
    if (sources.length === 0) {
      sourceSelect.innerHTML = '<option value="">No sources yet</option>';
      return null;
    }
    for (const s of sources) {
      const opt = document.createElement("option");
      opt.value = s.id;
      opt.dataset.kind = s.kind;
      const dot = s.connected ? "\u25cf" : "\u25cb";
      opt.textContent = dot + " " + (s.label || s.value) + " (" + s.kind + ")";
      sourceSelect.appendChild(opt);
    }
    if (prevValue && sources.some((s) => s.id === prevValue)) {
      sourceSelect.value = prevValue;
      return prevValue;
    }
    if (selected.sourceId && sources.some((s) => s.id === selected.sourceId)) {
      sourceSelect.value = selected.sourceId;
      return selected.sourceId;
    }
    const firstConnected = sources.find((s) => s.connected);
    sourceSelect.value = (firstConnected || sources[0]).id;
    return sourceSelect.value;
  }

  function renderSnapshot(data) {
    errorEl.textContent = "";
    document.getElementById("source-context").textContent =
      data.source?.label || data.instance?.name || data.source?.value || "";
    updateFleetPanel(data);
    updateStartBar(data);
    if (!data.connected) {
      emptyEl.style.display = "none";
      dashboardEl.style.display = "none";
      setFreshnessState("Offline");
      if (data.reason) {
        errorEl.textContent = data.source
          ? "Not connected to " + (data.source.label || data.source.value) + ": " + data.reason
          : data.reason;
      }
      return;
    }
    emptyEl.style.display = "none";
    dashboardEl.style.display = selectedRunId ? "none" : "block";

    lastCapabilities = data.capabilities || {};
    const workflows = data.workflows || [];
    const runs = data.runs || [];
    lastSnapshot = data;
    lastUpdatedAt = Date.now();
    const freshness = deriveFreshnessState({
      lastUpdatedAt,
      connected: true,
      mode: data.mode === "daemon" ? "daemon" : "polling",
      now: Date.now(),
    });
    setFreshnessState(freshness, lastUpdatedAt);
    renderAttention(data.attention, runs);
    instanceWarningsEl.innerHTML = renderConfigurationWarnings(
      data.instance?.warnings || [],
      "instance",
      { dismissedWarningKeys: dismissedConfigurationWarnings },
    );
    const inFlight = workflows.reduce((n, w) => n + (w.concurrency?.activeRuns || 0), 0);

    cardsEl.innerHTML = "";
    const cards = [
      ["Workflows", workflows.length],
      ["In flight", inFlight],
      ["Loaded runs", runs.length],
      ["Warnings", (data.instance?.warnings || []).length],
    ];
    for (const [label, value] of cards) {
      const div = document.createElement("div");
      div.className = "card";
      div.innerHTML = renderSnapshotCard(label, value);
      cardsEl.appendChild(div);
    }

    workflowsBody.innerHTML = "";
    for (const w of workflows) {
      const tr = document.createElement("tr");
      const name = w.identity ? w.identity.name : w.name;
      const gaggle = w.identity ? w.identity.gaggle : w.gaggle;
      const triggers = w.triggers || [];
      const triggerKinds = triggers.map((t) => t.type || t.kind).filter(Boolean);
      const triggerLabel = triggerKinds.length ? triggerKinds.join(", ") : "\u2014";
      const nonManualTriggers = triggers.filter((t) => (t.type || t.kind) !== "manual");
      // Workflows carry an optional human-readable blurb via the
      // goobers.dev/purpose annotation, surfaced as the purpose field by the
      // read API. Fall back to displayName when it adds something over the name.
      const displayName = w.displayName || "";
      const purpose = (w.purpose || "").trim();
      const nameTip = purpose || (displayName && displayName !== name ? displayName : "");
      const nameTitleAttr = nameTip ? ' title="' + escapeHtml(nameTip) + '"' : "";
      tr.innerHTML =
        "<td" + nameTitleAttr + '><button type="button" class="table-link">' + escapeHtml(name) + "</button></td>" +
        "<td>" + escapeHtml(gaggle) + "</td>" +
        "<td>" + escapeHtml(triggerLabel) + "</td>" +
        "<td>" + escapeHtml(w.concurrency?.activeRuns ?? "\u2014") + "</td>" +
        "<td>" + escapeHtml(w.concurrency?.maxConcurrentRuns ?? "\u2014") + "</td>" +
        '<td class="run-now-cell"></td>' +
        '<td class="enabled-cell"></td>' +
        '<td class="configuration-warning-cell"></td>';
      tr.classList.add("clickable-row");
      tr.title = "Filter runs to this workflow";
      tr.addEventListener("click", (ev) => {
        if (ev.target.closest(".enabled-cell, .run-now-cell, .configuration-warning-cell")) return;
        filterToWorkflow(gaggle, name);
      });
      workflowsBody.appendChild(tr);

      const runCell = tr.querySelector(".run-now-cell");
      const runKey = gaggle + "/" + name;
      const runPending = pendingWorkflowRuns.has(runKey);
      // Manual triggers only work against a live daemon (client.mjs's
      // triggerWorkflowNow() throws outside daemon mode) - don't offer a
      // control guaranteed to fail for standalone/remote-polling sources.
      const runSupported = data.mode === "daemon";
      const runBtn = document.createElement("button");
      runBtn.type = "button";
      runBtn.className = "workflow-run-now";
      // Icon-only control, matching the enable/disable toggle: the accessible
      // name comes from aria-label rather than the glyph.
      runBtn.textContent = runPending ? "\u23F3" : "\u25B6\uFE0F";
      runBtn.disabled = runPending || !runSupported;
      const runActionLabel = "Run " + name + " now";
      runBtn.title = !runSupported
        ? "Manually triggering a workflow requires a live daemon connection"
        : runPending
          ? "Triggering\u2026"
          : runActionLabel;
      runBtn.setAttribute(
        "aria-label",
        !runSupported ? runActionLabel + " (requires a live daemon)" : runPending ? "Triggering " + name : runActionLabel,
      );
      runBtn.addEventListener("click", (ev) => {
        ev.stopPropagation();
        if (!runSupported || pendingWorkflowRuns.has(runKey)) return;
        runWorkflowNow(gaggle, name);
      });
      runCell.appendChild(runBtn);

      const enabledCell = tr.querySelector(".enabled-cell");
      const warningCell = tr.querySelector(".configuration-warning-cell");
      warningCell.dataset.warningContext = "workflow";
      warningCell.dataset.gaggle = gaggle;
      warningCell.dataset.workflow = name;
      warningCell.innerHTML = renderConfigurationWarnings(
        w.warnings || [],
        "workflow",
        { dismissedWarningKeys: dismissedConfigurationWarnings },
      );
      if (nonManualTriggers.length === 0) {
        enabledCell.innerHTML = '<span class="muted">manual only</span>';
      } else if (!lastCapabilities.workflowEnable) {
        const anyEnabled = nonManualTriggers.some((t) => t.enabled !== false);
        enabledCell.innerHTML = '<span class="muted">' + (anyEnabled ? "enabled" : "disabled") + "</span>";
      } else {
        const anyEnabled = nonManualTriggers.some((t) => t.enabled !== false);
        const pendKey = gaggle + "/" + name;
        const pendingDesired = pendingToggles.get(pendKey);
        const isPending = pendingDesired !== undefined;

        const wrap = document.createElement("span");
        wrap.className = "enabled-wrap";

        // The status is a read-only label; acting on it is the adjacent
        // button's job, so a stray click on the state can't mutate anything.
        const label = document.createElement("span");
        label.className =
          "workflow-state " + (isPending ? "is-pending" : anyEnabled ? "is-enabled" : "is-disabled");
        // While a toggle is in flight the label must keep saying "Saving…"
        // across every re-render (including the 5s background poll) until the
        // daemon's own read model actually reports the new state.
        label.textContent = isPending ? "Saving\u2026" : anyEnabled ? "Enabled" : "Disabled";
        if (isPending) label.title = "Waiting for the daemon to apply and reload this change\u2026";
        wrap.appendChild(label);

        const btn = document.createElement("button");
        btn.className = "workflow-toggle " + (anyEnabled ? "is-stop" : "is-start");
        // Icon-only control, so the accessible name has to come from
        // aria-label rather than the glyph.
        btn.textContent = anyEnabled ? "\u23F9\uFE0F" : "\u25B6\uFE0F";
        btn.disabled = isPending;
        const actionLabel = anyEnabled
          ? "Disable this workflow's non-manual triggers"
          : "Enable this workflow's non-manual triggers";
        btn.title = actionLabel;
        btn.setAttribute("aria-label", actionLabel);
        btn.addEventListener("click", (ev) => {
          ev.stopPropagation();
          if (pendingToggles.has(pendKey)) return;
          if (!window.confirm(actionLabel + "? The change will be confirmed by the daemon before it is shown as applied.")) return;
          toggleWorkflow(gaggle, name, !anyEnabled);
        });
        wrap.appendChild(btn);
        const undo = workflowUndo.get(pendKey);
        if (undo && !isPending) {
          const undoBtn = document.createElement("button");
          undoBtn.type = "button";
          undoBtn.textContent = "Undo";
          undoBtn.title = "Restore the previous workflow state";
          undoBtn.addEventListener("click", (ev) => {
            ev.stopPropagation();
            if (!window.confirm("Restore the previous workflow state?")) return;
            workflowUndo.delete(pendKey);
            toggleWorkflow(gaggle, name, undo.enabled);
          });
          wrap.appendChild(undoBtn);
        }

        enabledCell.appendChild(wrap);
      }
    }
    if (workflows.length === 0) {
      workflowsBody.innerHTML = '<tr><td colspan="8" class="muted">No workflows configured.</td></tr>';
    }

    populateFilterOptions(data.gaggles || [], workflows);
    // Rebuild the preset list while preserving the operator's selection and focus.
    const prevPresetSelection = savedFilterPresets.value;
    const prevPresetFocused = document.activeElement === savedFilterPresets;
    loadSavedFilters();
    if (prevPresetSelection) savedFilterPresets.value = prevPresetSelection;
    if (prevPresetFocused) savedFilterPresets.focus();

    const restored = restoreFiltersFromUrl();
    setAdvancedFilterSupport((data.mode || "daemon") === "daemon");
    // Background polling refreshes the whole snapshot every few seconds; if
    // the operator has an active filter, re-fetch through the filtered path
    // instead of clobbering the table with the unfiltered snapshot runs.
    if (restored || hasActiveFilters()) {
      void applyFilters();
    } else {
      lastRuns = runs;
      renderRuns(runs);
    }
    if (restoredRunId && runs.some((run) => (run.runId || run.id) === restoredRunId)) {
      const runId = restoredRunId;
      restoredRunId = "";
      void openRun(runId);
    }
  }

  // ---- Runs table: filters + client-side sort ----
  const filterGaggle = document.getElementById("filter-gaggle");
  const filterWorkflow = document.getElementById("filter-workflow");
  const filterPhase = document.getElementById("filter-phase");
  const filterTrigger = document.getElementById("filter-trigger");
  const filterStage = document.getElementById("filter-stage");
  const filterOutcome = document.getElementById("filter-outcome");
  const filterPopulation = document.getElementById("filter-population");
  const filterNoWork = document.getElementById("filter-show-no-work");
  const filterSince = document.getElementById("filter-since");
  const filterUntil = document.getElementById("filter-until");
  const savedFilterPresets = document.getElementById("saved-filter-presets");
  const saveFilterPresetButton = document.getElementById("save-filter-preset");
  const loadMoreButton = document.getElementById("runs-load-more");
  let lastRuns = [];
  let sortKey = "startedAt";
  let sortDir = "desc";
  let advancedFiltersSupported = false;
  let filterRequestSequence = 0;
  let restoredFilters = false;
  let lastCursor = "";
  let hasMoreRuns = false;
  let invalidCursorRecoveryInProgress = false;
  let filterPersistenceTimer = null;
  const persistedFilterState = ${initialFilters};
  const multiFilters = new Map();

  function selectedOptionLabels(element) {
    return [...element.selectedOptions].map((option) => option.textContent.trim()).filter(Boolean);
  }

  function selectedValues(element) {
    return [...element.selectedOptions].map((option) => option.value).filter(Boolean);
  }

  function setSelectedValues(element, values) {
    const selected = new Set(Array.isArray(values) ? values : values ? [values] : []);
    for (const option of element.options) option.selected = selected.has(option.value);
    syncMultiFilter(element);
  }

  function syncMultiFilter(select) {
    const state = multiFilters.get(select);
    if (!state) return;
    const options = [...select.options].filter((option) => option.value);
    const selected = selectedValues(select);
    const labels = selectedOptionLabels(select);
    state.button.disabled = select.disabled;
    state.button.title = select.title || "";
    state.button.setAttribute("aria-expanded", String(!state.menu.hidden));
    const buttonLabel = labels.length === 0
      ? (select.dataset.allLabel || "All")
      : labels.length <= 2 ? labels.join(", ") : labels.length + " selected";
    state.button.textContent = buttonLabel;
    state.button.setAttribute("aria-label", buttonLabel);
    state.menu.innerHTML = "";
    if (options.length === 0) {
      state.menu.innerHTML = '<div class="multi-filter-empty">No options available</div>';
      return;
    }
    for (const option of options) {
      const id = select.id + "-option-" + option.value.replace(/[^A-Za-z0-9_-]/g, "-");
      const label = document.createElement("label");
      label.className = "multi-filter-option";
      label.setAttribute("for", id);
      const checkbox = document.createElement("input");
      checkbox.type = "checkbox";
      checkbox.id = id;
      checkbox.value = option.value;
      checkbox.checked = selected.includes(option.value);
      checkbox.disabled = select.disabled;
      checkbox.addEventListener("change", () => {
        option.selected = checkbox.checked;
        syncMultiFilter(select);
        select.dispatchEvent(new Event("change", { bubbles: true }));
      });
      const text = document.createElement("span");
      text.textContent = option.textContent;
      label.append(checkbox, text);
      state.menu.appendChild(label);
    }
  }

  function closeMultiFilters(except) {
    for (const state of multiFilters.values()) {
      if (state === except) continue;
      state.menu.hidden = true;
      state.button.setAttribute("aria-expanded", "false");
    }
  }

  function initMultiFilter(select) {
    const wrapper = document.createElement("div");
    wrapper.className = "multi-filter";
    wrapper.dataset.filterControl = select.id;
    const button = document.createElement("button");
    button.type = "button";
    button.className = "multi-filter-button";
    button.setAttribute("aria-haspopup", "true");
    button.setAttribute("aria-expanded", "false");
    const menu = document.createElement("div");
    menu.className = "multi-filter-menu";
    menu.hidden = true;
    menu.setAttribute("role", "group");
    menu.setAttribute("aria-label", select.getAttribute("aria-label") || select.title || "Filter options");
    select.before(wrapper);
    wrapper.append(button, menu, select);
    const state = { wrapper, button, menu };
    multiFilters.set(select, state);
    button.addEventListener("click", () => {
      if (select.disabled) return;
      const shouldOpen = menu.hidden;
      closeMultiFilters(state);
      menu.hidden = !shouldOpen;
      syncMultiFilter(select);
    });
    button.addEventListener("keydown", (event) => {
      if (event.key === "ArrowDown" && menu.hidden) {
        event.preventDefault();
        button.click();
        menu.querySelector("input")?.focus();
      }
      if (event.key === "Escape") {
        menu.hidden = true;
        syncMultiFilter(select);
      }
    });
    menu.addEventListener("keydown", (event) => {
      if (event.key === "Escape") {
        menu.hidden = true;
        syncMultiFilter(select);
        button.focus();
      }
    });
    syncMultiFilter(select);
  }

  [filterGaggle, filterWorkflow, filterPhase, filterTrigger, filterOutcome, filterPopulation].forEach(initMultiFilter);
  document.addEventListener("click", (event) => {
    if (![...multiFilters.values()].some((state) => state.wrapper.contains(event.target))) closeMultiFilters();
    const warningButton = event.target.closest("[data-dismiss-warning], [data-dismiss-warning-group]");
    if (!warningButton) return;
    event.stopPropagation();
    if (warningButton.hasAttribute("data-dismiss-warning-group")) {
      warningButton.closest(".configuration-warning-group")
        ?.querySelectorAll("[data-warning-key]")
        .forEach((warning) => dismissedConfigurationWarnings.add(warning.dataset.warningKey));
    } else {
      dismissedConfigurationWarnings.add(warningButton.dataset.dismissWarning);
    }
    rerenderConfigurationWarnings(warningButton.closest("[data-warning-context]"));
  });

  function rerenderConfigurationWarnings(container) {
    if (!container || !lastSnapshot) return;
    const context = container.dataset.warningContext;
    let warnings = [];
    if (context === "instance") {
      warnings = lastSnapshot.instance?.warnings || [];
    } else {
      const workflow = (lastSnapshot.workflows || []).find((item) =>
        (item.identity?.gaggle || item.gaggle) === container.dataset.gaggle &&
        (item.identity?.name || item.name) === container.dataset.workflow);
      warnings = workflow?.warnings || [];
    }
    container.innerHTML = renderConfigurationWarnings(
      warnings,
      context,
      { dismissedWarningKeys: dismissedConfigurationWarnings },
    );
  }

  function populateFilterOptions(gaggles, workflows) {
    const prevGaggle = selectedValues(filterGaggle);
    const gaggleNames = gaggles.length ? gaggles.map((g) => g.name) : [...new Set(workflows.map((w) => (w.identity ? w.identity.gaggle : w.gaggle)))];
    filterGaggle.innerHTML = gaggleNames.filter(Boolean).map((n) => '<option value="' + escapeHtml(n) + '">' + escapeHtml(n) + "</option>").join("");
    setSelectedValues(filterGaggle, prevGaggle);

    const prevWorkflow = selectedValues(filterWorkflow);
    const workflowNames = [...new Set(workflows.map((w) => (w.identity ? w.identity.name : w.name)))].filter(Boolean);
    filterWorkflow.innerHTML = workflowNames.map((n) => '<option value="' + escapeHtml(n) + '">' + escapeHtml(n) + "</option>").join("");
    setSelectedValues(filterWorkflow, prevWorkflow);
  }

  function currentFilters() {
    const f = {};
    for (const [key, element] of [
      ["gaggle", filterGaggle], ["workflow", filterWorkflow], ["phase", filterPhase],
      ["trigger", filterTrigger], ["outcome", filterOutcome], ["population", filterPopulation],
    ]) {
      const values = selectedValues(element);
      if (values.length) f[key] = values;
    }
    if (filterStage.value.trim()) f.stage = filterStage.value.trim();
    if (filterNoWork.checked) f.showNoWork = true;
    if (filterSince.value) f.since = new Date(filterSince.value).toISOString();
    if (filterUntil.value) f.until = new Date(filterUntil.value).toISOString();
    return f;
  }

  function syncViewUrl(runId = selectedRunId) {
    const query = new URLSearchParams(encodeViewState(currentFilters(), runId));
    const next = query.toString();
    window.history.replaceState(null, "", next ? "?" + next : window.location.pathname);
  }

  function persistFilters() {
    clearTimeout(filterPersistenceTimer);
    filterPersistenceTimer = setTimeout(() => {
      void fetch("/api/preferences", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ filters: currentFilters() }),
      }).catch((err) => {
        errorEl.textContent = portalRequestError(err);
      });
    }, 150);
  }

  function restoreFiltersFromUrl() {
    if (restoredFilters) return;
    restoredFilters = true;
    const decoded = decodeViewState(window.location.search);
    const filters = Object.keys(decoded.filters).length ? decoded.filters : persistedFilterState;
    const { selectedRun } = decoded;
    restoredRunId = selectedRun;
    for (const [key, element] of [
      ["gaggle", filterGaggle], ["workflow", filterWorkflow], ["phase", filterPhase],
      ["trigger", filterTrigger], ["stage", filterStage], ["outcome", filterOutcome],
      ["population", filterPopulation], ["since", filterSince], ["until", filterUntil],
    ]) {
      const value = filters[key];
      if (element.multiple) {
        setSelectedValues(element, value);
      } else if (typeof value === "string") {
        element.value = element.type === "datetime-local"
          ? value.slice(0, 16)
          : value;
      }
    }
    if (filters.showNoWork === true) filterNoWork.checked = true;
    return shouldApplyRestoredFilters(filters);
  }

  function setAdvancedFilterSupport(supported) {
    advancedFiltersSupported = supported;
    filterStage.disabled = !supported;
    filterStage.title = supported ? "Stage name" : "Requires a running Goobers daemon";
    if (!supported) filterStage.value = "";

    const hasStage = supported && Boolean(filterStage.value.trim());
    for (const el of [filterOutcome, filterPopulation]) {
      el.disabled = !hasStage;
      el.title = !supported
        ? "Requires a running Goobers daemon"
        : hasStage ? "" : "Choose a stage first";
      if (!hasStage) setSelectedValues(el, []);
      else syncMultiFilter(el);
    }
    filterNoWork.disabled = !supported;
    filterNoWork.title = supported ? "Include no-work runs" : "Requires a running Goobers daemon";
    if (!supported) filterNoWork.checked = false;
  }

  function hasActiveFilters() {
    return Object.keys(currentFilters()).length > 0;
  }

  function sortRuns(runs) {
    const dir = sortDir === "asc" ? 1 : -1;
    return [...runs].sort((a, b) => {
      const av = (sortKey === "id" ? (a.runId || a.id) : sortKey === "trigger" ? a.trigger?.kind : a[sortKey]) || "";
      const bv = (sortKey === "id" ? (b.runId || b.id) : sortKey === "trigger" ? b.trigger?.kind : b[sortKey]) || "";
      if (av < bv) return -1 * dir;
      if (av > bv) return 1 * dir;
      return 0;
    });
  }

  function updateSortIndicators() {
    document.querySelectorAll("#runs-table th[data-sort]").forEach((th) => {
      const button = th.querySelector("button");
      const label = button.textContent.replace(/\s*[\u25b2\u25bc]$/, "");
      button.textContent = label;
      th.setAttribute("aria-sort", th.dataset.sort === sortKey ? (sortDir === "asc" ? "ascending" : "descending") : "none");
      if (th.dataset.sort === sortKey) {
        const arrow = document.createElement("span");
        arrow.className = "sort-arrow";
        arrow.textContent = sortDir === "asc" ? "\u25b2" : "\u25bc";
        button.appendChild(arrow);
      }
    });
  }

  function renderRuns(runs) {
    const sorted = sortRuns(runs);
    runsBody.innerHTML = "";
    for (const r of sorted) {
      const tr = document.createElement("tr");
      const runId = r.runId || r.id;
      const actionsUrl = safeExternalUrl(r.actionsURL);
      const actionsLink = actionsUrl
        ? ' <a class="actions-run-link" href="' + escapeHtml(actionsUrl) +
          '" target="_blank" rel="noopener noreferrer" title="Open GitHub Actions run">Action &#8599;</a>'
        : "";
      const associations = renderRunAssociations(r);
      tr.className = "clickable-row";
      tr.dataset.runId = runId;
      tr.innerHTML = renderRunRowCells(r, {
        actionsLink,
        associations,
        startedAt: fmtTime(r.startedAt),
        lastActivityAt: fmtTime(r.lastActivityAt),
      });
      attachRunIdControls(tr);
      tr.querySelectorAll(".actions-run-link, .run-association-link").forEach((link) =>
        link.addEventListener("click", (event) => event.stopPropagation()));
      tr.addEventListener("click", () => openRun(runId));
      runsBody.appendChild(tr);
    }
    if (sorted.length === 0) {
      runsBody.innerHTML = '<tr><td colspan="8" class="muted">No runs match the current filters.</td></tr>';
    }
    updateSortIndicators();
  }

  function attachRunIdControls(root) {
    root.querySelectorAll("[data-open-run]").forEach((button) =>
        button.addEventListener("click", (event) => {
          event.stopPropagation();
          openRun(button.dataset.openRun);
        }));
    root.querySelectorAll("[data-copy-run-id]").forEach((button) =>
        button.addEventListener("click", async (event) => {
          event.stopPropagation();
          try {
            await navigator.clipboard.writeText(button.dataset.copyRunId || "");
            button.classList.add("copied");
            button.textContent = "✅";
            setTimeout(() => {
              button.classList.remove("copied");
              button.textContent = "\uD83D\uDCCB";
            }, 1200);
          } catch {
            errorEl.textContent = "Could not copy the run id.";
          }
        }));
  }

  function loadSavedFilters() {
    try {
      const entries = JSON.parse(localStorage.getItem("goobers-portal-filter-presets") || "{}");
      const options = ['<option value="">Saved filters</option>'];
      for (const name of Object.keys(entries)) {
        options.push('<option value="' + escapeHtml(name) + '">' + escapeHtml(name) + '</option>');
      }
      savedFilterPresets.innerHTML = options.join("");
    } catch {
      savedFilterPresets.innerHTML = '<option value="">Saved filters</option>';
    }
  }

  function persistCurrentFilterPreset() {
    const name = window.prompt("Name this filter preset", "");
    if (!name) return;
    try {
      const existing = JSON.parse(localStorage.getItem("goobers-portal-filter-presets") || "{}") || {};
      existing[name] = currentFilters();
      localStorage.setItem("goobers-portal-filter-presets", JSON.stringify(existing));
      loadSavedFilters();
      savedFilterPresets.value = name;
    } catch {
      errorEl.textContent = "Could not save the current filter preset.";
    }
  }

  function restoreSavedFilterPreset(name) {
    try {
      const entries = JSON.parse(localStorage.getItem("goobers-portal-filter-presets") || "{}") || {};
      const value = entries[name];
      if (!value) return;
      for (const [key, element] of [
        ["gaggle", filterGaggle], ["workflow", filterWorkflow], ["phase", filterPhase],
        ["trigger", filterTrigger], ["stage", filterStage], ["outcome", filterOutcome],
        ["population", filterPopulation], ["since", filterSince], ["until", filterUntil],
      ]) {
        if (value[key] !== undefined) {
          if (element.multiple) setSelectedValues(element, value[key]);
          else element.value = typeof value[key] === "string" ? value[key] : String(value[key]);
        }
      }
      filterNoWork.checked = value.showNoWork === true;
      setAdvancedFilterSupport(advancedFiltersSupported);
      persistFilters();
      applyFilters();
    } catch {
      errorEl.textContent = "Could not restore the selected filter preset.";
    }
  }

  async function applyFilters({ append = false } = {}) {
    const sourceId = sourceSelect.value;
    if (!sourceId) return;
    const requestSequence = ++filterRequestSequence;
    try {
      const params = new URLSearchParams({ source: sourceId });
      for (const [key, value] of Object.entries(currentFilters())) {
        if (Array.isArray(value)) value.forEach((item) => params.append(key, item));
        else params.set(key, value);
      }
      if (append && lastCursor) params.set("cursor", lastCursor);
      const res = await fetch("/api/runs?" + params.toString());
      const data = await res.json();
      if (requestSequence !== filterRequestSequence) return;
      if (!data.connected) {
        errorEl.textContent = data.reason || "Could not load runs.";
        return;
      }
      if (data.error) {
        const invalidCursor = isInvalidCursorError(data.error);
        if (invalidCursor && !invalidCursorRecoveryInProgress) {
          invalidCursorRecoveryInProgress = true;
          lastCursor = "";
          try {
            return await applyFilters({ append: false });
          } finally {
            invalidCursorRecoveryInProgress = false;
          }
        }
        errorEl.textContent = data.error;
        lastRuns = [];
        lastCursor = "";
        hasMoreRuns = false;
        loadMoreButton.style.display = "none";
        renderRuns(lastRuns);
        return;
      }
      errorEl.textContent = "";
      const page = mergeRunPage(lastRuns, data, append);
      lastRuns = page.runs;
      lastCursor = page.cursor;
      hasMoreRuns = page.hasMore;
      loadMoreButton.style.display = hasMoreRuns ? "inline-flex" : "none";
      syncViewUrl();
      renderRuns(lastRuns);
    } catch (err) {
      if (requestSequence !== filterRequestSequence) return;
      errorEl.textContent = portalRequestError(err);
    }
  }

  for (const el of [filterGaggle, filterWorkflow, filterPhase, filterTrigger, filterStage, filterOutcome, filterPopulation, filterNoWork, filterSince, filterUntil]) {
    el.addEventListener("change", () => {
      lastCursor = "";
      persistFilters();
      applyFilters();
    });
  }
  filterStage.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      setAdvancedFilterSupport(advancedFiltersSupported);
      persistFilters();
      applyFilters();
    }
  });
  filterStage.addEventListener("input", () => setAdvancedFilterSupport(advancedFiltersSupported));
  document.getElementById("filters-clear").addEventListener("click", () => {
    for (const el of [filterGaggle, filterWorkflow, filterPhase, filterTrigger, filterStage, filterOutcome, filterPopulation, filterSince, filterUntil]) {
      if (el.multiple) setSelectedValues(el, []);
      else el.value = "";
    }
    filterNoWork.checked = false;
    setAdvancedFilterSupport(advancedFiltersSupported);
    lastCursor = "";
    persistFilters();
    applyFilters();
  });

  saveFilterPresetButton.addEventListener("click", persistCurrentFilterPreset);
  savedFilterPresets.addEventListener("change", () => {
    if (!savedFilterPresets.value) return;
    restoreSavedFilterPreset(savedFilterPresets.value);
  });
  loadMoreButton.addEventListener("click", () => {
    if (!hasMoreRuns) return;
    applyFilters({ append: true });
  });

  function filterToWorkflow(gaggle, name) {
    if (gaggle && [...filterGaggle.options].some((o) => o.value === gaggle)) {
      setSelectedValues(filterGaggle, [gaggle]);
    }
    if (name && [...filterWorkflow.options].some((o) => o.value === name)) {
      setSelectedValues(filterWorkflow, [name]);
    }
    activateInternalTab(dashboardEl, "runs");
    persistFilters();
    applyFilters();
    document.getElementById("runs-table")?.scrollIntoView({ behavior: "smooth", block: "start" });
  }
  document.querySelectorAll("#runs-table th[data-sort]").forEach((th) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "sort-button";
    button.textContent = th.textContent;
    th.replaceChildren(button);
    button.addEventListener("click", () => {
      const key = th.dataset.sort;
      if (sortKey === key) {
        sortDir = sortDir === "asc" ? "desc" : "asc";
      } else {
        sortKey = key;
        sortDir = key === "startedAt" || key === "lastActivityAt" ? "desc" : "asc";
      }
      renderRuns(lastRuns);
    });
  });

  // ---- Run detail view ----
  const runViewEl = document.getElementById("run-view");
  const runErrorEl = document.getElementById("run-error");
  const runStatusEl = document.getElementById("run-status");
  const runContentEl = document.getElementById("run-content");
  let graphOrientation = "horizontal";

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  function layoutGraph(graph, orientation) {
    // Layer by distance from the start, then use predecessor barycenters to
    // keep related branches near one another and reduce edge crossings.
    const nodesById = new Map(graph.nodes.map((n) => [n.id, n]));
    const outgoing = new Map();
    const incoming = new Map();
    for (const e of graph.edges) {
      if (!e.target) continue;
      if (!outgoing.has(e.source)) outgoing.set(e.source, []);
      outgoing.get(e.source).push(e.target);
      if (!incoming.has(e.target)) incoming.set(e.target, []);
      incoming.get(e.target).push(e.source);
    }
    const depth = new Map();
    const queue = [[graph.start, 0]];
    depth.set(graph.start, 0);
    while (queue.length) {
      const [id, d] = queue.shift();
      for (const next of outgoing.get(id) || []) {
        if (!depth.has(next) || depth.get(next) > d + 1) {
          depth.set(next, d + 1);
          queue.push([next, d + 1]);
        }
      }
    }
    const columns = new Map();
    for (const n of graph.nodes) {
      const d = depth.has(n.id) ? depth.get(n.id) : 0;
      if (!columns.has(d)) columns.set(d, []);
      columns.get(d).push(n.id);
    }
    const maxCol = Math.max(0, ...columns.keys());
    for (let d = 1; d <= maxCol; d++) {
      const ids = columns.get(d) || [];
      const previous = columns.get(d - 1) || [];
      const previousIndex = new Map(previous.map((id, i) => [id, i]));
      ids.sort((a, b) => {
        const score = (id) => {
          const parents = (incoming.get(id) || []).filter((p) => previousIndex.has(p));
          if (!parents.length) return Number.MAX_SAFE_INTEGER;
          return parents.reduce((sum, p) => sum + previousIndex.get(p), 0) / parents.length;
        };
        return score(a) - score(b) || a.localeCompare(b);
      });
    }
    const nodeW = 150, nodeH = 42, padX = 24, padY = 24;
    // Reserve enough horizontal space between stages for the longest transition
    // label. The graph is zoomable, so growing its natural width is preferable
    // to drawing labels over the source or destination node.
    const longestOutcome = graph.edges.reduce(
      (longest, edge) => Math.max(longest, String(edge.outcome || "").length),
      0,
    );
    const edgeLabelWidth = longestOutcome * 5.5 + 20;
    const horizontal = orientation !== "vertical";
    const depthStep = horizontal
      ? nodeW + Math.max(80, edgeLabelWidth)
      : nodeH + 64;
    const branchStep = horizontal
      ? 68
      : Math.max(nodeW + 40, edgeLabelWidth + 30);
    const positions = new Map();
    for (const [d, ids] of columns) {
      ids.forEach((id, i) => {
        positions.set(id, horizontal
          ? { x: padX + d * depthStep, y: padY + i * branchStep }
          : { x: padX + i * branchStep, y: padY + d * depthStep });
      });
    }
    const placed = [...positions.values()];
    return {
      positions,
      width: Math.max(nodeW + padX * 2, ...placed.map((p) => p.x + nodeW + padX)),
      height: Math.max(nodeH + padY * 2, ...placed.map((p) => p.y + nodeH + padY)),
      nodeW,
      nodeH,
      nodesById,
      orientation: horizontal ? "horizontal" : "vertical",
    };
  }

  function workflowDetailKey(sourceId, gaggle, workflow) {
    return [sourceId, gaggle, workflow].join("\\u0000");
  }

  function loadWorkflowDefinition(sourceId, gaggle, workflow) {
    const key = workflowDetailKey(sourceId, gaggle, workflow);
    if (workflowDetailCache.has(key)) return workflowDetailCache.get(key);
    const request = fetch("/api/workflow-detail?source=" + encodeURIComponent(sourceId) +
      "&gaggle=" + encodeURIComponent(gaggle) + "&workflow=" + encodeURIComponent(workflow))
      .then((response) => response.json())
      .then((data) => {
        if (!data.connected) throw new Error(data.reason || "Workflow detail unavailable.");
        if (!data.workflow) throw new Error("Workflow definition was empty.");
        return data.workflow;
      });
    workflowDetailCache.set(key, request);
    request.catch(() => {
      if (workflowDetailCache.get(key) === request) workflowDetailCache.delete(key);
    });
    return request;
  }

  function setStageInspectorStatus(inspector, message, error = false) {
    const liveRegion = document.getElementById("stage-inspector-status");
    if (liveRegion) {
      liveRegion.setAttribute("aria-live", error ? "assertive" : "polite");
      liveRegion.textContent = message;
    }
    const state = document.createElement("div");
    state.className = error ? "stage-inspector-state stage-inspector-error" : "stage-inspector-state";
    state.setAttribute("role", error ? "alert" : "status");
    state.textContent = message;
    inspector.replaceChildren(state);
  }

  async function inspectStage(stageName, run, sourceId, runSequence) {
    const inspector = document.getElementById("stage-inspector");
    if (!inspector) return;
    selectedStageName = stageName;
    document.querySelectorAll("#graph-svg .stage-node").forEach((node) => {
      const selected = node.dataset.stageName === stageName;
      node.classList.toggle("selected", selected);
      node.setAttribute("aria-pressed", String(selected));
    });
    const requestSequence = ++stageInspectorRequestSequence;
    setStageInspectorStatus(inspector, "Loading " + stageName + " definition\u2026");
    try {
      if (!run.gaggle || !run.workflow) {
        throw new Error("This run does not identify its gaggle and workflow.");
      }
      const detail = await loadWorkflowDefinition(sourceId, run.gaggle, run.workflow);
      if (sourceId !== sourceSelect.value || runSequence !== runRequestSequence ||
          requestSequence !== stageInspectorRequestSequence || selectedStageName !== stageName) return;
      const stage = (detail.stages || []).find((candidate) => candidate.name === stageName);
      if (!stage) {
        throw new Error('The workflow definition does not contain stage "' + stageName + '".');
      }
      inspector.innerHTML = renderStageDefinitionInspector(stage, activeStageInspectorView);
      initInternalTabs(inspector.querySelector(".stage-definition-panel"), activeStageInspectorView);
      const liveRegion = document.getElementById("stage-inspector-status");
      if (liveRegion) {
        liveRegion.setAttribute("aria-live", "polite");
        liveRegion.textContent = stageName + " definition loaded.";
      }
    } catch (err) {
      if (sourceId !== sourceSelect.value || runSequence !== runRequestSequence ||
          requestSequence !== stageInspectorRequestSequence || selectedStageName !== stageName) return;
      setStageInspectorStatus(
        inspector,
        "Stage definition unavailable: " + portalRequestError(err),
        true,
      );
    }
  }

  function renderGraphSvg(graph, transitions, events, run, orientation = "horizontal", selectedStage = "") {
    if (!graph || !graph.nodes) return '<p class="muted">No workflow graph available for this run.</p>';
    const terminalTransition = [...(transitions || [])].reverse().find((t) => t.terminal);
    const lastFinishedStage = [...(events || [])].reverse().find((event) =>
      event.type === "stage.finished" && event.stage,
    );
    const terminalSource = terminalTransition?.source || (run.terminal && lastFinishedStage?.stage) || "";
    const finalNodeId = "__run_final__";
    const finalLabel = (terminalTransition && terminalTransition.status) || run.phase || "terminal";
    const graphForLayout = {
      ...graph,
      nodes: [...graph.nodes],
      edges: [...graph.edges],
    };
    if (terminalSource) {
      graphForLayout.nodes.push({ id: finalNodeId, kind: "terminal", label: finalLabel });
      graphForLayout.edges.push({
        source: terminalSource,
        target: finalNodeId,
        outcome: (terminalTransition && (terminalTransition.verdict || terminalTransition.status)) || finalLabel,
        synthetic: true,
      });
    }
    const layout = layoutGraph(graphForLayout, orientation);
    const traversedPairs = new Set();
    for (const t of transitions || []) {
      if (t.source && t.target) traversedPairs.add(t.source + "->" + t.target);
    }
    if (terminalSource) {
      traversedPairs.add(terminalSource + "->" + finalNodeId);
    }
    const visited = new Set((transitions || []).flatMap((t) => [t.source, t.target].filter(Boolean)));
    const stageStates = new Map();
    for (const event of events || []) {
      if (!event.stage) continue;
      if (event.type === "stage.started") stageStates.set(event.stage, "running");
      if (event.type === "stage.finished") stageStates.set(event.stage, String(event.status || "completed").toLowerCase());
      if (event.type === "stage.skipped") stageStates.set(event.stage, "skipped");
      if (event.type === "stage.blocked") stageStates.set(event.stage, "blocked");
    }
    const edgeGroups = new Map();
    for (const e of graphForLayout.edges) {
      if (!e.target) continue;
      const key = e.source + "->" + e.target;
      if (!edgeGroups.has(key)) edgeGroups.set(key, []);
      edgeGroups.get(key).push(e);
    }

    let svg = "";
    // Edges first (so nodes draw on top).
    for (const e of graphForLayout.edges) {
      if (!e.target) continue;
      const a = layout.positions.get(e.source);
      const b = layout.positions.get(e.target);
      if (!a || !b) continue;
      const traversed = traversedPairs.has(e.source + "->" + e.target);
      const siblings = edgeGroups.get(e.source + "->" + e.target) || [e];
      const lane = siblings.indexOf(e) - (siblings.length - 1) / 2;
      const laneOffset = lane * 14;
      let labelX;
      let labelY;
      if (layout.orientation === "vertical") {
        const x1 = a.x + layout.nodeW / 2, y1 = a.y + layout.nodeH;
        const x2 = b.x + layout.nodeW / 2, y2 = b.y;
        const midY = (y1 + y2) / 2;
        svg += '<path class="' + (traversed ? "edge-traversed" : "edge") + '" d="M ' + x1 + " " + y1 + " C " + (x1 + laneOffset) + " " + midY + ", " + (x2 + laneOffset) + " " + midY + ", " + x2 + " " + y2 + '" />';
        labelX = (x1 + x2) / 2 + laneOffset;
        labelY = midY - 5;
      } else {
        const x1 = a.x + layout.nodeW, y1 = a.y + layout.nodeH / 2;
        const x2 = b.x, y2 = b.y + layout.nodeH / 2;
        const midX = (x1 + x2) / 2;
        svg += '<path class="' + (traversed ? "edge-traversed" : "edge") + '" d="M ' + x1 + " " + y1 + " C " + midX + " " + (y1 + laneOffset) + ", " + midX + " " + (y2 + laneOffset) + ", " + x2 + " " + y2 + '" />';
        labelX = midX;
        labelY = (y1 + y2) / 2 + laneOffset - 3;
      }
      if (e.outcome && (siblings.length > 1 || e.synthetic)) {
        svg += '<text class="edge-label" text-anchor="middle" x="' + labelX + '" y="' + labelY + '">' + escapeHtml(e.outcome) + "</text>";
      }
    }
    // Nodes.
    for (const n of graphForLayout.nodes) {
      const p = layout.positions.get(n.id);
      if (!p) continue;
      let cls = "node-rect";
      if (visited.has(n.id)) cls += " visited";
      const state = stageStates.get(n.id) || "pending";
      if (state === "running") cls += " running";
      else if (state === "success" || state === "succeeded" || state === "completed" || state === "no-work") cls += " succeeded";
      else if (state === "failed" || state === "error" || state === "escalated") cls += " failed";
      else if (state === "blocked") cls += " blocked";
      else if (state === "skipped") cls += " skipped";
      else cls += " pending";
      if (n.id === finalNodeId) cls += " terminal " + (run.phase === "failed" || run.phase === "escalated" ? "failed" : "succeeded");
      const interactive = n.id !== finalNodeId;
      if (interactive) {
        svg += '<g class="stage-node' + (n.id === selectedStage ? " selected" : "") +
          '" role="button" tabindex="0" data-stage-name="' + escapeHtml(n.id) +
          '" aria-pressed="' + String(n.id === selectedStage) +
          '" aria-label="Inspect stage ' + escapeHtml(n.id) + '"><title>Inspect stage ' +
          escapeHtml(n.id) + "</title>";
      }
      svg += '<rect class="' + cls + '" x="' + p.x + '" y="' + p.y + '" width="' + layout.nodeW + '" height="' + layout.nodeH + '" rx="6" />';
      const label = n.id === finalNodeId ? "Final: " + finalLabel : n.id + (n.owner ? " (" + n.owner + ")" : "");
      svg += '<text class="node-label" x="' + (p.x + 7) + '" y="' + (p.y + 17) + '">' + escapeHtml(label.length > 24 ? label.slice(0, 23) + "\\u2026" : label) + "</text>";
      if (state && n.id !== finalNodeId) {
        svg += '<text class="node-status" x="' + (p.x + 7) + '" y="' + (p.y + 32) + '">' + escapeHtml(state) + "</text>";
      } else if (n.id === finalNodeId) {
        svg += '<text class="node-status" x="' + (p.x + 7) + '" y="' + (p.y + 32) + '">' + escapeHtml(run.terminal ? "terminal" : "current") + "</text>";
      }
      if (interactive) svg += "</g>";
    }
    return '<div class="graph-panel">' +
      '<div class="graph-toolbar" role="toolbar" aria-label="Workflow graph zoom controls">' +
      '<button type="button" data-graph-action="out" aria-label="Zoom out" title="Zoom out">&minus;</button>' +
      '<span class="graph-zoom-value" aria-live="polite">100%</span>' +
      '<button type="button" data-graph-action="in" aria-label="Zoom in" title="Zoom in">+</button>' +
      '<button type="button" data-graph-action="fit" title="Fit graph">Fit</button>' +
      '<select class="graph-orientation" aria-label="Graph arrangement" title="Graph arrangement">' +
      '<option value="horizontal"' + (layout.orientation === "horizontal" ? " selected" : "") + '>Horizontal</option>' +
      '<option value="vertical"' + (layout.orientation === "vertical" ? " selected" : "") + '>Vertical</option>' +
      '</select>' +
      '<span class="graph-help muted">Scroll to zoom · drag to pan</span>' +
      '</div>' +
      '<svg id="graph-svg" tabindex="0" role="img" aria-label="Workflow graph" viewBox="0 0 ' +
      layout.width + " " + layout.height + '" data-base-width="' + layout.width +
      '" data-base-height="' + layout.height + '" xmlns="http://www.w3.org/2000/svg"><g>' +
      svg + "</g></svg>" + renderGraphLegend() + "</div>";
  }

  function initGraphInteractions(graph, transitions, events, run, sourceId, runSequence) {
    const svg = document.getElementById("graph-svg");
    if (!svg) return;
    const panel = svg.closest(".graph-panel");
    const zoomValue = panel.querySelector(".graph-zoom-value");
    const base = {
      x: 0,
      y: 0,
      width: Number(svg.dataset.baseWidth),
      height: Number(svg.dataset.baseHeight),
    };
    let view = { ...base };
    let zoom = 1;
    let lastPointer = null;

    function renderView() {
      svg.setAttribute("viewBox", [view.x, view.y, view.width, view.height].join(" "));
      zoomValue.textContent = Math.round(zoom * 100) + "%";
    }

    function toGraphPoint(clientX, clientY) {
      const point = svg.createSVGPoint();
      point.x = clientX;
      point.y = clientY;
      const matrix = svg.getScreenCTM();
      return matrix ? point.matrixTransform(matrix.inverse()) : {
        x: view.x + view.width / 2,
        y: view.y + view.height / 2,
      };
    }

    function setZoom(nextZoom, clientX, clientY) {
      const clamped = Math.min(8, Math.max(0.5, nextZoom));
      if (clamped === zoom) return;
      const anchor = clientX === undefined
        ? { x: view.x + view.width / 2, y: view.y + view.height / 2 }
        : toGraphPoint(clientX, clientY);
      const ratioX = (anchor.x - view.x) / view.width;
      const ratioY = (anchor.y - view.y) / view.height;
      const width = base.width / clamped;
      const height = base.height / clamped;
      view = {
        x: anchor.x - ratioX * width,
        y: anchor.y - ratioY * height,
        width,
        height,
      };
      zoom = clamped;
      renderView();
    }

    panel.querySelector('[data-graph-action="in"]').addEventListener("click", () => setZoom(zoom * 1.25));
    panel.querySelector('[data-graph-action="out"]').addEventListener("click", () => setZoom(zoom / 1.25));
    panel.querySelector('[data-graph-action="fit"]').addEventListener("click", () => {
      view = { ...base };
      zoom = 1;
      renderView();
    });
    panel.querySelector(".graph-orientation").addEventListener("change", (event) => {
      graphOrientation = event.target.value === "vertical" ? "vertical" : "horizontal";
      const container = document.getElementById("graph-container");
      container.innerHTML = renderGraphSvg(graph, transitions, events, run, graphOrientation, selectedStageName);
      initGraphInteractions(graph, transitions, events, run, sourceId, runSequence);
    });

    const activateStage = (target) => inspectStage(target.dataset.stageName, run, sourceId, runSequence);
    svg.querySelectorAll(".stage-node").forEach((node) => {
      node.addEventListener("click", (event) => {
        event.stopPropagation();
        activateStage(node);
      });
      node.addEventListener("keydown", (event) => {
        if (event.key !== "Enter" && event.key !== " ") return;
        event.preventDefault();
        event.stopPropagation();
        activateStage(node);
      });
    });

    svg.addEventListener("wheel", (event) => {
      event.preventDefault();
      setZoom(zoom * Math.exp(-event.deltaY * 0.0015), event.clientX, event.clientY);
    }, { passive: false });

    svg.addEventListener("pointerdown", (event) => {
      if (event.button !== 0) return;
      if (event.target.closest(".stage-node")) return;
      svg.setPointerCapture(event.pointerId);
      lastPointer = { x: event.clientX, y: event.clientY };
      svg.classList.add("is-panning");
    });
    svg.addEventListener("pointermove", (event) => {
      if (!lastPointer || !svg.hasPointerCapture(event.pointerId)) return;
      const before = toGraphPoint(lastPointer.x, lastPointer.y);
      const after = toGraphPoint(event.clientX, event.clientY);
      view.x += before.x - after.x;
      view.y += before.y - after.y;
      lastPointer = { x: event.clientX, y: event.clientY };
      renderView();
    });
    function endPan(event) {
      if (svg.hasPointerCapture(event.pointerId)) svg.releasePointerCapture(event.pointerId);
      lastPointer = null;
      svg.classList.remove("is-panning");
    }
    svg.addEventListener("pointerup", endPan);
    svg.addEventListener("pointercancel", endPan);
    svg.addEventListener("dblclick", (event) => {
      if (!event.target.closest(".stage-node")) setZoom(zoom * 1.5, event.clientX, event.clientY);
    });
    svg.addEventListener("keydown", (event) => {
      if (event.key === "+" || event.key === "=") setZoom(zoom * 1.25);
      else if (event.key === "-") setZoom(zoom / 1.25);
      else if (event.key === "0") {
        view = { ...base };
        zoom = 1;
        renderView();
      } else {
        return;
      }
      event.preventDefault();
    });
  }

  function safeExternalUrl(value) {
    try {
      const url = new URL(value);
      return url.protocol === "http:" || url.protocol === "https:" ? url.href : "";
    } catch {
      return "";
    }
  }

  const configurationWarningKey = ${configurationWarningKey.toString()};
  const warningRemediation = ${warningRemediation.toString()};
  const sortConfigurationWarnings = ${sortConfigurationWarnings.toString()};
  const groupConfigurationWarnings = ${groupConfigurationWarnings.toString()};
  const renderConfigurationWarnings = ${renderConfigurationWarnings.toString()
        .replaceAll("escapeWarningHtml", "escapeHtml")};
  const gooberAvatar = ${gooberAvatar.toString()};
  const renderGooberChip = ${renderGooberChip.toString()
    .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderFullRunId = ${renderFullRunId.toString()
    .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderRunIdControl = ${renderRunIdControl.toString()
    .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderRunAssociations = ${renderRunAssociations.toString()
        .replaceAll("safeAssociationUrl", "safeExternalUrl")
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderFleetPortalLink = ${renderFleetPortalLink.toString()
        .replaceAll("safeAssociationUrl", "safeExternalUrl")
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const updateFleetPanel = ${updateFleetPanel.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderSnapshotCard = ${renderSnapshotCard.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderRunRowCells = ${renderRunRowCells.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderRunDetailSummary = ${renderRunDetailSummary.toString()
        .replaceAll("formatRunDetailTime", "fmtTime")
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const stageKindLabel = ${stageKindLabel.toString()};
  const stageActor = ${stageActor.toString()};
  const stageProperty = ${stageProperty.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderStageDefinitionInspector = ${renderStageDefinitionInspector.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderStageInspectorStatus = ${renderStageInspectorStatus.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderRunEventItems = ${renderRunEventItems.toString()
        .replaceAll("formatRunDetailTime", "fmtTime")
        .replaceAll("safeAssociationUrl", "safeExternalUrl")
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderTransitions = ${renderTransitions.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderOperatorPanel = ${renderOperatorPanel.toString()
        .replaceAll("safeAssociationUrl", "safeExternalUrl")
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const BOOLEAN_FILTER_KEYS = new Set(["showNoWork"]);
  const normalizeViewFilters = ${normalizeViewFilters.toString()};
  const encodeViewState = ${encodeViewState.toString()};
  const decodeViewState = ${decodeViewState.toString()};
  const createSnapshotFetcher = ${createSnapshotFetcher.toString()};
  const fetchSourceSnapshot = createSnapshotFetcher(fetch);
  const decodeStreamEvent = ${decodeStreamEvent.toString()};
  const mergeRunPage = ${mergeRunPage.toString()};
  const isInvalidCursorError = ${isInvalidCursorError.toString()};
  const shouldApplyRestoredFilters = ${shouldApplyRestoredFilters.toString()};
  const deriveFreshnessState = ${deriveFreshnessState.toString()};
  const asString = ${asString.toString()};
  const deriveAttemptLineage = ${deriveAttemptLineage.toString()};
  const deriveFailureBreadcrumbs = ${deriveFailureBreadcrumbs.toString()};
  const numericValue = ${numericValue.toString()};
  const explicitMeasure = ${explicitMeasure.toString()};
  const measureFromPayload = ${measureFromPayload.toString()};
  const deriveTelemetryInsights = ${deriveTelemetryInsights.toString()};
  const filterTranscriptEntries = ${filterTranscriptEntries.toString()};
  const renderGraphLegend = ${renderGraphLegend.toString()};
  const renderCausalDiagnosis = ${renderCausalDiagnosis.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderExecutionWaterfall = ${renderExecutionWaterfall.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderTelemetryInsights = ${renderTelemetryInsights.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};

  function externalRefsFrom(events) {
    const refs = [];
    const seen = new Set();
    for (const event of events || []) {
      const ref = event.externalRef;
      if (!ref) continue;
      const key = [ref.provider, ref.kind, ref.id, ref.url].join("|");
      if (seen.has(key)) continue;
      seen.add(key);
      refs.push(ref);
    }
    return refs;
  }

  function renderExternalRefs(refs) {
    const linked = (refs || []).filter((ref) => safeExternalUrl(ref.url));
    if (!linked.length) return "";
    return '<div class="external-refs">' + linked.map((ref) => {
      const label = (ref.provider || "external") + " " + (ref.kind || "ref") + " #" + (ref.id || "");
      return '<a href="' + escapeHtml(safeExternalUrl(ref.url)) + '" target="_blank" rel="noopener noreferrer">' + escapeHtml(label) + "</a>";
    }).join("") + "</div>";
  }

  function renderRunEvents(events, sourceId, runId) {
    if (!events || !events.length) return '<p class="muted">No events recorded.</p>';
    const transcriptEvents = events.filter((event) => {
      const name = String(event?.name || "").toLowerCase();
      return name.includes("transcript") || event?.role || event?.stage || event?.attempt !== undefined;
    });
    const transcriptFilterHtml = transcriptEvents.length ? '<div class="filters-bar" aria-label="Transcript filters">' +
      '<input type="text" data-transcript-filter="stage" placeholder="Stage" />' +
      '<input type="text" data-transcript-filter="role" placeholder="Role" />' +
      '<input type="number" min="1" data-transcript-filter="attempt" placeholder="Attempt" />' +
      '<input type="text" data-transcript-filter="text" placeholder="Text" />' +
      '</div>' : '';
    const displayedEvents = transcriptEvents.length ? transcriptEvents : events;
    const eventList = renderRunEventItems(displayedEvents, sourceId, runId);
    return transcriptFilterHtml + '<div class="event-list" id="transcript-events">' + eventList + '</div>';
  }

  function initTranscriptFilters(events, sourceId, runId) {
    const transcriptEvents = events.filter((event) => {
      const name = String(event?.name || "").toLowerCase();
      return name.includes("transcript") || event?.role || event?.stage || event?.attempt !== undefined;
    });
    const controls = runContentEl.querySelectorAll("[data-transcript-filter]");
    const list = runContentEl.querySelector("#transcript-events");
    if (!controls.length || !list || !transcriptEvents.length) return;
    const update = () => {
      const filters = {};
      controls.forEach((input) => { filters[input.dataset.transcriptFilter] = input.value; });
      list.innerHTML = renderRunEventItems(filterTranscriptEntries(transcriptEvents, filters), sourceId, runId);
    };
    controls.forEach((input) => input.addEventListener("input", update));
  }

  function runActionKey(sourceId, runId) {
    return sourceId + "/" + runId;
  }

  function runActionConfirmationMarkup(pending) {
    return '<strong>Confirm:</strong> ' + escapeHtml(pending.effect) + '. ' +
      'Actor: <code>' + escapeHtml(pending.actor) + '</code>. ' +
      (pending.rationale ? 'Rationale: <q>' + escapeHtml(pending.rationale) + '</q>. ' : '') +
      (pending.instructionAddendum ? 'Addendum: <q>' + escapeHtml(pending.instructionAddendum) + '</q>. ' : '') +
      '<button id="run-action-confirm" type="button"' + (pending.sending ? " disabled" : "") + ">" +
      (pending.sending ? "Sending\u2026" : "Confirm and send") + "</button>";
  }

  function runActionPanel(run, events, sourceId) {
    const available = lastCapabilities.revealRun ? ["approve", "override", "rerun"] : [];
    if (!available.length) return "";
    const currentStage = run.currentStage || [...events].reverse().find((event) => event.stage)?.stage || "";
    const journalSeq = run.journalSeq || [...events].reverse().find((event) => Number(event.seq) > 0)?.seq || "unavailable";
    const pending = pendingRunActions.get(runActionKey(sourceId, run.id));
    const confirmation = pending ? runActionConfirmationMarkup(pending) : "";
    return '<section class="run-actions"><h2>Safe operator actions</h2>' +
      '<p>Run <code>' + escapeHtml(run.id) + '</code>, stage <code>' + escapeHtml(currentStage || "unknown") +
      '</code>, journal sequence <code>' + escapeHtml(journalSeq) + '</code>.</p>' +
      '<div class="run-actions-grid">' +
      '<label>Action<select id="run-action">' + available.map((action) => '<option value="' + action + '"' +
      (pending?.action === action ? " selected" : "") + ">" + action + "</option>").join("") + '</select></label>' +
      '<label>Actor<input id="run-action-actor" type="text" autocomplete="username" value="' + escapeHtml(pending?.actor || "") + '" /></label>' +
      '<label>Rationale (override)<input id="run-action-rationale" type="text" value="' + escapeHtml(pending?.rationale || "") + '" /></label>' +
      '<label>Instruction addendum (rerun)<input id="run-action-addendum" type="text" value="' + escapeHtml(pending?.instructionAddendum || "") + '" /></label>' +
      '<button id="run-action-review" type="button">Review action</button></div>' +
      '<div id="run-action-confirmation" class="run-action-confirmation"' + (pending ? "" : " hidden") + ">" +
      confirmation + "</div></section>";
  }

  function initRunActions(run, events, sourceId) {
    const review = runContentEl.querySelector("#run-action-review");
    if (!review) return;
    const confirmation = runContentEl.querySelector("#run-action-confirmation");
    const actionInput = runContentEl.querySelector("#run-action");
    const actorInput = runContentEl.querySelector("#run-action-actor");
    const rationaleInput = runContentEl.querySelector("#run-action-rationale");
    const addendumInput = runContentEl.querySelector("#run-action-addendum");
    const actionKey = runActionKey(sourceId, run.id);
    const attachConfirm = () => {
      const confirm = runContentEl.querySelector("#run-action-confirm");
      if (!confirm) return;
      confirm.addEventListener("click", async () => {
        const pending = pendingRunActions.get(actionKey);
        if (!pending || pending.sending) return;
        pending.sending = true;
        confirm.disabled = true;
        runStatusEl.textContent = "Sending " + pending.action + "\u2026";
        try {
          const response = await fetch("/api/run-action", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              source: sourceId, action: pending.action, runId: run.id,
              stage: run.currentStage || [...events].reverse().find((event) => event.stage)?.stage,
              input: {
                actor: pending.actor,
                ...(pending.action === "approve" ? { decision: "pass" } : {}),
                ...(pending.action === "override" ? { rationale: pending.rationale } : {}),
                ...(pending.action === "rerun" ? { instructionAddendum: pending.instructionAddendum } : {}),
              },
            }),
          });
          const data = await response.json();
          if (!data.ok) throw new Error((data.code ? data.code + ": " : "") + (data.reason || "action failed"));
          if (!Number(data.result?.journalSeq)) throw new Error("The server did not confirm a durable journal position.");
          runStatusEl.textContent = pending.action + " accepted at journal sequence " + data.result.journalSeq + ".";
          pendingRunActions.delete(actionKey);
          await openRun(run.id);
          runContentEl.querySelector("#run-action")?.focus();
        } catch (err) {
          runStatusEl.textContent = "Action failed: " + (err.message || err);
          pending.sending = false;
          runContentEl.querySelector("#run-action-confirm")?.removeAttribute("disabled");
        }
      });
    };
    attachConfirm();
    review.addEventListener("click", () => {
      const action = actionInput.value;
      const actor = actorInput.value.trim();
      const rationale = rationaleInput.value.trim();
      const instructionAddendum = addendumInput.value.trim();
      if (!actor || (action === "override" && !rationale) || (action === "rerun" && !instructionAddendum)) {
        runStatusEl.textContent = "Enter an actor and the required rationale or addendum.";
        return;
      }
      const effect = action === "approve" ? "approve the stage with decision=pass" :
        action === "override" ? "override the stage using the entered rationale" :
        "rerun the stage using the entered instruction addendum";
      pendingRunActions.set(actionKey, { action, actor, rationale, instructionAddendum, effect, sending: false });
      confirmation.hidden = false;
      confirmation.innerHTML = runActionConfirmationMarkup(pendingRunActions.get(actionKey));
      attachConfirm();
    });
  }

  async function openRun(runId) {
    const sourceId = sourceSelect.value;
    if (!sourceId) {
      errorEl.textContent = "Choose a source before opening a run.";
      sourceSelect.focus();
      return;
    }
    const requestSequence = ++runRequestSequence;
    const isNewRun = selectedRunId !== runId;
    selectedRunId = runId;
    if (isNewRun) {
      activeRunTab = "summary";
      activeStageInspectorView = "fields";
      selectedStageName = "";
      ++stageInspectorRequestSequence;
    }
    const activeFilter = document.activeElement?.closest("[data-transcript-filter]");
    const savedFilters = [...runContentEl.querySelectorAll("[data-transcript-filter]")].map((input) => ({
      name: input.dataset.transcriptFilter,
      value: input.value,
      selectionStart: input.selectionStart,
      selectionEnd: input.selectionEnd,
    }));
    syncViewUrl(runId);
    dashboardEl.style.display = "none";
    runViewEl.style.display = "block";
    runErrorEl.textContent = "";
    runContentEl.innerHTML = '<p class="muted">Loading run\\u2026</p>';
    try {
      const res = await fetch("/api/run?source=" + encodeURIComponent(sourceId) + "&id=" + encodeURIComponent(runId));
      const data = await res.json();
      if (sourceId !== sourceSelect.value || requestSequence !== runRequestSequence) return;
      if (!data.connected) {
        runErrorEl.textContent = data.reason || "Run unavailable.";
        runContentEl.innerHTML = "";
        return;
      }
      const r = data.run;
      const events = r.events || [];
      const refs = externalRefsFrom(events);
      const actionsRunUrl = safeExternalUrl(r.actionsRunUrl);
      const actionsLink = actionsRunUrl
        ? '<a class="actions-run-link" href="' + escapeHtml(actionsRunUrl) +
          '" target="_blank" rel="noopener noreferrer">View GitHub Action &#8599;</a>'
        : "";
      let html = renderRunDetailSummary(r, { actionsLink });
      if (r.operator) {
        html += "<h2>Operator</h2>" + renderOperatorPanel(r.operator, refs);
      }
      if (refs.length) {
        html += "<h2>Associated work</h2>" + renderExternalRefs(refs);
      }
      html += "<h2>Telemetry insights</h2>" + renderTelemetryInsights(r);
      html += '</section><section id="run-panel-execution" role="tabpanel" aria-labelledby="run-tab-execution" hidden>';
      html += '<h2>Workflow graph</h2><div class="stage-definition-layout"><div id="graph-container">' +
        renderGraphSvg(r.graph, r.transitions, events, r, graphOrientation, selectedStageName) +
        '</div><div id="stage-inspector">' +
        renderStageInspectorStatus("Select a stage to inspect its workflow definition.") +
        '</div><div id="stage-inspector-status" class="sr-only" aria-live="polite" aria-atomic="true"></div></div>';
      html += "<h2>Causal diagnosis</h2>" + renderCausalDiagnosis(r);
      html += "<h2>Execution waterfall</h2>" + renderExecutionWaterfall(r);
      html += "<h2>Transitions</h2>" + renderTransitions(r.transitions);
      html += '</section><section id="run-panel-diagnostics" role="tabpanel" aria-labelledby="run-tab-diagnostics" hidden>';
      html += "<h2>Events, logs, and messages</h2>" + renderRunEvents(events, sourceId, runId);
      html += '</section><section id="run-panel-actions" role="tabpanel" aria-labelledby="run-tab-actions" hidden>' +
        (runActionPanel(r, events, sourceId) || '<p class="muted">No operator actions are available for this source.</p>') +
        "</section>";
      runContentEl.innerHTML = html;
      initInternalTabs(runContentEl, activeRunTab);
      attachRunIdControls(runContentEl);
      if (isNewRun) document.getElementById("run-back").focus();
      savedFilters.forEach((saved) => {
        const input = runContentEl.querySelector('[data-transcript-filter="' + saved.name + '"]');
        if (!input) return;
        input.value = saved.value;
        if (saved.selectionStart !== null && saved.selectionEnd !== null) {
          input.setSelectionRange(saved.selectionStart, saved.selectionEnd);
        }
      });
      initTranscriptFilters(events, sourceId, runId);
      initRunActions(r, events, sourceId);
      if (activeFilter?.dataset.transcriptFilter) {
        runContentEl.querySelector('[data-transcript-filter="' + activeFilter.dataset.transcriptFilter + '"]')?.focus();
      }
      const restoredFilterValues = {};
      savedFilters.forEach((saved) => { restoredFilterValues[saved.name] = saved.value; });
      if (savedFilters.length) {
        const list = runContentEl.querySelector("#transcript-events");
        const transcriptEvents = events.filter((event) => {
          const name = String(event?.name || "").toLowerCase();
          return name.includes("transcript") || event?.role || event?.stage || event?.attempt !== undefined;
        });
        if (list && transcriptEvents.length) {
          list.innerHTML = renderRunEventItems(filterTranscriptEntries(transcriptEvents, restoredFilterValues), sourceId, runId);
        }
      }
      initGraphInteractions(r.graph, r.transitions, events, r, sourceId, requestSequence);
      if (selectedStageName) inspectStage(selectedStageName, r, sourceId, requestSequence);
    } catch (err) {
      if (sourceId !== sourceSelect.value || requestSequence !== runRequestSequence) return;
      runErrorEl.textContent = String(err);
      runContentEl.innerHTML = "";
    }
  }

  document.getElementById("run-back").addEventListener("click", () => {
    const previousRunId = selectedRunId;
    selectedRunId = "";
    ++runRequestSequence;
    ++stageInspectorRequestSequence;
    selectedStageName = "";
    activeStageInspectorView = "fields";
    runViewEl.style.display = "none";
    dashboardEl.style.display = "block";
    activateInternalTab(dashboardEl, "runs", true);
    const row = [...runsBody.querySelectorAll("[data-run-id]")].find((row) => row.dataset.runId === previousRunId);
    row?.querySelector(".table-link")?.focus();
    syncViewUrl();
  });

  async function loadSnapshot() {
    const sourceId = sourceSelect.value;
    if (snapshotSourceId !== sourceId) {
      ++sourceSelectionEpoch;
      workflowRunRequests.clear();
      updateFleetPanel({});
      if (snapshotSourceId !== null) {
        restoredRunId = "";
        selectedRunId = "";
        ++runRequestSequence;
        ++stageInspectorRequestSequence;
        selectedStageName = "";
        activeStageInspectorView = "fields";
        ++filterRequestSequence;
        runViewEl.style.display = "none";
        runContentEl.innerHTML = "";
        runErrorEl.textContent = "";
        runStatusEl.textContent = "";
        setRunStatus("");
        dashboardEl.style.display = "none";
        document.getElementById("source-context").textContent = "";
        lastRuns = [];
        lastCapabilities = {};
        lastUpdatedAt = null;
        pendingToggles.clear();
        pendingWorkflowRuns.clear();
        workflowUndo.clear();
        dismissedAttention.clear();
        syncViewUrl();
        setFreshnessState("Loading");
      }
      snapshotSourceId = sourceId;
    }
    const requestSequence = ++snapshotRequestSequence;
    if (!sourceId) {
      updateFleetPanel({});
      emptyEl.style.display = "block";
      dashboardEl.style.display = "none";
      startBarEl.style.display = "none";
      errorEl.textContent = "";
      return;
    }
    try {
      const data = await fetchSnapshot();
      if (sourceId !== sourceSelect.value || requestSequence !== snapshotRequestSequence) return;
      if (data) renderSnapshot(data);
    } catch (err) {
      if (sourceId !== sourceSelect.value || requestSequence !== snapshotRequestSequence) return;
      errorEl.textContent = portalRequestError(err);
    }
  }

  /** Fetch the snapshot for the selected source without rendering it. */
  async function fetchSnapshot() {
    const sourceId = sourceSelect.value;
    if (!sourceId) return null;
    return await fetchSourceSnapshot(sourceId);
  }

  function setFreshnessState(state, freshAt = null) {
    const timestamp = freshAt || lastUpdatedAt;
    const timeStr = timestamp ? fmtTime(timestamp) : "never";
    freshnessEl.dataset.freshness = String(state || "").toLowerCase();
    freshnessEl.textContent = state + " · Updated " + timeStr;
  }

  async function refreshAll() {
    await loadSources();
    await loadSnapshot();
  }

  function connectLiveEvents() {
    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    if (freshnessTimer) {
      clearInterval(freshnessTimer);
      freshnessTimer = null;
    }
    if (eventSource) {
      eventSource.close();
      eventSource = null;
    }
    const sourceId = sourceSelect.value;
    const mode = sourceSelect.selectedOptions[0]?.dataset.kind;
    if (!sourceId || mode !== "local" && mode !== "remote") return;

    function scheduleReconnect() {
      if (reconnectTimer) return;
      const delay = Math.min(10000, 1000 * Math.pow(2, reconnectAttemptCount));
      reconnectAttemptCount++;
      setFreshnessState("Reconnecting");
      reconnectTimer = window.setTimeout(() => {
        reconnectTimer = null;
        if (sourceSelect.value === sourceId && !eventSource) {
          connectLiveEvents();
        }
      }, delay);
    }

    function startFreshnessTimer() {
      if (freshnessTimer) clearInterval(freshnessTimer);
      freshnessTimer = setInterval(() => {
        const freshness = deriveFreshnessState({
          lastUpdatedAt,
          connected: true,
          mode: mode === "remote" ? "polling" : "daemon",
          now: Date.now(),
        });
        setFreshnessState(freshness, lastUpdatedAt);
      }, 1000);
    }

    function stopFreshnessTimer() {
      if (freshnessTimer) {
        clearInterval(freshnessTimer);
        freshnessTimer = null;
      }
    }

    eventSource = new EventSource("/api/events?source=" + encodeURIComponent(sourceId));
    eventSource.onopen = () => {
      const wasReconnect = liveConnectionEstablished;
      reconnectAttemptCount = 0;
      liveConnectionEstablished = true;
      lastUpdatedAt = Date.now();
      const freshness = deriveFreshnessState({
        lastUpdatedAt,
        connected: true,
        mode: mode === "remote" ? "polling" : "daemon",
        now: Date.now(),
      });
      setFreshnessState(freshness, lastUpdatedAt);
      startFreshnessTimer();
      if (wasReconnect) void loadSnapshot();
    };
    eventSource.onmessage = (event) => {
      if (!decodeStreamEvent(event.data)) return;
      lastUpdatedAt = Date.now();
      const freshness = deriveFreshnessState({
        lastUpdatedAt,
        connected: true,
        mode: mode === "remote" ? "polling" : "daemon",
        now: Date.now(),
      });
      setFreshnessState(freshness, lastUpdatedAt);
      void loadSnapshot();
    };
    eventSource.onerror = () => {
      if (eventSource) {
        eventSource.close();
        eventSource = null;
      }
      stopFreshnessTimer();
      scheduleReconnect();
    };
  }

  document.getElementById("refresh").addEventListener("click", refreshAll);
  async function changeSource() {
    liveConnectionEstablished = false;
    reconnectAttemptCount = 0;
    if (eventSource) eventSource.close();
    await loadSnapshot();
    connectLiveEvents();
  }
  sourceSelect.addEventListener("change", () => void changeSource());
  function jumpToRun() {
    const runId = runJumpInput.value.trim();
    if (!runId) return;
    restoredRunId = runId;
    syncViewUrl(runId);
    void openRun(runId);
  }
  runJumpButton.addEventListener("click", jumpToRun);
  runJumpInput.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      jumpToRun();
    }
  });

  async function connectSource(payload) {
    errorEl.textContent = "";
    const response = await fetch("/api/add-source", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload),
    });
    const result = await response.json();
    if (!response.ok || result.error) throw new Error(result.error || "Could not add source.");
    await loadSources();
    sourceSelect.value = result.id;
    await changeSource();
    document.getElementById("add-source-details").open = false;
  }

  async function openDirectory(directory) {
    const query = directory ? "?path=" + encodeURIComponent(directory) : "";
    const response = await fetch("/api/directories" + query);
    const data = await response.json();
    if (!response.ok || data.error) throw new Error(data.error || "Could not browse directories.");
    directoryCurrent.value = data.current;
    directoryParent.disabled = !data.parent;
    directoryParent.dataset.path = data.parent || "";
    directoryRoots.innerHTML = "";
    for (const root of data.roots || []) {
      const option = document.createElement("option");
      option.value = root;
      option.textContent = root;
      if (data.current.toLowerCase().startsWith(root.toLowerCase())) option.selected = true;
      directoryRoots.appendChild(option);
    }
    directoryList.innerHTML = "";
    for (const entry of data.directories || []) {
      const button = document.createElement("button");
      button.className = "directory-entry";
      button.textContent = "\uD83D\uDCC1 " + entry.name;
      button.addEventListener("click", () => openDirectory(entry.path).catch(showDirectoryError));
      directoryList.appendChild(button);
    }
    if (!(data.directories || []).length) {
      const empty = document.createElement("div");
      empty.className = "muted";
      empty.textContent = "No subfolders";
      directoryList.appendChild(empty);
    }
  }

  function showDirectoryError(err) {
    errorEl.textContent = portalRequestError(err);
  }

  document.getElementById("browse-local").addEventListener("click", async () => {
    try {
      await openDirectory(document.getElementById("local-root").value.trim());
      directoryDialog.showModal();
    } catch (err) {
      showDirectoryError(err);
    }
  });
  directoryParent.addEventListener("click", () =>
    openDirectory(directoryParent.dataset.path).catch(showDirectoryError));
  directoryRoots.addEventListener("change", () =>
    openDirectory(directoryRoots.value).catch(showDirectoryError));
  document.getElementById("directory-cancel").addEventListener("click", () => directoryDialog.close());
  document.getElementById("directory-choose").addEventListener("click", () => {
    document.getElementById("local-root").value = directoryCurrent.value;
    directoryDialog.close();
  });

  document.getElementById("add-local").addEventListener("click", async () => {
    const root = document.getElementById("local-root").value.trim();
    if (!root) return;
    try {
      await connectSource({ kind: "local", value: root });
      document.getElementById("local-root").value = "";
    } catch (err) {
      errorEl.textContent = portalRequestError(err);
    }
  });

  document.getElementById("add-remote").addEventListener("click", async () => {
    const url = document.getElementById("remote-url").value.trim();
    if (!url) return;
    const token = document.getElementById("remote-token").value.trim();
    try {
      await connectSource({ kind: "remote", value: url, token: token || undefined });
      document.getElementById("remote-url").value = "";
      document.getElementById("remote-token").value = "";
    } catch (err) {
      errorEl.textContent = portalRequestError(err);
    }
  });

  document.getElementById("add-github").addEventListener("click", async () => {
    const input = document.getElementById("github-workflow-url");
    const url = input.value.trim();
    if (!url) return;
    try {
      await connectSource({ kind: "github-actions", value: url });
      input.value = "";
    } catch (err) {
      errorEl.textContent = portalRequestError(err);
    }
  });

  async function poll() {
    try {
      await refreshAll();
      if (!eventSource) connectLiveEvents();
    } catch (err) {
      errorEl.textContent = portalRequestError(err);
    } finally {
      const kind = sourceSelect.selectedOptions[0]?.dataset.kind;
      setTimeout(poll, kind === "github-actions" ? 30000 : 5000);
    }
  }
  poll();
})();
</script>
</body>
</html>`;
}
