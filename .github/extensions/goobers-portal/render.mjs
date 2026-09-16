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
    const className = options.error ? "stage-inspector-state stage-inspector-error" : "stage-inspector-state";
    return '<div class="' + className + '">' + escapeAssociationHtml(message) + "</div>";
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
        const hasBody = Boolean(refHtml || detailsHtml || linksHtml);
        if (!hasBody) {
            return '<div class="event-row event-row-flat">' +
                summary.replace(/^<summary>/, '<span class="event-summary-line">').replace(/<\/summary>$/, "</span>") +
                "</div>";
        }
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

// ---------------------------------------------------------------------------
// Insights tab: aggregate telemetry (GET /api/v1/telemetry/stats), ported
// from the web portal's InsightPage.tsx/insightScope.ts/insightData.ts —
// behavior and data shape, not the React implementation. Scoped by
// instance/gaggle/workflow (no stage-level scope, unlike the web portal) and
// a bounded time window matching INSIGHT_WINDOWS exactly.
// ---------------------------------------------------------------------------

export const INSIGHT_WINDOWS = [
    { value: "24h", label: "Last 24 hours" },
    { value: "7d", label: "Last 7 days" },
    { value: "30d", label: "Last 30 days" },
    { value: "all", label: "All time" },
];

const INSIGHT_WINDOW_MS = {
    "24h": 24 * 60 * 60 * 1000,
    "7d": 7 * 24 * 60 * 60 * 1000,
    "30d": 30 * 24 * 60 * 60 * 1000,
};

// One network round trip per bucket (there is no bucketed telemetry
// endpoint), so bucket counts are fixed per window rather than
// one-bucket-per-hour/day — matches the portal's TREND_BUCKET_COUNTS.
const INSIGHT_TREND_BUCKET_COUNTS = { "24h": 8, "7d": 7, "30d": 10 };

export function insightWindowRange(windowValue, now = new Date()) {
    const until = now.toISOString();
    const durationMs = INSIGHT_WINDOW_MS[windowValue];
    return durationMs ? { since: new Date(now.getTime() - durationMs).toISOString(), until } : { until };
}

/** The window of the same length immediately preceding the selected one. Undefined for "all". */
export function insightPreviousWindowRange(windowValue, now = new Date()) {
    const durationMs = INSIGHT_WINDOW_MS[windowValue];
    if (!durationMs) return undefined;
    const currentSince = now.getTime() - durationMs;
    return {
        since: new Date(currentSince - durationMs).toISOString(),
        until: new Date(currentSince).toISOString(),
    };
}

export function parseInsightScope(value) {
    if (!value || value === "instance") return { kind: "instance" };
    if (value.startsWith("gaggle:")) {
        return { kind: "gaggle", gaggle: value.slice("gaggle:".length) };
    }
    if (value.startsWith("workflow:")) {
        const rest = value.slice("workflow:".length);
        const separator = rest.indexOf("|");
        if (separator === -1) return { kind: "instance" };
        return { kind: "workflow", gaggle: rest.slice(0, separator), workflow: rest.slice(separator + 1) };
    }
    return { kind: "instance" };
}

export function insightScopeValue(scope) {
    if (scope.kind === "gaggle") return "gaggle:" + scope.gaggle;
    if (scope.kind === "workflow") return "workflow:" + scope.gaggle + "|" + scope.workflow;
    return "instance";
}

export function insightScopeLabel(scope) {
    if (scope.kind === "gaggle") return "Gaggle \u00b7 " + scope.gaggle;
    if (scope.kind === "workflow") return "Workflow \u00b7 " + scope.gaggle + " / " + scope.workflow;
    return "Instance";
}

export function insightScopeApiParams(scope) {
    if (scope.kind === "gaggle") return { gaggle: scope.gaggle };
    if (scope.kind === "workflow") return { gaggle: scope.gaggle, workflow: scope.workflow };
    return {};
}

/** Builds the scope <select> option list from the most recently loaded stats. */
export function insightScopeOptionsFromStats(stats = {}) {
    const options = [{ value: "instance", label: "Instance" }];
    for (const item of stats.gaggles || []) {
        const scope = { kind: "gaggle", gaggle: item.gaggle };
        options.push({ value: insightScopeValue(scope), label: insightScopeLabel(scope) });
    }
    for (const item of stats.runs || []) {
        const scope = { kind: "workflow", gaggle: item.gaggle, workflow: item.workflow };
        options.push({ value: insightScopeValue(scope), label: insightScopeLabel(scope) });
    }
    return options;
}

/** The full options object to pass to loadInsightStats for a (scope, window) pair. */
export function insightRequestParams(scope, windowValue, now = new Date()) {
    const range = insightWindowRange(windowValue, now);
    const params = { ...insightScopeApiParams(scope), until: range.until };
    if (range.since !== undefined) params.since = range.since;
    if (windowValue === "all") return params;
    const previous = insightPreviousWindowRange(windowValue, now);
    const bucketCount = INSIGHT_TREND_BUCKET_COUNTS[windowValue] || 1;
    return {
        ...params,
        trendSince: previous.since,
        trendUntil: range.until,
        trendBuckets: bucketCount * 2,
        trendPreviousSince: previous.since,
        trendPreviousUntil: previous.until,
    };
}

export function isInInsightScope(scope, identity) {
    if (scope.kind === "instance") return true;
    if (identity.gaggle !== scope.gaggle) return false;
    if (scope.kind === "gaggle") return true;
    return identity.workflow === scope.workflow;
}

export function insightUsageForScope(stats, scope) {
    return (stats.usage || []).find((item) => item.scope === scope.kind && isInInsightScope(scope, item));
}

function insightFormatRate(value) {
    return value === undefined ? "Unmeasured" : (value * 100).toFixed(1) + "%";
}

function insightFormatDuration(ms) {
    if (ms === undefined || ms === null) return "Unmeasured";
    return ms < 1000 ? Math.round(ms) + "ms" : (ms / 1000).toFixed(1) + "s";
}

function insightFormatTokens(value) {
    return value === undefined ? "Unmeasured" : value.toLocaleString("en-US") + " tokens";
}

function insightFormatCost(value) {
    return value === undefined ? "Unmeasured" : "$" + value.toFixed(2);
}

function insightFormatSamples(samples) {
    return !samples ? "Unmeasured" : samples + (samples === 1 ? " sample" : " samples");
}

function insightFormatBucketLabel(since, until) {
    const start = new Date(since);
    const end = new Date(until);
    if (Number.isNaN(start.getTime()) || Number.isNaN(end.getTime())) return since + " \u2013 " + until;
    return start.toLocaleString() + " \u2013 " + end.toLocaleString();
}

function insightGaggleMetric(item) {
    return {
        label: item.gaggle,
        successRate: item.successRate,
        succeeded: item.completedRuns,
        failed: item.failedRuns,
        other: item.otherRuns,
        total: item.totalRuns,
    };
}

function insightRunMetric(item) {
    return {
        label: item.gaggle + " / " + item.workflow,
        successRate: item.successRate,
        succeeded: item.completedRuns,
        failed: item.failedRuns,
        other: item.otherRuns,
        total: item.totalRuns,
    };
}

function insightStageOutcomeMetric(item) {
    return {
        label: item.gaggle + " / " + item.workflow + " / " + item.stage,
        successRate: item.successRate,
        succeeded: item.succeededAttempts,
        failed: item.failedAttempts,
        other: item.totalAttempts - item.succeededAttempts - item.failedAttempts,
        total: item.totalAttempts,
    };
}

// Recomputes the instance-wide success rate client-side rather than reading
// any single server field, so it must apply the same denominator rule the
// daemon uses per-gaggle: an infra-fault terminal is not a verdict about the
// work and is excluded from both the numerator and denominator.
function insightSumGaggles(gaggles) {
    if (!gaggles || !gaggles.length) return null;
    const total = gaggles.reduce((sum, item) => ({
        completed: sum.completed + item.completedRuns,
        failed: sum.failed + item.failedRuns,
        infraFailed: sum.infraFailed + item.infraFailedRuns,
        other: sum.other + item.otherRuns,
        runs: sum.runs + item.totalRuns,
    }), { completed: 0, failed: 0, infraFailed: 0, other: 0, runs: 0 });
    const terminal = total.completed + total.failed - total.infraFailed;
    return {
        label: "Instance",
        successRate: terminal > 0 ? total.completed / terminal : undefined,
        succeeded: total.completed,
        failed: total.failed,
        other: total.other,
        total: total.runs,
    };
}

function insightOutcomeSummary(stats, scope) {
    if (scope.kind === "instance") return insightSumGaggles(stats.gaggles || []);
    if (scope.kind === "gaggle") {
        const item = (stats.gaggles || []).find((g) => g.gaggle === scope.gaggle);
        return item ? insightGaggleMetric(item) : null;
    }
    const item = (stats.runs || []).find((r) => r.gaggle === scope.gaggle && r.workflow === scope.workflow);
    return item ? insightRunMetric(item) : null;
}

function insightOutcomeBreakdown(stats, scope) {
    if (scope.kind === "instance") return (stats.gaggles || []).map(insightGaggleMetric);
    if (scope.kind === "gaggle") {
        return (stats.runs || []).filter((r) => r.gaggle === scope.gaggle).map(insightRunMetric);
    }
    return (stats.stages || [])
        .filter((s) => s.gaggle === scope.gaggle && s.workflow === scope.workflow)
        .map(insightStageOutcomeMetric);
}

function renderInsightOutcomeRow(metric, emphasis) {
    return '<tr' + (emphasis ? ' class="insight-outcome-summary"' : "") + '>' +
        "<td>" + escapeAssociationHtml(metric.label) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatRate(metric.successRate)) + "</td>" +
        "<td>" + escapeAssociationHtml(metric.succeeded) + "</td>" +
        "<td>" + escapeAssociationHtml(metric.failed) + "</td>" +
        "<td>" + escapeAssociationHtml(metric.other) + "</td>" +
        "<td>" + escapeAssociationHtml(metric.total) + "</td></tr>";
}

function renderInsightOutcomeSection(stats, scope) {
    const summary = insightOutcomeSummary(stats, scope);
    const breakdown = insightOutcomeBreakdown(stats, scope);
    if (!summary && !breakdown.length) return "";
    const rows = (summary ? renderInsightOutcomeRow(summary, true) : "") +
        breakdown.map((metric) => renderInsightOutcomeRow(metric, false)).join("");
    return '<section class="content-section"><h3>Success and failure</h3>' +
        '<div class="table-scroll"><table><thead><tr>' +
        "<th>Scope</th><th>Success rate</th><th>Succeeded</th><th>Failed</th><th>Other</th><th>Total</th>" +
        "</tr></thead><tbody>" + rows + "</tbody></table></div></section>";
}

function insightUnmeasuredLabel(everRecorded) {
    return everRecorded ? "No data in window" : "Never recorded";
}

function insightFormatSeconds(value, everRecorded) {
    return value === undefined ? insightUnmeasuredLabel(everRecorded) : insightFormatDuration(value * 1000);
}

function insightHasCurationHealth(stats, scope) {
    if (scope.kind !== "instance") return false;
    const curation = stats.curation || {};
    const readyPool = stats.readyPool || {};
    return Boolean(
        curation.runs > 0 ||
        readyPool.depth !== undefined ||
        curation.everRecorded ||
        readyPool.sampleEverRecorded ||
        readyPool.bounceEverRecorded,
    );
}

function renderInsightCurationSection(stats, scope) {
    if (!insightHasCurationHealth(stats, scope)) return "";
    const curation = stats.curation || {};
    const readyPool = stats.readyPool || {};
    const depthLabel = readyPool.depth === undefined
        ? insightUnmeasuredLabel(readyPool.sampleEverRecorded)
        : readyPool.starved ? "0 \u00b7 Starved" : String(readyPool.depth);
    const bounceLabel = readyPool.bounceRate === undefined
        ? insightUnmeasuredLabel(readyPool.bounceEverRecorded)
        : (readyPool.bounceRate * 100).toFixed(1) + "%";
    const inFlightLabel = !readyPool.inFlightClaimSamples
        ? "0"
        : insightFormatDuration(readyPool.averageInFlightClaimAgeSeconds * 1000) +
            " average \u00b7 " + readyPool.inFlightClaimSamples + " claimed";
    const throughputLabel = (curation.everRecorded ? readyPool.forwardCurationThroughput : insightUnmeasuredLabel(false)) +
        " / " + readyPool.implementationDemand;
    const actionsLabel = curation.everRecorded
        ? curation.ready + " ready \u00b7 " + curation.needsHuman + " needs human \u00b7 " + curation.closed + " closed"
        : insightUnmeasuredLabel(false);
    const rows = [
        ["Ready depth", depthLabel],
        ["Oldest ready", insightFormatSeconds(readyPool.oldestAgeSeconds, readyPool.sampleEverRecorded)],
        ["Age before claim", insightFormatSeconds(readyPool.averageClaimAgeSeconds, true)],
        ["In flight now", inFlightLabel],
        ["Bounce rate", bounceLabel],
        ["Throughput / demand", throughputLabel],
        ["Curation actions", actionsLabel],
    ];
    const items = rows.map(([label, value]) =>
        '<div class="kv"><div class="label">' + escapeAssociationHtml(label) +
            '</div><div class="value">' + escapeAssociationHtml(value) + "</div></div>",
    ).join("");
    return '<section class="content-section"><h3>Ready-pool health</h3><div class="kv-grid">' + items + "</div></section>";
}

function renderInsightCreditSection(stats, scope) {
    const credits = (stats.creditAssignment || []).filter((credit) => isInInsightScope(scope, credit));
    if (!credits.length) return "";
    const visible = credits.slice(0, 10);
    const rows = visible.map((credit) =>
        "<tr><td>" + escapeAssociationHtml(credit.gaggle + " / " + credit.workflow + " / " + credit.stage) + "</td>" +
        "<td>" + escapeAssociationHtml(credit.kind) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatRate(credit.failureShare)) + "</td>" +
        "<td>" + escapeAssociationHtml(credit.failureRuns) + "</td>" +
        "<td>" + escapeAssociationHtml(credit.escalationRuns) + "</td>" +
        "<td>" + escapeAssociationHtml(credit.retryWasteAttempts) + "</td></tr>",
    ).join("");
    const overflow = credits.length > visible.length
        ? '<p class="muted">+' + (credits.length - visible.length) + " more contributors.</p>"
        : "";
    return '<section class="content-section"><h3>Highest-contributing nodes</h3>' +
        '<div class="table-scroll"><table><thead><tr>' +
        "<th>Node</th><th>Kind</th><th>Failure share</th><th>Failures</th><th>Escalations</th><th>Retry waste</th>" +
        "</tr></thead><tbody>" + rows + "</tbody></table></div>" + overflow + "</section>";
}

function renderInsightUsageSection(stats, scope) {
    const usage = insightUsageForScope(stats, scope);
    if (!usage) return "";
    const rows = [
        ["Attempts", String(usage.totalAttempts)],
        ["Tokens (P50 / P95)", insightFormatTokens(usage.p50Tokens) + " / " + insightFormatTokens(usage.p95Tokens)],
        ["Cost total", insightFormatCost(usage.costUSD)],
        ["Cost (P50 / P95)", insightFormatCost(usage.p50CostUSD) + " / " + insightFormatCost(usage.p95CostUSD)],
        ["Samples", insightFormatSamples(usage.costSamples)],
        ["Retry waste", usage.retryWasteAttempts === 0
            ? "No retry waste"
            : usage.retryWasteAttempts + " attempts \u00b7 " + insightFormatTokens(usage.retryWasteTokens) +
                " \u00b7 " + insightFormatCost(usage.retryWasteCostUSD)],
    ];
    const items = rows.map(([label, value]) =>
        '<div class="kv"><div class="label">' + escapeAssociationHtml(label) +
            '</div><div class="value">' + escapeAssociationHtml(value) + "</div></div>",
    ).join("");
    return '<section class="content-section"><h3>Tokens and retry waste</h3><div class="kv-grid">' + items + "</div></section>";
}

/** The most recent `bucketCount` buckets in an ascending trend array — the current window's half. */
function insightCurrentTrendBuckets(stats, windowValue) {
    const bucketCount = INSIGHT_TREND_BUCKET_COUNTS[windowValue];
    if (!bucketCount || !Array.isArray(stats.trend) || !stats.trend.length) return [];
    return stats.trend.slice(Math.max(0, stats.trend.length - bucketCount));
}

function renderInsightTrendSection(stats, scope, windowValue) {
    if (windowValue === "all") {
        return '<p class="usage-trend-note">Trend and period comparison need a bounded time window \u2014 choose 24h, 7d, or 30d.</p>';
    }
    const buckets = insightCurrentTrendBuckets(stats, windowValue);
    if (!buckets.length) {
        return '<p class="inline-empty">No cost trend data is available for this window.</p>';
    }
    const rows = buckets.map((bucket) => {
        const usage = (bucket.usage || []).find((item) => item.scope === scope.kind && isInInsightScope(scope, item));
        return "<tr><td>" + escapeAssociationHtml(insightFormatBucketLabel(bucket.since, bucket.until)) + "</td>" +
            "<td>" + escapeAssociationHtml(insightFormatCost(usage && usage.costUSD)) + "</td>" +
            "<td>" + escapeAssociationHtml(insightFormatTokens(usage && usage.p50Tokens)) + "</td>" +
            "<td>" + escapeAssociationHtml(insightFormatSamples(usage ? usage.costSamples : 0)) + "</td></tr>";
    }).join("");
    const previousUsage = stats.trendPrevious &&
        (stats.trendPrevious.usage || []).find((item) => item.scope === scope.kind && isInInsightScope(scope, item));
    const currentUsage = insightUsageForScope(stats, scope);
    const comparison = previousUsage || currentUsage
        ? '<p class="usage-trend-note">Previous window: ' + escapeAssociationHtml(insightFormatCost(previousUsage && previousUsage.costUSD)) +
            " \u00b7 Current window: " + escapeAssociationHtml(insightFormatCost(currentUsage && currentUsage.costUSD)) + "</p>"
        : "";
    return '<div class="table-scroll"><table><thead><tr><th>Bucket</th><th>Cost</th><th>P50 tokens</th><th>Samples</th></tr></thead>' +
        "<tbody>" + rows + "</tbody></table></div>" + comparison;
}

function renderInsightStageSection(stats, scope) {
    const stages = (stats.stages || [])
        .filter((stage) => isInInsightScope(scope, stage) && stage.durationSamples > 0)
        .sort((a, b) => (b.p95DurationMs ?? -1) - (a.p95DurationMs ?? -1));
    if (!stages.length) return "";
    const visible = stages.slice(0, 10);
    const rows = visible.map((stage) =>
        "<tr><td>" + escapeAssociationHtml(stage.gaggle + " / " + stage.workflow + " / " + stage.stage) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatDuration(stage.p50DurationMs)) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatDuration(stage.p95DurationMs)) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatDuration(stage.avgDurationMs)) + "</td>" +
        "<td>" + escapeAssociationHtml(stage.durationSamples) + "</td></tr>",
    ).join("");
    const overflow = stages.length > visible.length
        ? '<p class="muted">+' + (stages.length - visible.length) + " more stages.</p>"
        : "";
    return '<section class="content-section"><h3>Slowest stages</h3>' +
        '<div class="table-scroll"><table><thead><tr>' +
        "<th>Stage</th><th>P50</th><th>P95</th><th>Average</th><th>Samples</th>" +
        "</tr></thead><tbody>" + rows + "</tbody></table></div>" + overflow + "</section>";
}

/** Renders the full Insights tab content for a loaded TelemetryStatsResult. */
export function renderInsightPanel(stats, scope, windowValue) {
    if (!stats) {
        return '<p class="inline-empty">No telemetry loaded yet.</p>';
    }
    const outcomeHtml = renderInsightOutcomeSection(stats, scope);
    const curationHtml = renderInsightCurationSection(stats, scope);
    const creditHtml = renderInsightCreditSection(stats, scope);
    const usageHtml = renderInsightUsageSection(stats, scope);
    const stagesHtml = renderInsightStageSection(stats, scope);
    if (!outcomeHtml && !curationHtml && !creditHtml && !usageHtml && !stagesHtml) {
        return '<div class="empty-state insight-empty"><h3>No telemetry in this window</h3>' +
            "<p>Choose a wider time window or another scope to inspect recorded runs.</p></div>";
    }
    const trendHtml = '<section class="content-section"><h3>Cost over time</h3>' +
        renderInsightTrendSection(stats, scope, windowValue) + "</section>";
    return outcomeHtml + curationHtml + creditHtml + usageHtml + trendHtml + stagesHtml;
}

const COST_MAX_WINDOW_MS = 90 * 24 * 60 * 60 * 1000;

export function costSummaryRequestParams(windowValue, now = new Date()) {
    const until = now.toISOString();
    const durationMs = INSIGHT_WINDOW_MS[windowValue] || COST_MAX_WINDOW_MS;
    return {
        scope: "summary",
        since: new Date(now.getTime() - durationMs).toISOString(),
        until,
    };
}

export function costLookupRequestParams(kind, provider, id, windowValue, now = new Date()) {
    const range = costSummaryRequestParams(windowValue, now);
    return {
        provider,
        scope: kind === "issue" ? "issue" : "pr",
        id,
        since: range.since,
        until: range.until,
    };
}

function costAmountByUnit(amounts, unit) {
    return (amounts || []).find((amount) => amount.unit === unit);
}

function formatCostAmount(amount) {
    if (!amount) return "Unmeasured";
    const suffix = amount.estimated ? " est." : "";
    if (amount.unit === "usd") return "$" + Number(amount.value || 0).toFixed(2) + suffix;
    if (amount.unit === "aiCredits") return Number(amount.value || 0).toLocaleString("en-US") + " credits" + suffix;
    if (amount.unit === "premiumRequests") return Number(amount.value || 0).toLocaleString("en-US") + " premium requests" + suffix;
    return String(amount.value) + " " + String(amount.unit || "units") + suffix;
}

function costAggregateNativeLabel(aggregate) {
    const amounts = aggregate?.nativeTotals || [];
    if (!amounts.length) return "Unmeasured";
    return amounts.map(formatCostAmount).join(" / ");
}

function costAggregateNormalizedLabel(aggregate) {
    const usd = costAmountByUnit(aggregate?.normalizedTotals, "usd");
    return formatCostAmount(usd || (aggregate?.normalizedTotals || [])[0]);
}

function costAggregateComparableValue(aggregate) {
    const usd = costAmountByUnit(aggregate?.nativeTotals, "usd") ?? costAmountByUnit(aggregate?.normalizedTotals, "usd");
    const aiCredits = costAmountByUnit(aggregate?.normalizedTotals, "aiCredits") ?? costAmountByUnit(aggregate?.nativeTotals, "aiCredits");
    return {
        usd: usd?.value ?? null,
        aiCredits: aiCredits?.value ?? null,
        fallback: (aggregate?.nativeTotals || [])[0]?.value ?? (aggregate?.normalizedTotals || [])[0]?.value ?? 0,
    };
}

function costCoverageLabel(coverage = {}) {
    const measuredRuns = coverage.measuredRuns ?? 0;
    const totalRuns = coverage.totalRuns ?? 0;
    const measuredAttempts = coverage.measuredAttempts ?? 0;
    const totalAttempts = coverage.totalAttempts ?? 0;
    const completeness = coverage.complete ? "complete" : coverage.lowerBound ? "lower bound" : "partial";
    return measuredRuns + "/" + totalRuns + " runs, " + measuredAttempts + "/" + totalAttempts + " attempts · " + completeness;
}

export function deriveExternalCostRows(result = {}) {
    return [
        ...(result.pullRequests || []),
        ...(result.issues || []),
    ].map((aggregate) => ({
        key: [aggregate.provider, aggregate.externalKind, aggregate.repository || "", aggregate.externalId].join("|"),
        label: (aggregate.externalKind === "issue" ? "Issue" : "PR") + " #" + aggregate.externalId,
        provider: aggregate.provider || result.provider || "provider",
        repository: aggregate.repository || "",
        externalKind: aggregate.externalKind,
        externalId: aggregate.externalId,
        url: aggregate.url || "",
        native: costAggregateNativeLabel(aggregate),
        normalized: costAggregateNormalizedLabel(aggregate),
        coverage: costCoverageLabel(aggregate.coverage),
        lowerBound: Boolean(aggregate.coverage?.lowerBound),
        models: (aggregate.models || []).map((model) =>
            model.model + " · " + insightFormatSamples(model.measuredAttempts) + " · " +
            costAggregateNormalizedLabel(model),
        ),
        runs: (aggregate.runs || []).map((run) => run.runId).filter(Boolean),
        comparable: costAggregateComparableValue(aggregate),
    }));
}

function compareExternalCostRows(a, b) {
    if (a.comparable.usd != null || b.comparable.usd != null) {
        return (b.comparable.usd ?? -Infinity) - (a.comparable.usd ?? -Infinity);
    }
    if (a.comparable.aiCredits != null || b.comparable.aiCredits != null) {
        return (b.comparable.aiCredits ?? -Infinity) - (a.comparable.aiCredits ?? -Infinity);
    }
    return (b.comparable.fallback - a.comparable.fallback) || a.label.localeCompare(b.label);
}

function renderCostSummarySection(stats, scope) {
    const usage = insightUsageForScope(stats, scope);
    if (!usage) {
        return '<section class="content-section"><h3>Cost summary</h3>' +
            '<p class="inline-empty">No measured AI usage for this scope in the selected window.</p></section>';
    }
    const rows = [
        ["Scope", insightScopeLabel(scope)],
        ["Attempts", String(usage.totalAttempts ?? 0)],
        ["AI cost total", insightFormatCost(usage.costUSD)],
        ["AI cost P50 / P95", insightFormatCost(usage.p50CostUSD) + " / " + insightFormatCost(usage.p95CostUSD)],
        ["Token P50 / P95", insightFormatTokens(usage.p50Tokens) + " / " + insightFormatTokens(usage.p95Tokens)],
        ["Measured samples", insightFormatSamples(usage.costSamples)],
        ["Retry waste", (usage.retryWasteAttempts || 0) === 0
            ? "No retry waste"
            : usage.retryWasteAttempts + " attempts · " + insightFormatCost(usage.retryWasteCostUSD) + " · " + insightFormatTokens(usage.retryWasteTokens)],
    ];
    const items = rows.map(([label, value]) =>
        '<div class="kv"><div class="label">' + escapeAssociationHtml(label) +
            '</div><div class="value">' + escapeAssociationHtml(value) + "</div></div>",
    ).join("");
    return '<section class="content-section"><h3>Cost summary</h3>' +
        '<p class="section-description">Measured attempts only; unreported runner usage remains unmeasured.</p>' +
        '<div class="kv-grid">' + items + "</div></section>";
}

function renderCostTrendSection(stats, scope, windowValue) {
    return '<section class="content-section"><h3>Cost trend</h3>' +
        renderInsightTrendSection(stats, scope, windowValue) + "</section>";
}

function renderInstanceCostRollupSection(stats, scope, windowValue) {
    if (scope.kind !== "instance") return "";
    const rows = (stats.gaggles || [])
        .map((gaggle) => ({
            gaggle: gaggle.gaggle,
            usage: (stats.usage || []).find((item) => item.scope === "gaggle" && item.gaggle === gaggle.gaggle),
        }))
        .filter((entry) => (entry.usage?.costSamples || 0) > 0)
        .sort((a, b) => (b.usage?.costUSD || 0) - (a.usage?.costUSD || 0));
    if (!rows.length) {
        return '<section class="content-section"><h3>Cost by gaggle</h3>' +
            '<p class="inline-empty">No gaggle has a measured AI cost in this window.</p></section>';
    }
    const body = rows.map(({ gaggle, usage }) =>
        "<tr><td>" + escapeAssociationHtml(gaggle) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatCost(usage.costUSD)) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatCost(usage.p50CostUSD)) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatCost(usage.p95CostUSD)) + "</td>" +
        "<td>" + escapeAssociationHtml(insightFormatSamples(usage.costSamples)) + "</td></tr>",
    ).join("");
    return '<section class="content-section"><h3>Cost by gaggle</h3>' +
        '<p class="section-description">All gaggles · ' + escapeAssociationHtml(windowValue === "all" ? "all time" : windowValue) + "</p>" +
        '<div class="table-scroll"><table><thead><tr><th>Gaggle</th><th>Total</th><th>P50</th><th>P95</th><th>Samples</th></tr></thead>' +
        "<tbody>" + body + "</tbody></table></div></section>";
}

function renderExternalCostBreakdownSection(costs, lookupCosts) {
    const renderRows = (items) => items.map((row) => {
        const href = safeAssociationUrl(row.url);
        const label = href
            ? '<a href="' + escapeAssociationHtml(href) + '" target="_blank" rel="noopener noreferrer">' + escapeAssociationHtml(row.label) + "</a>"
            : "<strong>" + escapeAssociationHtml(row.label) + "</strong>";
        const visibleModels = row.models.slice(0, 3);
        const modelOverflow = row.models.length > visibleModels.length ? " +" + (row.models.length - visibleModels.length) + " more" : "";
        const modelLabel = visibleModels.join("; ") + modelOverflow;
        return '<tr><td>' + label + '<div class="muted">' + escapeAssociationHtml(row.repository || row.provider) + "</div></td>" +
            "<td>" + escapeAssociationHtml(row.provider) + "</td>" +
            "<td>" + escapeAssociationHtml(row.native) + "</td>" +
            "<td>" + escapeAssociationHtml(row.normalized) + "</td>" +
            '<td class="' + (row.lowerBound ? "cost-coverage-warning" : "muted") + '">' + escapeAssociationHtml(row.coverage) + "</td>" +
            "<td>" + escapeAssociationHtml(row.runs.length) + "</td>" +
            "<td>" + escapeAssociationHtml(modelLabel || "Unmeasured") + "</td></tr>";
    }).join("");
    const lookupRows = lookupCosts ? deriveExternalCostRows(lookupCosts).sort(compareExternalCostRows) : [];
    const lookupHtml = lookupCosts
        ? '<h4>Lookup result</h4>' + (lookupRows.length
            ? '<div class="table-scroll"><table><thead><tr><th>Work item</th><th>Provider</th><th>Provider-native</th><th>Normalized estimate</th><th>Coverage</th><th>Runs</th><th>Models</th></tr></thead><tbody>' +
                renderRows(lookupRows) + "</tbody></table></div>"
            : '<p class="inline-empty">No attributed costs match that pull request or issue in this window.</p>')
        : "";
    if (!costs) {
        return '<section class="content-section"><h3>Cost by pull request and issue</h3>' +
            '<p class="inline-empty">Attributed pull request and issue costs could not be loaded; selected-scope cost summary remains available.</p>' +
            lookupHtml + "</section>";
    }
    const allRows = deriveExternalCostRows(costs)
        .sort(compareExternalCostRows);
    const rows = allRows.slice(0, 25);
    const overflow = allRows.length > rows.length
        ? '<p class="muted">+' + (allRows.length - rows.length) + " more work items.</p>"
        : "";
    if (!rows.length) {
        return '<section class="content-section"><h3>Cost by pull request and issue</h3>' +
            '<p class="inline-empty">No pull request or issue cost was attributed in this window.</p>' + lookupHtml + "</section>";
    }
    const bounded = costs.scope === "summary" && costs.since && costs.until
        ? '<p class="usage-description">Attribution is instance-wide and uses exact recorded usage from ' +
            escapeAssociationHtml(insightFormatBucketLabel(costs.since, costs.until)) +
            (costs.boundedAllTime ? " (all-time attribution is capped at 90 days)." : ".") +
            "</p>"
        : '<p class="usage-description">Attribution is instance-wide regardless of the selected operational scope.</p>';
    return '<section class="content-section"><h3>Cost by pull request and issue</h3>' +
        '<p class="section-description">Exact recorded usage by external work item, sorted by comparable cost where available; lower-bound rows have incomplete cost coverage.</p>' +
        bounded +
        '<div class="table-scroll"><table><thead><tr><th>Work item</th><th>Provider</th><th>Provider-native</th><th>Normalized estimate</th><th>Coverage</th><th>Runs</th><th>Models</th></tr></thead><tbody>' +
        renderRows(rows) + "</tbody></table></div>" + overflow + lookupHtml + "</section>";
}

export function renderCostPanel(stats, costs, scope, windowValue, lookupCosts = null) {
    if (!stats && !costs && !lookupCosts) {
        return '<p class="inline-empty">No cost telemetry loaded yet.</p>';
    }
    const statsUnavailable = stats
        ? ""
        : '<section class="content-section"><h3>Cost summary</h3>' +
            '<p class="inline-empty">Selected-scope usage, trend, and instance rollup are unavailable; attributed work-item costs remain available.</p></section>';
    const summaryHtml = stats ? renderCostSummarySection(stats, scope) : statsUnavailable;
    const trendHtml = stats ? renderCostTrendSection(stats, scope, windowValue) : "";
    const rollupHtml = stats ? renderInstanceCostRollupSection(stats, scope, windowValue) : "";
    const externalHtml = renderExternalCostBreakdownSection(costs, lookupCosts);
    return summaryHtml + trendHtml + rollupHtml + externalHtml;
}

export function workItemLabel(repository, externalId) {
    return repository ? repository + "#" + externalId : "#" + externalId;
}

export function humanizeWorkItemOperation(operation) {
    const value = String(operation || "").trim();
    if (!value) return "Provider action";
    return value.replaceAll("-", " ").replace(/\b\w/g, (letter) => letter.toUpperCase());
}

export function formatWorkItemTimestamp(value) {
    if (!value) return "\u2014";
    const date = new Date(value);
    return Number.isNaN(date.getTime()) || date.getUTCFullYear() < 1970 ? "\u2014" : date.toLocaleString();
}

export function formatWorkItemCost(cost) {
    if (!cost) return "Not attributed";
    if (cost.costUSD !== undefined && cost.costUSD !== null) {
        return new Intl.NumberFormat("en-US", {
            style: "currency",
            currency: "USD",
            minimumFractionDigits: 2,
            maximumFractionDigits: 4,
        }).format(cost.costUSD);
    }
    if (cost.nanoAIU !== undefined && cost.nanoAIU !== null) {
        return new Intl.NumberFormat("en-US").format(cost.nanoAIU) + " nano-AIU";
    }
    return "Not measured";
}

export function filterWorkItems(items, gaggle, search) {
    const query = String(search || "").trim().toLocaleLowerCase();
    return (items || []).filter((item) => {
        if (gaggle && item.gaggle !== gaggle) return false;
        if (!query) return true;
        return [
            workItemLabel(item.repository, item.externalId),
            item.repository,
            item.externalId,
        ].some((value) => String(value || "").toLocaleLowerCase().includes(query));
    });
}

export const WORK_ITEM_SORT_KEYS = ["identity", "lastActionAt", "workflow", "actionCount"];

export function sortWorkItems(items, sortKey = "lastActionAt", sortDir = "desc") {
    const dir = sortDir === "asc" ? 1 : -1;
    const valueFor = (item) => {
        if (sortKey === "identity") return workItemLabel(item.repository, item.externalId).toLocaleLowerCase();
        if (sortKey === "actionCount") return item.actionCount ?? 0;
        if (sortKey === "lastActionAt") return new Date(item.lastActionAt || 0).getTime() || 0;
        return String(item[sortKey] || "").toLocaleLowerCase();
    };
    return [...(items || [])].sort((a, b) => {
        const av = valueFor(a);
        const bv = valueFor(b);
        if (av < bv) return -1 * dir;
        if (av > bv) return 1 * dir;
        return 0;
    });
}

export function workItemKindIcon(kind) {
    const icons = {
        pr: '<svg class="work-item-kind-icon" data-work-item-kind-icon="pr" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><path fill="currentColor" d="M1.5 3.25a2.25 2.25 0 1 1 3 2.122v5.256a2.251 2.251 0 1 1-1.5 0V5.372A2.25 2.25 0 0 1 1.5 3.25Zm5.677-.177L9.573.677A.25.25 0 0 1 10 .854V2.5h1A2.5 2.5 0 0 1 13.5 5v5.628a2.251 2.251 0 1 1-1.5 0V5a1 1 0 0 0-1-1h-1v1.646a.25.25 0 0 1-.427.177L7.177 3.427a.25.25 0 0 1 0-.354ZM3.75 2.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm0 9.5a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Zm8.25.75a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Z"></path></svg>',
        issue: '<svg class="work-item-kind-icon" data-work-item-kind-icon="issue" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><path fill="currentColor" d="M8 9.5a1.5 1.5 0 1 0 0-3 1.5 1.5 0 0 0 0 3Z"></path><path fill="currentColor" d="M8 0a8 8 0 1 1 0 16A8 8 0 0 1 8 0ZM1.5 8a6.5 6.5 0 1 0 13 0 6.5 6.5 0 0 0-13 0Z"></path></svg>',
    };
    return icons[kind] || icons.issue;
}

export function renderWorkItemStatusBadge(runStatus) {
    const status = String(runStatus || "").trim();
    if (!status) return "";
    const meta = {
        running: { emoji: "\ud83d\udd35", label: "Running" },
        completed: { emoji: "\u2705", label: "Completed" },
        succeeded: { emoji: "\u2705", label: "Succeeded" },
        failed: { emoji: "\u274c", label: "Failed" },
        escalated: { emoji: "\ud83d\udea8", label: "Escalated" },
        blocked: { emoji: "\u23f8\ufe0f", label: "Blocked" },
        "awaiting-human": { emoji: "\ud83d\udd52", label: "Awaiting human" },
        pending: { emoji: "\u26aa", label: "Pending" },
    }[status] || { emoji: "\u26aa", label: status };
    return '<span class="phase" data-phase="' + escapeAssociationHtml(status) + '" title="Last run status: ' +
        escapeAssociationHtml(meta.label) + '">' + meta.emoji + " " + escapeAssociationHtml(meta.label) + "</span>";
}

function renderWorkItemListHeader(sortKey, sortDir) {
    const columns = [
        ["identity", "Work item"],
        ["lastActionAt", "Last action"],
        ["workflow", "Workflow / Gaggle"],
        ["actionCount", "Actions"],
    ];
    const cells = columns.map(([key, label]) => {
        const active = key === sortKey;
        const ariaSort = active ? (sortDir === "asc" ? "ascending" : "descending") : "none";
        const arrow = active ? (sortDir === "asc" ? " \u25b2" : " \u25bc") : "";
        return '<span role="columnheader" aria-sort="' + ariaSort + '"><button type="button" class="sort-button" data-work-item-sort="' +
            key + '">' + escapeAssociationHtml(label) + arrow + "</button></span>";
    }).join("");
    return '<div class="work-item-row work-item-header" role="row">' + cells + "<span></span></div>";
}

export function renderWorkItemList(page, gaggle = "", search = "", sortKey = "lastActionAt", sortDir = "desc") {
    if (!page) return '<p class="inline-empty">No work items loaded yet.</p>';
    const items = sortWorkItems(filterWorkItems(page.items, gaggle, search), sortKey, sortDir);
    const header = renderWorkItemListHeader(sortKey, sortDir);
    if (!items.length) {
        return '<div class="work-items-list work-items-list-empty">' + header + "</div>" +
            '<p class="inline-empty">No confirmed provider actions match this filter.' +
            (page.hasMore ? " Only the 200 most recently actioned work items are searched." : "") +
            "</p>";
    }
    const rows = items.map((item) => {
        const identity = workItemLabel(item.repository, item.externalId);
        const typeLabel = item.kind === "pr" ? "pull request" : "issue";
        const statusBadge = renderWorkItemStatusBadge(item.runStatus);
        return '<button type="button" class="work-item-row" data-work-item-provider="' +
            escapeAssociationHtml(item.provider || "") + '" data-work-item-repository="' +
            escapeAssociationHtml(item.repository || "") + '" data-work-item-kind="' +
            escapeAssociationHtml(item.kind || "") + '" data-work-item-id="' +
            escapeAssociationHtml(item.externalId || "") + '" aria-label="Open ' +
            escapeAssociationHtml(item.kind === "pr" ? "PR" : "issue") + " #" +
            escapeAssociationHtml(item.externalId || "") + " in " +
            escapeAssociationHtml(item.repository || "unknown repository") + '" title="' +
            escapeAssociationHtml(identity) + " \u00b7 " + escapeAssociationHtml(typeLabel) + '">' +
            '<span class="work-item-identity" title="' + escapeAssociationHtml(typeLabel) + '">' +
            workItemKindIcon(item.kind) + '<strong>' + escapeAssociationHtml(identity) +
            '</strong><small>' + escapeAssociationHtml(item.provider || "provider") + " \u00b7 " +
            escapeAssociationHtml(typeLabel) + "</small></span>" +
            '<span title="Last action"><strong>' + escapeAssociationHtml(humanizeWorkItemOperation(item.lastOperation)) +
            '</strong><small>' + escapeAssociationHtml(formatWorkItemTimestamp(item.lastActionAt)) +
            (statusBadge ? " " + statusBadge : "") + "</small></span>" +
            '<span title="Workflow and gaggle"><strong>' + escapeAssociationHtml(item.workflow || "Unknown") +
            '</strong><small>' + escapeAssociationHtml(item.gaggle || "No gaggle recorded") +
            "</small></span>" +
            '<strong class="work-item-action-count" title="Recorded provider actions">' + escapeAssociationHtml(item.actionCount ?? 0) +
            '</strong><span aria-hidden="true">\u203a</span></button>';
    }).join("");
    const overflow = page.hasMore
        ? '<p class="muted work-item-overflow">Showing the 200 most recently actioned work items.</p>'
        : "";
    return '<div class="work-items-list" role="group" aria-label="Work items">' + header + rows + "</div>" + overflow;
}

export function renderWorkItemDetail(item, actionType = "all") {
    if (!item) return '<p class="inline-empty">No work item detail loaded yet.</p>';
    const identity = workItemLabel(item.repository, item.externalId);
    const typeLabel = item.kind === "pr" ? "pull request" : "issue";
    const itemHref = safeAssociationUrl(item.url);
    const headingLink = itemHref
        ? '<a class="actions-run-link" href="' + escapeAssociationHtml(itemHref) +
            '" target="_blank" rel="noopener noreferrer">Open ' +
            escapeAssociationHtml(typeLabel) + " \u2197</a>"
        : "";
    const relatedLinks = (item.relatedPullRequests || []).map((related) => {
        const href = safeAssociationUrl(related.url);
        if (!href) return "";
        return '<a class="actions-run-link" href="' + escapeAssociationHtml(href) +
            '" target="_blank" rel="noopener noreferrer" aria-label="Open related PR ' +
            escapeAssociationHtml(workItemLabel(related.repository, related.externalId)) +
            '">Related PR ' + escapeAssociationHtml(workItemLabel(related.repository, related.externalId)) +
            " \u2197</a>";
    }).filter(Boolean).join("");
    const actions = item.actions || [];
    const actionTypes = [...new Set(actions.map((action) => action.operation).filter(Boolean))]
        .sort((left, right) => humanizeWorkItemOperation(left).localeCompare(humanizeWorkItemOperation(right)));
    const options = ['<option value="all">All actions</option>'].concat(actionTypes.map((operation) =>
        '<option value="' + escapeAssociationHtml(operation) + '"' +
        (operation === actionType ? " selected" : "") + ">" +
        escapeAssociationHtml(humanizeWorkItemOperation(operation)) + "</option>",
    )).join("");
    const visibleActions = actionType === "all"
        ? actions
        : actions.filter((action) => action.operation === actionType);
    const actionRows = visibleActions.map((action) =>
        '<div class="work-item-action-row" role="row">' +
        '<span role="cell"><strong>' + escapeAssociationHtml(humanizeWorkItemOperation(action.operation)) +
        '</strong><small>Sequence ' + escapeAssociationHtml(action.sequence ?? "") + "</small></span>" +
        '<span role="cell"><strong>' + escapeAssociationHtml(action.gaggle || "Unknown gaggle") +
        '</strong><small>' + escapeAssociationHtml(action.workflow || "Workflow unavailable") + "</small></span>" +
        '<span role="cell">' + escapeAssociationHtml(action.runStatus
            ? humanizeWorkItemOperation(action.runStatus)
            : "Unknown") + "</span>" +
        '<time role="cell" datetime="' + escapeAssociationHtml(action.occurredAt || "") + '">' +
        escapeAssociationHtml(formatWorkItemTimestamp(action.occurredAt)) + "</time>" +
        '<span role="cell"><button type="button" class="table-link" data-work-item-run="' +
        escapeAssociationHtml(action.runId || "") + '">View run</button></span></div>',
    ).join("");
    const emptyActions = visibleActions.length
        ? ""
        : '<p class="inline-empty" role="status">No actions match this type.</p>';
    const lowerBound = item.cost?.lowerBound
        ? '<small class="cost-coverage-warning">Lower bound; some usage is unmeasured.</small>'
        : "";
    const coverage = item.cost
        ? '<small>' + escapeAssociationHtml(
            (item.cost.measuredRuns ?? 0) + "/" + (item.cost.totalRuns ?? 0) + " runs \u00b7 " +
            (item.cost.measuredAttempts ?? 0) + "/" + (item.cost.totalAttempts ?? 0) + " attempts",
        ) + "</small>"
        : "";
    const truncated = item.truncated
        ? '<p class="muted work-item-overflow">Showing the 200 most recent actions.</p>'
        : "";
    return '<div class="work-item-detail">' +
        '<button type="button" class="back" id="work-item-back">\u2190 Back to Work Items</button>' +
        '<div class="work-item-detail-heading"><div><p class="muted">' +
        escapeAssociationHtml(item.provider || "provider") + " " + escapeAssociationHtml(typeLabel) +
        ' activity</p><h2>' + escapeAssociationHtml(identity) + "</h2></div>" +
        '<div class="work-item-heading-links">' + headingLink + relatedLinks + "</div></div>" +
        '<div class="cards work-item-summary-cards"><div class="card"><div class="label">Confirmed actions</div>' +
        '<div class="value">' + escapeAssociationHtml(actions.length) + "</div></div>" +
        '<div class="card"><div class="label">Attributed cost to date</div><div class="value">' +
        escapeAssociationHtml(formatWorkItemCost(item.cost)) + "</div>" + coverage + lowerBound + "</div></div>" +
        '<div class="filters-bar work-item-action-filters"><label>Action type ' +
        '<select id="work-item-action-type" aria-label="Filter actions by type">' + options +
        "</select></label></div>" +
        '<div class="work-item-actions" role="table" aria-label="Action history for ' +
        escapeAssociationHtml(identity) + '"><div class="work-item-action-header" role="row">' +
        '<span role="columnheader">Action</span><span role="columnheader">Gaggle / workflow</span>' +
        '<span role="columnheader">Status</span><span role="columnheader">Time</span>' +
        '<span role="columnheader">Run</span></div>' + actionRows + emptyActions + "</div>" +
        truncated + "</div>";
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
  .source-context { display: flex; gap: 8px 16px; align-items: center; flex-wrap: wrap; flex: 1 1 auto; min-width: 0; }
  #source-context { font-weight: 600; overflow-wrap: anywhere; margin: 0; }
  #freshness { margin: 0; }
  .section-description { margin: 0 0 12px; color: var(--text-color-muted, #656d76); }
  .cost-coverage-warning { color: var(--true-color-yellow, #9a6700); }
  .table-link { padding: 0; border: 0; background: transparent; color: var(--true-color-blue, #0969da); text-align: left; }
  .table-link:hover { background: transparent; text-decoration: underline; }
  .sort-button { border: 0; padding: 0; background: transparent; color: inherit; }
  .table-scroll { max-width: 100%; overflow-x: auto; }
  .table-scroll table { width: auto; min-width: 100%; white-space: nowrap; }
  .work-items-list {
    display: grid;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    overflow: hidden;
  }
  .work-item-row {
    display: grid;
    grid-template-columns: minmax(220px, 2fr) minmax(160px, 1.2fr) minmax(170px, 1.2fr) 70px 20px;
    gap: 10px;
    align-items: center;
    width: 100%;
    border: 0;
    border-bottom: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 0;
    padding: 10px 12px;
    text-align: left;
  }
  .work-item-row:last-child { border-bottom: 0; }
  .work-item-row span { min-width: 0; }
  .work-item-row strong,
  .work-item-row small { display: block; overflow-wrap: anywhere; }
  .work-item-row small { color: var(--text-color-muted, #656d76); }
  .work-item-action-count { text-align: center; }
  .work-item-header { font-weight: 600; background: var(--background-color-hover, #f6f8fa); cursor: default; }
  .work-item-header span[role="columnheader"] { min-width: 0; }
  .work-item-header .sort-button { font: inherit; font-weight: 600; white-space: nowrap; }
  .work-item-identity { display: flex !important; align-items: center; gap: 6px; }
  .work-item-identity strong { flex: 1 1 auto; min-width: 0; }
  .work-item-kind-icon { flex: 0 0 auto; color: var(--text-color-muted, #656d76); }
  .work-item-row[data-work-item-kind="pr"] .work-item-kind-icon { color: var(--true-color-green, #1a7f37); }
  .work-item-row[data-work-item-kind="issue"] .work-item-kind-icon { color: var(--true-color-blue, #0969da); }
  .work-items-list-empty .work-item-header { border-radius: 8px; }
  .work-item-overflow { margin: 10px 0 0; }
  .work-item-detail-heading,
  .work-item-heading-links {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 8px 12px;
    flex-wrap: wrap;
  }
  .work-item-detail-heading h2,
  .work-item-detail-heading p { margin: 0; }
  .work-item-heading-links { justify-content: flex-end; }
  .work-item-summary-cards { margin-top: 14px; }
  .work-item-summary-cards small { display: block; margin-top: 4px; }
  .work-item-actions {
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    overflow: hidden;
  }
  .work-item-action-header,
  .work-item-action-row {
    display: grid;
    grid-template-columns: minmax(150px, 1.25fr) minmax(160px, 1.25fr) minmax(90px, .7fr) minmax(150px, 1fr) 70px;
    gap: 10px;
    align-items: center;
    padding: 9px 11px;
  }
  .work-item-action-header {
    color: var(--text-color-muted, #656d76);
    background: var(--border-color-default, #d0d7de22);
    font-size: 12px;
  }
  .work-item-action-row { border-top: 1px solid var(--border-color-default, #d0d7de); }
  .work-item-action-row strong,
  .work-item-action-row small { display: block; overflow-wrap: anywhere; }
  .work-item-action-row small { color: var(--text-color-muted, #656d76); }
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
  .source-picker { position: relative; }
  .source-picker-trigger {
    display: flex;
    align-items: center;
    gap: 6px;
    max-width: min(100%, 360px);
  }
  .source-picker-trigger span:first-child {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .source-picker-caret { font-size: 11px; }
  .source-picker-menu {
    position: absolute;
    z-index: 20;
    top: calc(100% + 4px);
    left: 0;
    min-width: max(100%, 280px);
    max-width: min(90vw, 420px);
    background: var(--background-color-default, #fff);
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 8px;
    box-shadow: 0 8px 24px #0003;
    padding: 6px;
  }
  #source-picker-list {
    max-height: 260px;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .source-picker-option {
    display: flex;
    align-items: center;
    gap: 4px;
  }
  .source-picker-option-select {
    flex: 1;
    min-width: 0;
    text-align: left;
    border: 0;
    background: transparent;
    padding: 6px 8px;
    border-radius: 6px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .source-picker-option-select:hover { background: var(--background-color-muted, #f6f8fa); }
  .source-picker-option.is-selected .source-picker-option-select { font-weight: 600; }
  .source-picker-option-remove {
    border: 0;
    background: transparent;
    color: var(--text-color-muted, #656d76);
    padding: 4px 6px;
    border-radius: 6px;
  }
  .source-picker-option-remove:hover { color: var(--true-color-red, #cf222e); background: var(--background-color-muted, #f6f8fa); }
  .source-picker-connect {
    width: 100%;
    margin-top: 6px;
    text-align: left;
  }
  #source-picker-list:empty { display: none; }
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
  .configuration-warning-cell:not(:empty) { min-width: 320px; vertical-align: top; }
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
    overflow: hidden;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 999px;
    padding: 2px 7px;
    background: var(--border-color-default, #d0d7de22);
    font-size: 12px;
    vertical-align: middle;
  }
  .goober-avatar { line-height: 1; flex: 0 0 auto; }
  .goober-label { min-width: 0; flex: 1 1 auto; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
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
    .source-picker { flex: 1; min-width: 0; }
    .waterfall-row { grid-template-columns: minmax(0, 1fr); gap: 4px; }
    .graph-toolbar { flex-wrap: wrap; }
    .graph-help { width: 100%; }
    .work-item-row,
    .work-item-action-header,
    .work-item-action-row { grid-template-columns: minmax(0, 1fr); }
    .work-item-action-header { display: none; }
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
  .dialog-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 12px 16px;
    border-bottom: 1px solid var(--border-color-default, #d0d7de);
  }
  .dialog-header h2 { margin: 0; font-size: var(--text-title-medium, 18px); }
  #connect-source-close {
    border: 0;
    background: transparent;
    font-size: 18px;
    line-height: 1;
    padding: 4px 8px;
  }
  #connect-source-dialog { max-height: calc(100vh - 48px); overflow: auto; }
  .add-form-section { padding: 4px 16px 16px; }
  .add-form-section:not(:last-of-type) { border-bottom: 1px solid var(--border-color-default, #d0d7de); }
  .add-form-section h3 {
    font-size: var(--text-body-medium, 14px);
    margin: 12px 0 8px;
    color: var(--text-color-muted, #656d76);
  }
  .discover-local { display: flex; flex-direction: column; gap: 6px; margin-bottom: 4px; }
  #discover-local-results {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  #discover-local-results li {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 6px 8px;
    border: 1px solid var(--border-color-default, #d0d7de);
    border-radius: 6px;
    overflow-wrap: anywhere;
  }
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
  .stage-node.selected .node-rect {
    stroke: var(--true-color-blue, #0969da);
    stroke-width: 3;
  }
  .stage-node:focus .node-rect {
    stroke: var(--true-color-blue, #0969da);
    stroke-width: 3;
    stroke-dasharray: 5 3;
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
  .event-list details,
  .event-list .event-row { margin: 0; border-bottom: 1px solid var(--border-color-default, #d0d7de); }
  .event-list details:last-child,
  .event-list .event-row:last-child { border-bottom: none; }
  .event-list summary {
    padding: 7px 10px 7px 26px;
    display: flex;
    align-items: baseline;
    gap: 8px;
    flex-wrap: wrap;
    position: relative;
    list-style: none;
  }
  .event-list summary::-webkit-details-marker { display: none; }
  .event-list summary::before {
    content: "+";
    position: absolute;
    left: 10px;
    width: 12px;
    text-align: center;
    color: var(--text-color-muted, #656d76);
    font-weight: 600;
  }
  .event-list details[open] > summary::before { content: "\u2212"; }
  .event-list .event-row-flat {
    padding: 7px 10px 7px 26px;
  }
  .event-list .event-summary-line { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
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
  <div class="source-context">
    <p id="source-context" class="muted"></p>
    <p id="freshness" class="freshness" role="status">Connecting</p>
  </div>
  <div class="toolbar">
    <select id="theme-select" aria-label="Color theme" title="Color theme">
      <option value="system">System theme</option>
      <option value="light">Light theme</option>
      <option value="dark">Dark theme</option>
    </select>
    <div class="source-picker">
      <button id="source-picker-trigger" type="button" class="source-picker-trigger"
        aria-haspopup="listbox" aria-expanded="false" aria-controls="source-picker-menu" aria-label="Goobers source">
        <span id="source-picker-label">No sources yet</span>
        <span class="source-picker-caret" aria-hidden="true">&#9662;</span>
      </button>
      <div id="source-picker-menu" class="source-picker-menu" role="listbox" aria-label="Goobers source" hidden>
        <div id="source-picker-list"></div>
        <button id="connect-source-button" type="button" class="source-picker-connect">+ Connect a source&hellip;</button>
      </div>
    </div>
    <!-- Internal state store only: kept in sync with the picker above and driven
         by the same change event the rest of the app already listens on. Hidden
         from both layout and the accessibility tree; the picker is the real UI. -->
    <select id="source-select" hidden><option value="">No sources yet</option></select>
    <input id="run-jump" type="text" placeholder="Run ID" aria-label="Jump to a run" style="max-width: 180px;" />
    <button id="run-jump-button" type="button">Jump</button>
    <button id="refresh">Refresh</button>
  </div>
</header>
<main id="main-content" tabindex="-1">
  <dialog id="connect-source-dialog" aria-label="Connect a source">
    <div class="dialog-header">
      <h2>Connect a source</h2>
      <button id="connect-source-close" type="button" aria-label="Close">&times;</button>
    </div>
    <section class="add-form-section">
    <h3>Local instance</h3>
    <div class="add-form">
      <input id="local-root" aria-label="Local instance root" placeholder="Local instance root path (e.g. C:\\\\path\\\\to\\\\instance)" />
      <button id="browse-local" title="Browse folders">&#128193; Browse</button>
      <button id="add-local">Add local</button>
    </div>
    <div class="discover-local">
      <button id="discover-local-instances" type="button">&#128269; Discover local instances</button>
      <div id="discover-local-status" class="muted" role="status"></div>
      <ul id="discover-local-results"></ul>
    </div>
    </section>
    <section class="add-form-section">
    <h3>Remote control plane</h3>
    <div class="add-form">
      <input id="remote-url" aria-label="Remote control-plane URL" placeholder="Remote control-plane URL (e.g. http://10.0.0.5:8080)" />
      <input id="remote-token" placeholder="Bearer token (optional)" style="flex: 0 0 200px" />
      <button id="add-remote">Add remote</button>
    </div>
    </section>
    <section class="add-form-section">
    <h3>GitHub Actions</h3>
    <div class="add-form">
      <input id="github-workflow-url" aria-label="GitHub Actions workflow URL" placeholder="GitHub Actions workflow URL (https://github.com/owner/repo/actions/workflows/file.yml)" />
      <button id="add-github">Connect to GitHub</button>
    </div>
    </section>
  </dialog>
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
      <button id="dashboard-tab-work-items" role="tab" data-tab="work-items" aria-controls="dashboard-panel-work-items">Work Items</button>
      <button id="dashboard-tab-insights" role="tab" data-tab="insights" aria-controls="dashboard-panel-insights">Insights</button>
      <button id="dashboard-tab-cost" role="tab" data-tab="cost" aria-controls="dashboard-panel-cost">Cost</button>
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
          <tr><th data-sort="name">Workflow</th><th data-sort="gaggle">Gaggle</th><th data-sort="trigger">Trigger</th><th data-sort="inFlight">In flight</th><th data-sort="max">Max</th><th>Run</th><th>Enabled</th><th>Warnings</th></tr>
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
    <section id="dashboard-panel-work-items" role="tabpanel" aria-labelledby="dashboard-tab-work-items" hidden>
      <h2>Work Items</h2>
      <p class="section-description">Pull requests and issues that Goobers changed through a provider operation.</p>
      <div id="work-item-list-controls">
        <div class="filters-bar" id="work-item-kind-filters" role="group" aria-label="Work item type">
          <button type="button" data-work-item-kind-filter="" aria-pressed="true">All</button>
          <button type="button" data-work-item-kind-filter="pr" aria-pressed="false">Pull requests</button>
          <button type="button" data-work-item-kind-filter="issue" aria-pressed="false">Issues</button>
          <label>Gaggle
            <select id="work-item-gaggle" aria-label="Filter work items by gaggle">
              <option value="">All gaggles</option>
            </select>
          </label>
          <label>Find work item
            <input id="work-item-search" type="search" aria-label="Search work items" placeholder="Repository or number" />
          </label>
        </div>
      </div>
      <div id="work-item-status" class="muted" role="status"></div>
      <div id="work-item-content"></div>
    </section>
    <section id="dashboard-panel-insights" role="tabpanel" aria-labelledby="dashboard-tab-insights" hidden>
      <h2>Insights</h2>
      <p class="section-description">Aggregate telemetry across runs: success/failure, cost and usage, curation health, and cost trend.</p>
      <div class="filters-bar" id="insights-filters">
        <select id="insight-scope" aria-label="Insight scope" title="Select instance, gaggle, or workflow scope">
          <option value="instance">Instance</option>
        </select>
        <select id="insight-window" aria-label="Time window" title="Select time window"></select>
      </div>
      <div id="insight-status" class="muted" role="status"></div>
      <div id="insight-content"></div>
    </section>
    <section id="dashboard-panel-cost" role="tabpanel" aria-labelledby="dashboard-tab-cost" hidden>
      <h2>Cost</h2>
      <p class="section-description">Instance spend, selected-scope AI cost, retry waste, and attributed pull request and issue costs.</p>
      <div class="filters-bar" id="cost-filters">
        <select id="cost-scope" aria-label="Cost scope" title="Select instance, gaggle, or workflow scope">
          <option value="instance">Instance</option>
        </select>
        <select id="cost-window" aria-label="Cost time window" title="Select time window"></select>
      </div>
      <div class="filters-bar" id="cost-lookup-filters" aria-label="External cost lookup">
        <select id="cost-lookup-kind" aria-label="External cost type">
          <option value="pr">Pull request</option>
          <option value="issue">Issue</option>
        </select>
        <input id="cost-lookup-provider" aria-label="External cost provider" placeholder="Provider (github)" value="github" />
        <input id="cost-lookup-id" aria-label="External cost ID" placeholder="PR or issue ID" />
        <button id="cost-lookup-run" type="button">Lookup</button>
        <button id="cost-lookup-clear" type="button">Clear</button>
      </div>
      <div id="cost-status" class="muted" role="status"></div>
      <div id="cost-content"></div>
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
  let runOrigin = "runs";
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
    if (root === dashboardEl) {
      activeDashboardTab = selected.dataset.tab;
      if (activeDashboardTab === "work-items") void loadWorkItems();
      if (activeDashboardTab === "insights") void loadInsights();
      if (activeDashboardTab === "cost") void loadCost();
    }
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

  let lastSources = [];
  const sourcePickerTrigger = document.getElementById("source-picker-trigger");
  const sourcePickerMenu = document.getElementById("source-picker-menu");
  const sourcePickerList = document.getElementById("source-picker-list");
  const sourcePickerLabel = document.getElementById("source-picker-label");

  async function loadSources() {
    const [res, selectedRes] = await Promise.all([
      fetch("/api/sources"),
      fetch("/api/selected-source"),
    ]);
    const [data, selected] = await Promise.all([res.json(), selectedRes.json()]);
    const sources = data.sources || [];
    lastSources = sources;
    const prevValue = sourceSelect.value;
    sourceSelect.innerHTML = "";
    if (sources.length === 0) {
      sourceSelect.innerHTML = '<option value="">No sources yet</option>';
      renderSourcePicker();
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
      renderSourcePicker();
      return prevValue;
    }
    if (selected.sourceId && sources.some((s) => s.id === selected.sourceId)) {
      sourceSelect.value = selected.sourceId;
      renderSourcePicker();
      return selected.sourceId;
    }
    const firstConnected = sources.find((s) => s.connected);
    sourceSelect.value = (firstConnected || sources[0]).id;
    renderSourcePicker();
    return sourceSelect.value;
  }

  function openSourcePicker() {
    sourcePickerMenu.hidden = false;
    sourcePickerTrigger.setAttribute("aria-expanded", "true");
  }

  function closeSourcePicker() {
    sourcePickerMenu.hidden = true;
    sourcePickerTrigger.setAttribute("aria-expanded", "false");
  }

  async function removeSourceById(id, label) {
    if (!window.confirm("Remove " + label + " from known sources?")) return;
    try {
      const response = await fetch("/api/remove-source", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id }),
      });
      const result = await response.json();
      if (!response.ok || result.error) throw new Error(result.error || "Could not remove source.");
      await loadSources();
      await changeSource();
    } catch (err) {
      errorEl.textContent = portalRequestError(err);
    }
  }

  function renderSourcePicker() {
    sourcePickerList.innerHTML = "";
    for (const s of lastSources) {
      const label = s.label || s.value;
      const dot = s.connected ? "\u25cf" : "\u25cb";
      const row = document.createElement("div");
      row.className = "source-picker-option";
      row.setAttribute("role", "option");
      row.dataset.id = s.id;
      const isSelected = s.id === sourceSelect.value;
      row.setAttribute("aria-selected", String(isSelected));
      if (isSelected) row.classList.add("is-selected");
      const selectButton = document.createElement("button");
      selectButton.type = "button";
      selectButton.className = "source-picker-option-select";
      selectButton.textContent = dot + " " + label + " (" + s.kind + ")";
      selectButton.addEventListener("click", () => {
        sourceSelect.value = s.id;
        sourceSelect.dispatchEvent(new Event("change"));
        closeSourcePicker();
      });
      row.appendChild(selectButton);
      if (s.kind === "local") {
        const removeButton = document.createElement("button");
        removeButton.type = "button";
        removeButton.className = "source-picker-option-remove";
        removeButton.title = "Remove " + label;
        removeButton.setAttribute("aria-label", "Remove " + label);
        removeButton.textContent = "\u2715";
        removeButton.addEventListener("click", (event) => {
          event.stopPropagation();
          void removeSourceById(s.id, label);
        });
        row.appendChild(removeButton);
      }
      sourcePickerList.appendChild(row);
    }
    const selected = lastSources.find((s) => s.id === sourceSelect.value);
    sourcePickerLabel.textContent = selected
      ? (selected.connected ? "\u25cf" : "\u25cb") + " " + (selected.label || selected.value) + " (" + selected.kind + ")"
      : "No sources yet";
  }

  sourcePickerTrigger.addEventListener("click", () => {
    if (sourcePickerMenu.hidden) openSourcePicker(); else closeSourcePicker();
  });
  document.addEventListener("click", (event) => {
    if (sourcePickerMenu.hidden) return;
    if (sourcePickerMenu.contains(event.target) || sourcePickerTrigger.contains(event.target)) return;
    closeSourcePicker();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && !sourcePickerMenu.hidden) {
      closeSourcePicker();
      sourcePickerTrigger.focus();
    }
  });

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
    for (const w of sortWorkflows(workflows)) {
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
      // Skip the whole warnings block for rows with nothing to report -
      // showing an empty "0 active warnings" section on every row just adds
      // noise to a table where most workflows are unremarkable.
      if ((w.warnings || []).length > 0) {
        warningCell.dataset.warningContext = "workflow";
        warningCell.dataset.gaggle = gaggle;
        warningCell.dataset.workflow = name;
        warningCell.innerHTML = renderConfigurationWarnings(
          w.warnings || [],
          "workflow",
          { dismissedWarningKeys: dismissedConfigurationWarnings },
        );
      }
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
    updateSortIndicators("workflows-table", workflowSortKey, workflowSortDir);

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
  let workflowSortKey = "name";
  let workflowSortDir = "asc";
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

    populateInsightScopeOptions(gaggles, workflows);
  }

  function populateInsightScopeOptions(gaggles, workflows) {
    const prevValue = insightScopeSelect.value;
    const gaggleNames = gaggles.length ? gaggles.map((g) => g.name) : [...new Set(workflows.map((w) => (w.identity ? w.identity.gaggle : w.gaggle)))];
    const options = ['<option value="instance">Instance</option>'];
    for (const name of gaggleNames.filter(Boolean)) {
      const scope = { kind: "gaggle", gaggle: name };
      options.push('<option value="' + escapeHtml(insightScopeValue(scope)) + '">' + escapeHtml(insightScopeLabel(scope)) + "</option>");
    }
    const seenWorkflows = new Set();
    for (const w of workflows) {
      const gaggle = w.identity ? w.identity.gaggle : w.gaggle;
      const name = w.identity ? w.identity.name : w.name;
      if (!gaggle || !name) continue;
      const key = gaggle + "|" + name;
      if (seenWorkflows.has(key)) continue;
      seenWorkflows.add(key);
      const scope = { kind: "workflow", gaggle, workflow: name };
      options.push('<option value="' + escapeHtml(insightScopeValue(scope)) + '">' + escapeHtml(insightScopeLabel(scope)) + "</option>");
    }
    insightScopeSelect.innerHTML = options.join("");
    if ([...insightScopeSelect.options].some((o) => o.value === prevValue)) insightScopeSelect.value = prevValue;
    populateCostScopeOptionsFromHtml(options, prevValue);
  }

  function populateCostScopeOptionsFromHtml(options, preferredValue) {
    if (!costScopeSelect) return;
    const prevValue = costScopeSelect.value || preferredValue;
    costScopeSelect.innerHTML = options.join("");
    if ([...costScopeSelect.options].some((o) => o.value === prevValue)) costScopeSelect.value = prevValue;
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

  function syncViewUrl(runId = selectedRunId, { push = false } = {}) {
    const query = new URLSearchParams(encodeViewState(currentFilters(), runId));
    const next = query.toString();
    const url = next ? "?" + next : window.location.pathname;
    // Most filter/tab tweaks replace in place to avoid spamming history, but
    // opening a run pushes a real entry so a browser/mouse "back" lands on
    // the dashboard (handled by the popstate listener) instead of leaving
    // the document entirely.
    if (push) window.history.pushState(null, "", url);
    else window.history.replaceState(null, "", url);
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

  function sortWorkflows(workflows) {
    const dir = workflowSortDir === "asc" ? 1 : -1;
    const value = (w, key) => {
      if (key === "name") return (w.identity ? w.identity.name : w.name) || "";
      if (key === "gaggle") return (w.identity ? w.identity.gaggle : w.gaggle) || "";
      if (key === "trigger") return (w.triggers || []).map((t) => t.type || t.kind).filter(Boolean).join(", ");
      if (key === "inFlight") return w.concurrency?.activeRuns ?? -1;
      if (key === "max") return w.concurrency?.maxConcurrentRuns ?? -1;
      return "";
    };
    return [...workflows].sort((a, b) => {
      const av = value(a, workflowSortKey);
      const bv = value(b, workflowSortKey);
      if (av < bv) return -1 * dir;
      if (av > bv) return 1 * dir;
      return 0;
    });
  }

  function updateSortIndicators(tableId = "runs-table", activeKey = sortKey, activeDir = sortDir) {
    document.querySelectorAll("#" + tableId + " th[data-sort]").forEach((th) => {
      const button = th.querySelector("button");
      const label = button.textContent.replace(/\s*[\u25b2\u25bc]$/, "");
      button.textContent = label;
      th.setAttribute("aria-sort", th.dataset.sort === activeKey ? (activeDir === "asc" ? "ascending" : "descending") : "none");
      if (th.dataset.sort === activeKey) {
        const arrow = document.createElement("span");
        arrow.className = "sort-arrow";
        arrow.textContent = activeDir === "asc" ? "\u25b2" : "\u25bc";
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
  document.querySelectorAll("#workflows-table th[data-sort]").forEach((th) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "sort-button";
    button.textContent = th.textContent;
    th.replaceChildren(button);
    button.addEventListener("click", () => {
      const key = th.dataset.sort;
      if (workflowSortKey === key) {
        workflowSortDir = workflowSortDir === "asc" ? "desc" : "asc";
      } else {
        workflowSortKey = key;
        workflowSortDir = key === "inFlight" || key === "max" ? "desc" : "asc";
      }
      if (lastSnapshot) renderSnapshot(lastSnapshot);
    });
  });

  // ---- Insights tab: aggregate telemetry ----
  const INSIGHT_WINDOWS = ${JSON.stringify(INSIGHT_WINDOWS)};
  const INSIGHT_WINDOW_MS = ${JSON.stringify(INSIGHT_WINDOW_MS)};
  const INSIGHT_TREND_BUCKET_COUNTS = ${JSON.stringify(INSIGHT_TREND_BUCKET_COUNTS)};
  const COST_MAX_WINDOW_MS = ${COST_MAX_WINDOW_MS};
  const insightScopeSelect = document.getElementById("insight-scope");
  const insightWindowSelect = document.getElementById("insight-window");
  const insightStatusEl = document.getElementById("insight-status");
  const insightContentEl = document.getElementById("insight-content");
  let insightRequestSequence = 0;

  insightWindowSelect.innerHTML = INSIGHT_WINDOWS.map((w) => '<option value="' + escapeHtml(w.value) + '">' + escapeHtml(w.label) + "</option>").join("");
  insightWindowSelect.value = "24h";

  function resetInsightsForNewSource() {
    ++insightRequestSequence;
    insightScopeSelect.value = "instance";
    insightWindowSelect.value = "24h";
    insightStatusEl.textContent = "";
    insightContentEl.innerHTML = "";
  }

  async function loadInsights() {
    const sourceId = sourceSelect.value;
    if (!sourceId) return;
    const scope = parseInsightScope(insightScopeSelect.value);
    const windowValue = insightWindowSelect.value;
    const requestSequence = ++insightRequestSequence;
    insightStatusEl.textContent = "Loading…";
    try {
      const params = new URLSearchParams({ source: sourceId, ...insightRequestParams(scope, windowValue) });
      const res = await fetch("/api/insight-stats?" + params.toString());
      const data = await res.json();
      if (requestSequence !== insightRequestSequence || sourceId !== sourceSelect.value) return;
      if (!data.connected) {
        insightStatusEl.textContent = data.reason || "Could not load insights.";
        insightContentEl.innerHTML = "";
        return;
      }
      insightStatusEl.textContent = "";
      insightContentEl.innerHTML = renderInsightPanel(data.stats, scope, windowValue);
    } catch (err) {
      if (requestSequence !== insightRequestSequence || sourceId !== sourceSelect.value) return;
      insightStatusEl.textContent = portalRequestError(err);
      insightContentEl.innerHTML = "";
    }
  }

  insightScopeSelect.addEventListener("change", () => void loadInsights());
  insightWindowSelect.addEventListener("change", () => void loadInsights());

  // ---- Cost tab: selected-scope spend and external attribution ----
  const costScopeSelect = document.getElementById("cost-scope");
  const costWindowSelect = document.getElementById("cost-window");
  const costStatusEl = document.getElementById("cost-status");
  const costContentEl = document.getElementById("cost-content");
  const costLookupKind = document.getElementById("cost-lookup-kind");
  const costLookupProvider = document.getElementById("cost-lookup-provider");
  const costLookupId = document.getElementById("cost-lookup-id");
  const costLookupRun = document.getElementById("cost-lookup-run");
  const costLookupClear = document.getElementById("cost-lookup-clear");
  let costRequestSequence = 0;
  let costLookup = null;

  costWindowSelect.innerHTML = INSIGHT_WINDOWS.map((w) => '<option value="' + escapeHtml(w.value) + '">' + escapeHtml(w.label) + "</option>").join("");
  costWindowSelect.value = "7d";

  function resetCostForNewSource() {
    ++costRequestSequence;
    costScopeSelect.value = "instance";
    costWindowSelect.value = "7d";
    costLookup = null;
    costLookupId.value = "";
    costStatusEl.textContent = "";
    costContentEl.innerHTML = "";
  }

  async function fetchCostSummary(sourceId, params) {
    const response = await fetch("/api/cost-summary?" + new URLSearchParams({ source: sourceId, ...params }).toString());
    const data = await response.json();
    if (!data.connected) throw new Error(data.reason || "Could not load cost telemetry.");
    return data.costs;
  }

  async function loadCost() {
    const sourceId = sourceSelect.value;
    if (!sourceId) return;
    const scope = parseInsightScope(costScopeSelect.value);
    const windowValue = costWindowSelect.value;
    const requestSequence = ++costRequestSequence;
    costStatusEl.textContent = "Loading…";
    try {
      const statsParams = insightRequestParams(scope, windowValue);
      const [statsResponse, costResult] = await Promise.all([
        fetch("/api/insight-stats?" + new URLSearchParams({ source: sourceId, ...statsParams }).toString())
          .then((res) => res.json())
          .then((stats) => ({ ok: true, stats }))
          .catch((error) => ({ ok: false, error })),
        fetchCostSummary(sourceId, costSummaryRequestParams(windowValue))
          .then((costs) => ({ ok: true, costs }))
          .catch((error) => ({ ok: false, error })),
      ]);
      if (requestSequence !== costRequestSequence || sourceId !== sourceSelect.value) return;
      const costs = costResult.ok ? { ...costResult.costs, boundedAllTime: windowValue === "all" } : null;
      const warnings = [];
      if (!costResult.ok) warnings.push("Attributed costs unavailable: " + portalRequestError(costResult.error));
      const stats = statsResponse.ok && statsResponse.stats.connected ? statsResponse.stats.stats : null;
      if (!statsResponse.ok) {
        warnings.push("Selected-scope cost telemetry unavailable: " + portalRequestError(statsResponse.error));
      } else if (!statsResponse.stats.connected) {
        warnings.push(statsResponse.stats.reason || "Selected-scope cost telemetry is unavailable.");
      }
      let lookupCosts = null;
      if (costLookup) {
        try {
          lookupCosts = await fetchCostSummary(
            sourceId,
            costLookupRequestParams(costLookup.kind, costLookup.provider, costLookup.id, windowValue),
          );
        } catch (error) {
          warnings.push("Lookup unavailable: " + portalRequestError(error));
        }
        if (requestSequence !== costRequestSequence || sourceId !== sourceSelect.value) return;
      }
      costStatusEl.textContent = warnings.join(" ");
      costContentEl.innerHTML = renderCostPanel(stats, costs, scope, windowValue, lookupCosts);
    } catch (err) {
      if (requestSequence !== costRequestSequence || sourceId !== sourceSelect.value) return;
      costStatusEl.textContent = portalRequestError(err);
      costContentEl.innerHTML = "";
    }
  }

  costScopeSelect.addEventListener("change", () => void loadCost());
  costWindowSelect.addEventListener("change", () => void loadCost());
  costLookupRun.addEventListener("click", () => {
    const id = costLookupId.value.trim();
    const provider = costLookupProvider.value.trim();
    if (!id || !provider) {
      costStatusEl.textContent = "Provider and ID are required for cost lookup.";
      return;
    }
    costLookup = { kind: costLookupKind.value === "issue" ? "issue" : "pr", provider, id };
    void loadCost();
  });
  costLookupClear.addEventListener("click", () => {
    costLookup = null;
    costLookupId.value = "";
    void loadCost();
  });

  // ---- Work Items tab: bounded provider-action index and detail ----
  const workItemListControlsEl = document.getElementById("work-item-list-controls");
  const workItemStatusEl = document.getElementById("work-item-status");
  const workItemContentEl = document.getElementById("work-item-content");
  const workItemGaggleSelect = document.getElementById("work-item-gaggle");
  const workItemSearchInput = document.getElementById("work-item-search");
  const workItemKindButtons = [...document.querySelectorAll("[data-work-item-kind-filter]")];
  let workItemKind = "";
  let workItemSortKey = "lastActionAt";
  let workItemSortDir = "desc";
  let workItemRequestSequence = 0;
  let lastWorkItemPage = null;
  let selectedWorkItem = null;
  let workItemActionType = "all";
  const workItemDetailCache = new Map();

  function workItemCacheKey(sourceId, item) {
    return [sourceId, item.provider, item.repository, item.kind, item.externalId].join("|");
  }

  function updateWorkItemKindButtons() {
    for (const button of workItemKindButtons) {
      const selected = button.dataset.workItemKindFilter === workItemKind;
      button.setAttribute("aria-pressed", String(selected));
    }
  }

  function populateWorkItemGaggles(items) {
    const previous = workItemGaggleSelect.value;
    const options = [...new Set((items || []).map((item) => item.gaggle).filter(Boolean))]
      .sort((left, right) => left.localeCompare(right));
    workItemGaggleSelect.innerHTML = '<option value="">All gaggles</option>' +
      options.map((value) => '<option value="' + escapeHtml(value) + '">' + escapeHtml(value) + "</option>").join("");
    workItemGaggleSelect.value = options.includes(previous) ? previous : "";
  }

  function bindWorkItemList() {
    workItemContentEl.querySelectorAll("[data-work-item-provider]").forEach((button) => {
      button.addEventListener("click", () => void openWorkItem({
        provider: button.dataset.workItemProvider,
        repository: button.dataset.workItemRepository,
        kind: button.dataset.workItemKind,
        externalId: button.dataset.workItemId,
      }));
    });
    workItemContentEl.querySelectorAll("[data-work-item-sort]").forEach((button) => {
      button.addEventListener("click", () => {
        const key = button.dataset.workItemSort;
        if (workItemSortKey === key) {
          workItemSortDir = workItemSortDir === "asc" ? "desc" : "asc";
        } else {
          workItemSortKey = key;
          workItemSortDir = key === "lastActionAt" ? "desc" : "asc";
        }
        renderCurrentWorkItemList();
      });
    });
  }

  function renderCurrentWorkItemList() {
    selectedWorkItem = null;
    workItemListControlsEl.hidden = false;
    workItemContentEl.innerHTML = renderWorkItemList(
      lastWorkItemPage,
      workItemGaggleSelect.value,
      workItemSearchInput.value,
      workItemSortKey,
      workItemSortDir,
    );
    bindWorkItemList();
  }

  function bindWorkItemDetail() {
    document.getElementById("work-item-back")?.addEventListener("click", () => {
      ++workItemRequestSequence;
      workItemStatusEl.textContent = "";
      renderCurrentWorkItemList();
      document.querySelector("[data-work-item-provider]")?.focus();
    });
    document.getElementById("work-item-action-type")?.addEventListener("change", (event) => {
      workItemActionType = event.target.value;
      workItemContentEl.innerHTML = renderWorkItemDetail(selectedWorkItem, workItemActionType);
      bindWorkItemDetail();
      document.getElementById("work-item-action-type")?.focus();
    });
    workItemContentEl.querySelectorAll("[data-work-item-run]").forEach((button) =>
      button.addEventListener("click", () => void openRun(button.dataset.workItemRun, "work-items")));
  }

  function showWorkItemDetail(item) {
    selectedWorkItem = item;
    workItemActionType = "all";
    workItemListControlsEl.hidden = true;
    workItemStatusEl.textContent = "";
    workItemContentEl.innerHTML = renderWorkItemDetail(item, workItemActionType);
    bindWorkItemDetail();
    document.getElementById("work-item-back")?.focus();
  }

  async function openWorkItem(identity) {
    const sourceId = sourceSelect.value;
    if (!sourceId) return;
    const requestSequence = ++workItemRequestSequence;
    const cacheKey = workItemCacheKey(sourceId, identity);
    workItemListControlsEl.hidden = true;
    workItemStatusEl.textContent = "Loading\u2026";
    workItemContentEl.innerHTML = "";
    try {
      let item = workItemDetailCache.get(cacheKey);
      if (!item) {
        const params = new URLSearchParams({
          source: sourceId,
          provider: identity.provider,
          repository: identity.repository,
          kind: identity.kind,
          id: identity.externalId,
        });
        const response = await fetch("/api/work-item-detail?" + params.toString());
        const data = await response.json();
        if (requestSequence !== workItemRequestSequence || sourceId !== sourceSelect.value) return;
        if (!data.connected) throw new Error(data.reason || "Could not load work item detail.");
        item = data.workItem;
        workItemDetailCache.set(cacheKey, item);
      }
      if (requestSequence !== workItemRequestSequence || sourceId !== sourceSelect.value) return;
      showWorkItemDetail(item);
    } catch (error) {
      if (requestSequence !== workItemRequestSequence || sourceId !== sourceSelect.value) return;
      selectedWorkItem = null;
      workItemStatusEl.textContent = portalRequestError(error);
      workItemContentEl.innerHTML = '<button type="button" class="back" id="work-item-error-back">\u2190 Back to Work Items</button>';
      document.getElementById("work-item-error-back")?.addEventListener("click", () => {
        ++workItemRequestSequence;
        workItemStatusEl.textContent = "";
        renderCurrentWorkItemList();
      });
      document.getElementById("work-item-error-back")?.focus();
    }
  }

  function resetWorkItemsForNewSource() {
    ++workItemRequestSequence;
    workItemKind = "";
    lastWorkItemPage = null;
    selectedWorkItem = null;
    workItemActionType = "all";
    workItemDetailCache.clear();
    workItemGaggleSelect.innerHTML = '<option value="">All gaggles</option>';
    workItemSearchInput.value = "";
    workItemListControlsEl.hidden = false;
    workItemStatusEl.textContent = "";
    workItemContentEl.innerHTML = "";
    updateWorkItemKindButtons();
  }

  async function loadWorkItems() {
    const sourceId = sourceSelect.value;
    if (!sourceId) return;
    const requestSequence = ++workItemRequestSequence;
    selectedWorkItem = null;
    workItemListControlsEl.hidden = false;
    workItemStatusEl.textContent = "Loading\u2026";
    workItemContentEl.innerHTML = "";
    try {
      const params = new URLSearchParams({ source: sourceId, limit: "200" });
      if (workItemKind) params.set("kind", workItemKind);
      const response = await fetch("/api/work-items?" + params.toString());
      const data = await response.json();
      if (requestSequence !== workItemRequestSequence || sourceId !== sourceSelect.value) return;
      if (!data.connected) {
        workItemStatusEl.textContent = data.reason || "Could not load work items.";
        return;
      }
      lastWorkItemPage = data.workItems || { items: [], hasMore: false };
      populateWorkItemGaggles(lastWorkItemPage.items);
      workItemStatusEl.textContent = "";
      renderCurrentWorkItemList();
    } catch (error) {
      if (requestSequence !== workItemRequestSequence || sourceId !== sourceSelect.value) return;
      workItemStatusEl.textContent = portalRequestError(error);
      workItemContentEl.innerHTML = "";
    }
  }

  for (const button of workItemKindButtons) {
    button.addEventListener("click", () => {
      const nextKind = button.dataset.workItemKindFilter;
      if (nextKind === workItemKind) return;
      workItemKind = nextKind;
      updateWorkItemKindButtons();
      void loadWorkItems();
    });
  }
  workItemGaggleSelect.addEventListener("change", renderCurrentWorkItemList);
  workItemSearchInput.addEventListener("input", renderCurrentWorkItemList);

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
      '<svg id="graph-svg" tabindex="0" role="group" aria-label="Workflow graph" viewBox="0 0 ' +
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
  const insightWindowRange = ${insightWindowRange.toString()};
  const insightPreviousWindowRange = ${insightPreviousWindowRange.toString()};
  const parseInsightScope = ${parseInsightScope.toString()};
  const insightScopeValue = ${insightScopeValue.toString()};
  const insightScopeLabel = ${insightScopeLabel.toString()};
  const insightScopeApiParams = ${insightScopeApiParams.toString()};
  const insightRequestParams = ${insightRequestParams.toString()};
  const isInInsightScope = ${isInInsightScope.toString()};
  const insightUsageForScope = ${insightUsageForScope.toString()};
  const insightFormatRate = ${insightFormatRate.toString()};
  const insightFormatDuration = ${insightFormatDuration.toString()};
  const insightFormatTokens = ${insightFormatTokens.toString()};
  const insightFormatCost = ${insightFormatCost.toString()};
  const insightFormatSamples = ${insightFormatSamples.toString()};
  const insightFormatBucketLabel = ${insightFormatBucketLabel.toString()};
  const insightGaggleMetric = ${insightGaggleMetric.toString()};
  const insightRunMetric = ${insightRunMetric.toString()};
  const insightStageOutcomeMetric = ${insightStageOutcomeMetric.toString()};
  const insightSumGaggles = ${insightSumGaggles.toString()};
  const insightOutcomeSummary = ${insightOutcomeSummary.toString()};
  const insightOutcomeBreakdown = ${insightOutcomeBreakdown.toString()};
  const renderInsightOutcomeRow = ${renderInsightOutcomeRow.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderInsightOutcomeSection = ${renderInsightOutcomeSection.toString()};
  const insightUnmeasuredLabel = ${insightUnmeasuredLabel.toString()};
  const insightFormatSeconds = ${insightFormatSeconds.toString()};
  const insightHasCurationHealth = ${insightHasCurationHealth.toString()};
  const renderInsightCurationSection = ${renderInsightCurationSection.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderInsightCreditSection = ${renderInsightCreditSection.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderInsightUsageSection = ${renderInsightUsageSection.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const insightCurrentTrendBuckets = ${insightCurrentTrendBuckets.toString()};
  const renderInsightTrendSection = ${renderInsightTrendSection.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderInsightStageSection = ${renderInsightStageSection.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderInsightPanel = ${renderInsightPanel.toString()};
  const costSummaryRequestParams = ${costSummaryRequestParams.toString()};
  const costLookupRequestParams = ${costLookupRequestParams.toString()};
  const costAmountByUnit = ${costAmountByUnit.toString()};
  const formatCostAmount = ${formatCostAmount.toString()};
  const costAggregateNativeLabel = ${costAggregateNativeLabel.toString()};
  const costAggregateNormalizedLabel = ${costAggregateNormalizedLabel.toString()};
  const costAggregateComparableValue = ${costAggregateComparableValue.toString()};
  const costCoverageLabel = ${costCoverageLabel.toString()};
  const deriveExternalCostRows = ${deriveExternalCostRows.toString()};
  const compareExternalCostRows = ${compareExternalCostRows.toString()};
  const renderCostSummarySection = ${renderCostSummarySection.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderCostTrendSection = ${renderCostTrendSection.toString()};
  const renderInstanceCostRollupSection = ${renderInstanceCostRollupSection.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderExternalCostBreakdownSection = ${renderExternalCostBreakdownSection.toString()
        .replaceAll("safeAssociationUrl", "safeExternalUrl")
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderCostPanel = ${renderCostPanel.toString()};
  const workItemLabel = ${workItemLabel.toString()};
  const humanizeWorkItemOperation = ${humanizeWorkItemOperation.toString()};
  const formatWorkItemTimestamp = ${formatWorkItemTimestamp.toString()};
  const formatWorkItemCost = ${formatWorkItemCost.toString()};
  const filterWorkItems = ${filterWorkItems.toString()};
  const sortWorkItems = ${sortWorkItems.toString()};
  const workItemKindIcon = ${workItemKindIcon.toString()};
  const renderWorkItemStatusBadge = ${renderWorkItemStatusBadge.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderWorkItemListHeader = ${renderWorkItemListHeader.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderWorkItemList = ${renderWorkItemList.toString()
        .replaceAll("escapeAssociationHtml", "escapeHtml")};
  const renderWorkItemDetail = ${renderWorkItemDetail.toString()
        .replaceAll("safeAssociationUrl", "safeExternalUrl")
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

  async function openRun(runId, origin = "runs", { fromPopstate = false } = {}) {
    const sourceId = sourceSelect.value;
    if (!sourceId) {
      errorEl.textContent = "Choose a source before opening a run.";
      sourceSelect.focus();
      return;
    }
    const requestSequence = ++runRequestSequence;
    const isNewRun = selectedRunId !== runId;
    runOrigin = origin;
    document.getElementById("run-back").textContent =
      runOrigin === "work-items" ? "Back to Work Items" : "Back to runs";
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
    syncViewUrl(runId, { push: isNewRun && !fromPopstate });
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

  // Closes the run detail view and returns to the dashboard. Shared by the
  // explicit "Back" button and the popstate handler (browser/mouse back
  // navigation), so both paths always leave the DOM in a rendered state
  // instead of a blank one.
  function closeRunView(previousRunId, options = {}) {
    const { syncUrl = true } = options;
    selectedRunId = "";
    ++runRequestSequence;
    ++stageInspectorRequestSequence;
    selectedStageName = "";
    activeStageInspectorView = "fields";
    runViewEl.style.display = "none";
    dashboardEl.style.display = "block";
    if (runOrigin === "work-items") {
      const actionButton = [...workItemContentEl.querySelectorAll("[data-work-item-run]")]
        .find((button) => button.dataset.workItemRun === previousRunId);
      actionButton?.focus();
      if (syncUrl) syncViewUrl();
      return;
    }
    activateInternalTab(dashboardEl, "runs", true);
    const row = [...runsBody.querySelectorAll("[data-run-id]")].find((row) => row.dataset.runId === previousRunId);
    row?.querySelector(".table-link")?.focus();
    if (syncUrl) syncViewUrl();
  }

  document.getElementById("run-back").addEventListener("click", () => {
    closeRunView(selectedRunId);
  });

  // The extension page is a single long-lived document with no server-side
  // routing, so a real browser/mouse "back" navigation has no other page to
  // land on: without a listener it just leaves the last-rendered DOM in
  // place (or blanks it) instead of restoring the dashboard or a prior run.
  // Re-derive the visible state from the URL on every popstate so back/
  // forward navigation always re-renders something instead of going blank.
  window.addEventListener("popstate", () => {
    const decoded = decodeViewState(window.location.search);
    if (decoded.selectedRun) {
      if (decoded.selectedRun !== selectedRunId) {
        void openRun(decoded.selectedRun, runOrigin || "runs", { fromPopstate: true });
      }
    } else if (selectedRunId) {
      closeRunView(selectedRunId, { syncUrl: false });
    }
  });

  async function loadSnapshot() {
    const sourceId = sourceSelect.value;
    const sourceChanged = snapshotSourceId !== sourceId;
    if (sourceChanged) {
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
        resetInsightsForNewSource();
        resetCostForNewSource();
        resetWorkItemsForNewSource();
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
      if (data) {
        renderSnapshot(data);
        if (sourceChanged && activeDashboardTab === "insights") void loadInsights();
        if (sourceChanged && activeDashboardTab === "cost") void loadCost();
        if (sourceChanged && activeDashboardTab === "work-items") void loadWorkItems();
      }
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

  document.getElementById("refresh").addEventListener("click", () => {
    workflowDetailCache.clear();
    workItemDetailCache.clear();
    void refreshAll().then(() => {
      if (activeDashboardTab === "work-items") void loadWorkItems();
    });
  });
  async function changeSource() {
    liveConnectionEstablished = false;
    reconnectAttemptCount = 0;
    workflowDetailCache.clear();
    if (eventSource) eventSource.close();
    renderSourcePicker();
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
    document.getElementById("connect-source-dialog").close();
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

  const connectSourceDialog = document.getElementById("connect-source-dialog");
  document.getElementById("connect-source-button").addEventListener("click", () => {
    closeSourcePicker();
    connectSourceDialog.showModal();
  });
  document.getElementById("connect-source-close").addEventListener("click", () => {
    connectSourceDialog.close();
  });

  const discoverStatusEl = document.getElementById("discover-local-status");
  const discoverResultsEl = document.getElementById("discover-local-results");
  document.getElementById("discover-local-instances").addEventListener("click", async () => {
    discoverStatusEl.textContent = "Scanning for local instances\u2026";
    discoverResultsEl.innerHTML = "";
    try {
      const response = await fetch("/api/discover-local-sources");
      const data = await response.json();
      if (!response.ok || data.error) throw new Error(data.error || "Could not scan for local instances.");
      const candidates = data.candidates || [];
      if (candidates.length === 0) {
        discoverStatusEl.textContent = "No unregistered local instances found.";
        return;
      }
      discoverStatusEl.textContent = candidates.length + " found:";
      for (const candidate of candidates) {
        const li = document.createElement("li");
        const label = document.createElement("span");
        label.textContent = candidate.root;
        const addButton = document.createElement("button");
        addButton.type = "button";
        addButton.textContent = "Add";
        addButton.addEventListener("click", async () => {
          addButton.disabled = true;
          try {
            await connectSource({ kind: "local", value: candidate.root });
            li.remove();
          } catch (err) {
            errorEl.textContent = portalRequestError(err);
            addButton.disabled = false;
          }
        });
        li.append(label, addButton);
        discoverResultsEl.appendChild(li);
      }
    } catch (err) {
      discoverStatusEl.textContent = portalRequestError(err);
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
