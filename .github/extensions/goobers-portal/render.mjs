// Renders the goobers-portal HTML shell. Kept out of extension.mjs so the
// wiring file stays focused on SDK plumbing.

import {
  decodeStreamEvent,
  decodeViewState,
  deriveFreshnessState,
  isInvalidCursorError,
  mergeRunPage,
  shouldApplyRestoredFilters,
} from "./ux.mjs";

function escapeAssociationHtml(value) {
    return String(value).replace(/[&<>"']/g, (character) => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
    })[character]);
}

function safeAssociationUrl(value) {
    try {
        const url = new URL(value);
        return url.protocol === "http:" || url.protocol === "https:" ? url.href : "";
    } catch {
        return "";
    }
}

export function renderRunAssociations(operator) {
    const links = [];
    const issueURL = safeAssociationUrl(operator?.issue?.url);
    if (issueURL) {
        const title = String(operator.issue.title || "").trim();
        const label = "Issue #" + operator.issue.number + (title ? ": " + title : "");
        links.push('<a class="run-association-link" href="' + escapeAssociationHtml(issueURL) +
            '" target="_blank" rel="noopener noreferrer">' + escapeAssociationHtml(label) + "</a>");
    }
    const pullURL = safeAssociationUrl(operator?.pullRequest?.url);
    if (pullURL) {
        const title = String(operator.pullRequestTitle || "").trim();
        const label = "PR #" + operator.pullRequest.id + (title ? ": " + title : "");
        links.push('<a class="run-association-link" href="' + escapeAssociationHtml(pullURL) +
            '" target="_blank" rel="noopener noreferrer">' + escapeAssociationHtml(label) + "</a>");
    }
    return links.length ? '<div class="run-associations">' + links.join("") + "</div>" : "\u2014";
}

export function renderHtml(instanceId, themePreference = "system") {
    return `<!doctype html>
<html data-theme-preference="${themePreference}">
<head>
<meta charset="utf-8" />
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
  main { padding: 16px; }
  .muted { color: var(--text-color-muted, #656d76); }
  .cards {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
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
  button:focus-visible, select:focus-visible, input:focus-visible, #graph-svg:focus-visible {
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
  #error { color: var(--true-color-red, #cf222e); margin-bottom: 12px; white-space: pre-wrap; }
  #empty-state { padding: 32px 0; }
  #empty-state ol { padding-left: 20px; }
  #needs-you { margin-bottom: 20px; }
  .attention-list { display: grid; gap: 8px; }
  .attention-item {
    display: grid;
    grid-template-columns: minmax(110px, auto) 1fr auto;
    gap: 8px 12px;
    align-items: baseline;
    border: 1px solid var(--true-color-red-muted, #cf222e66);
    border-left: 4px solid var(--true-color-red, #cf222e);
    border-radius: 6px;
    padding: 9px 12px;
  }
  .attention-item a { color: inherit; }
  .attention-reason { min-width: 0; overflow-wrap: anywhere; }
  .attention-action { color: var(--text-color-muted, #656d76); font-size: 12px; }
  .freshness { color: var(--text-color-muted, #656d76); font-size: 12px; }
  @media (max-width: 640px) {
    .attention-item { grid-template-columns: 1fr; gap: 3px; }
    main { padding: 10px; }
    table { display: block; overflow-x: auto; white-space: nowrap; }
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
  .run-associations { display: flex; flex-direction: column; gap: 3px; min-width: 180px; }
  .run-association-link {
    color: var(--true-color-blue, #0969da);
    max-width: 320px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .run-association-link:hover { text-decoration: underline; }
  .kv-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 10px; margin: 12px 0 20px; }
  .kv { border: 1px solid var(--border-color-default, #d0d7de); border-radius: 8px; padding: 10px 12px; }
  .kv-wide { grid-column: 1 / -1; }
  .kv .label { color: var(--text-color-muted, #656d76); font-size: 12px; }
  .kv .value { font-size: 14px; margin-top: 2px; word-break: break-word; }
  .kv .value a { color: inherit; }
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
  .node-rect { fill: var(--border-color-default, #d0d7de33); stroke: var(--border-color-default, #d0d7de); }
  .node-rect.visited { fill: var(--true-color-blue-muted, #ddf4ff); stroke: var(--true-color-blue, #0969da); }
  .node-rect.completed { fill: var(--true-color-green-muted, #dafbe1); stroke: var(--true-color-green, #1a7f37); }
  .node-rect.running { fill: var(--true-color-blue-muted, #ddf4ff); stroke: var(--true-color-blue, #0969da); stroke-width: 2; }
  .node-rect.failed { fill: var(--true-color-red-muted, #ffebe9); stroke: var(--true-color-red, #cf222e); }
  .node-rect.terminal { stroke-width: 3; }
  .node-label { font-size: 10px; fill: var(--text-color-default, #1f2328); }
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
</style>
</head>
<body>
<header>
  <h1>Goobers Portal</h1>
  <div class="toolbar">
    <select id="theme-select" aria-label="Color theme" title="Color theme">
      <option value="system">System theme</option>
      <option value="light">Light theme</option>
      <option value="dark">Dark theme</option>
    </select>
    <select id="source-select"><option value="">No sources yet</option></select>
    <input id="run-jump" type="text" placeholder="Run ID" aria-label="Jump to a run" style="max-width: 180px;" />
    <button id="run-jump-button" type="button">Jump</button>
    <button id="refresh">Refresh</button>
  </div>
</header>
<main>
  <details id="add-source-details">
    <summary>Connect a source&hellip;</summary>
    <div class="add-form">
      <input id="local-root" placeholder="Local instance root path (e.g. C:\\\\path\\\\to\\\\instance)" />
      <button id="browse-local" title="Browse folders">&#128193; Browse</button>
      <button id="add-local">Add local</button>
    </div>
    <div class="add-form">
      <input id="remote-url" placeholder="Remote control-plane URL (e.g. http://10.0.0.5:8080)" />
      <input id="remote-token" placeholder="Bearer token (optional)" style="flex: 0 0 200px" />
      <button id="add-remote">Add remote</button>
    </div>
    <div class="add-form">
      <input id="github-workflow-url" placeholder="GitHub Actions workflow URL (https://github.com/owner/repo/actions/workflows/file.yml)" />
      <button id="add-github">Connect to GitHub</button>
    </div>
  </details>
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
  <div id="error"></div>
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
    <div class="cards" id="cards"></div>
    <section id="needs-you" aria-labelledby="needs-you-heading">
      <h2 id="needs-you-heading">Needs you <span class="freshness" id="freshness" aria-live="polite"></span></h2>
      <div id="attention-list"></div>
    </section>
    <section>
      <h2>Workflows</h2>
      <table id="workflows-table">
        <thead>
          <tr><th>Workflow</th><th>Gaggle</th><th>Trigger</th><th>In flight</th><th>Max</th><th>Enabled</th></tr>
        </thead>
        <tbody></tbody>
      </table>
    </section>
    <section>
      <h2>Runs</h2>
      <div class="filters-bar" id="runs-filters">
        <select id="filter-gaggle"><option value="">All gaggles</option></select>
        <select id="filter-workflow"><option value="">All workflows</option></select>
        <select id="filter-phase">
          <option value="">All phases</option>
          <option value="running">running</option>
          <option value="completed">completed</option>
          <option value="failed">failed</option>
          <option value="aborted">aborted</option>
          <option value="escalated">escalated</option>
        </select>
        <select id="filter-trigger">
          <option value="">All triggers</option>
          <option value="manual">manual</option>
          <option value="schedule">schedule</option>
          <option value="item">item</option>
          <option value="webhook">webhook</option>
        </select>
        <input id="filter-stage" type="text" placeholder="Stage" title="Stage name (daemon sources)" />
        <select id="filter-outcome">
          <option value="">All outcomes</option>
          <option value="finished">finished</option>
          <option value="terminal">terminal</option>
          <option value="success">success</option>
          <option value="failure">failure</option>
          <option value="other">other</option>
        </select>
        <select id="filter-population">
          <option value="">All populations</option>
          <option value="attempts">attempts</option>
          <option value="measured">measured</option>
          <option value="token-measured">token measured</option>
          <option value="premium-measured">premium measured</option>
          <option value="cost-measured">cost measured</option>
          <option value="retry-waste">retry waste</option>
        </select>
        <label class="filter-toggle"><input id="filter-show-no-work" type="checkbox" /> Show no-work</label>
        <select id="saved-filter-presets" aria-label="Saved filters">
          <option value="">Saved filters</option>
        </select>
        <button id="save-filter-preset" type="button">Save</button>
        <input id="filter-since" type="datetime-local" title="Since" />
        <input id="filter-until" type="datetime-local" title="Until" />
        <button id="filters-clear">Clear filters</button>
      </div>
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
      <div style="margin-top: 10px; display: flex; justify-content: flex-end;">
        <button id="runs-load-more" type="button" style="display:none">Load more</button>
      </div>
    </section>
  </div>
  <div id="run-view">
    <button class="back" id="run-back">&larr; Back to runs</button>
    <div id="run-error" style="color: var(--true-color-red, #cf222e);"></div>
    <div id="run-content"></div>
  </div>
</main>
<script>
(function () {
  const errorEl = document.getElementById("error");
  const emptyEl = document.getElementById("empty-state");
  const dashboardEl = document.getElementById("dashboard");
  const cardsEl = document.getElementById("cards");
  const attentionListEl = document.getElementById("attention-list");
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
  let restoredRunId = new URLSearchParams(window.location.search).get("run") || "";
  // gaggle/workflow -> desired enabled state, for toggles the daemon hasn't
  // confirmed yet. Kept outside the render pass so the "Saving…" label survives
  // background-poll re-renders.
  const pendingToggles = new Map();

  function portalRequestError(err) {
    const message = String(err && err.message ? err.message : err);
    if (message === "Failed to fetch" || message.includes("NetworkError")) {
      return "Portal extension connection was lost. Close and reopen this canvas to reconnect.";
    }
    return message;
  }

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
    const key = gaggle + "/" + name;
    // renderSnapshot() clears #error, so failures must be re-applied after the
    // final re-render rather than set before it.
    let failure = "";
    pendingToggles.set(key, desired);
    await loadSnapshot();
    try {
      const res = await fetch("/api/set-workflow-enabled", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ source: sourceSelect.value, gaggle, workflow: name, enabled: desired }),
      });
      const data = await res.json();
      if (!data.ok) {
        failure = "Failed to update " + name + ": " + (data.reason || "unknown error");
      } else {
        const deadline = Date.now() + 30000;
        let confirmed = false;
        while (Date.now() < deadline) {
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
        }
      }
    } catch (err) {
      failure = "Failed to update " + name + ": " + (err.message || err);
    } finally {
      pendingToggles.delete(key);
      await loadSnapshot();
      if (failure) errorEl.textContent = failure;
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
    try { return new Date(v).toLocaleString(); } catch { return v; }
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
    const markup = '<div class="attention-list">' + visible.map((item) => {
      const run = (runs || []).find((candidate) => (candidate.runId || candidate.id) === item.id);
      const runLabel = escapeHtml(item.id || "unknown run");
      const stage = item.stage ? " · stage " + escapeHtml(item.stage) : "";
      const elapsed = item.elapsedMillis == null ? "" : " · " + Math.round(item.elapsedMillis / 60000) + "m";
      return '<div class="attention-item">' +
        '<strong>' + escapeHtml(item.phase) + '</strong>' +
        '<span class="attention-reason">' +
        (run ? '<a href="#run=' + encodeURIComponent(item.id) + '" data-attention-run="' + escapeHtml(item.id) + '">' : "") +
        '<code>' + runLabel + '</code> ' + escapeHtml(item.workflow) + stage + elapsed +
        (run ? "</a>" : "") + '<br />' + escapeHtml(item.reason) + '</span>' +
        '<span class="attention-action">' + escapeHtml(item.nextAction) +
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
    updateStartBar(data);
    if (!data.connected) {
      emptyEl.style.display = data.reason ? "block" : "none";
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
    dashboardEl.style.display = "block";

    lastCapabilities = data.capabilities || {};
    const workflows = data.workflows || [];
    const runs = data.runs || [];
    lastUpdatedAt = Date.now();
    const freshness = deriveFreshnessState({
      lastUpdatedAt,
      connected: true,
      mode: data.mode === "daemon" ? "daemon" : "polling",
      now: Date.now(),
    });
    setFreshnessState(freshness, lastUpdatedAt);
    renderAttention(data.attention, runs);
    const inFlight = workflows.reduce((n, w) => n + (w.concurrency?.activeRuns || 0), 0);

    cardsEl.innerHTML = "";
    const cards = [
      ["Instance", data.instance?.name || "\u2014"],
      ["Workflows", workflows.length],
      ["In flight", inFlight],
      ["Recent runs", runs.length],
      ["Warnings", (data.instance?.warnings || []).length],
    ];
    for (const [label, value] of cards) {
      const div = document.createElement("div");
      div.className = "card";
      div.innerHTML = '<div class="label">' + label + '</div><div class="value">' + value + "</div>";
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
        "<td" + nameTitleAttr + "><code>" + escapeHtml(name) + "</code></td>" +
        "<td>" + escapeHtml(gaggle) + "</td>" +
        "<td>" + escapeHtml(triggerLabel) + "</td>" +
        "<td>" + (w.concurrency?.activeRuns ?? "\u2014") + "</td>" +
        "<td>" + (w.concurrency?.maxConcurrentRuns ?? "\u2014") + "</td>" +
        '<td class="enabled-cell"></td>';
      tr.classList.add("clickable-row");
      tr.title = "Filter runs to this workflow";
      tr.addEventListener("click", (ev) => {
        if (ev.target.closest(".enabled-cell")) return;
        filterToWorkflow(gaggle, name);
      });
      workflowsBody.appendChild(tr);

      const enabledCell = tr.querySelector(".enabled-cell");
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
          toggleWorkflow(gaggle, name, !anyEnabled);
        });
        wrap.appendChild(btn);

        enabledCell.appendChild(wrap);
      }
    }
    if (workflows.length === 0) {
      workflowsBody.innerHTML = '<tr><td colspan="6" class="muted">No workflows configured.</td></tr>';
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

  function populateFilterOptions(gaggles, workflows) {
    const prevGaggle = filterGaggle.value;
    const gaggleNames = gaggles.length ? gaggles.map((g) => g.name) : [...new Set(workflows.map((w) => (w.identity ? w.identity.gaggle : w.gaggle)))];
    filterGaggle.innerHTML = '<option value="">All gaggles</option>' + gaggleNames.filter(Boolean).map((n) => '<option value="' + escapeHtml(n) + '">' + escapeHtml(n) + "</option>").join("");
    if (gaggleNames.includes(prevGaggle)) filterGaggle.value = prevGaggle;

    const prevWorkflow = filterWorkflow.value;
    const workflowNames = [...new Set(workflows.map((w) => (w.identity ? w.identity.name : w.name)))].filter(Boolean);
    filterWorkflow.innerHTML = '<option value="">All workflows</option>' + workflowNames.map((n) => '<option value="' + escapeHtml(n) + '">' + escapeHtml(n) + "</option>").join("");
    if (workflowNames.includes(prevWorkflow)) filterWorkflow.value = prevWorkflow;
  }

  function currentFilters() {
    const f = {};
    if (filterGaggle.value) f.gaggle = filterGaggle.value;
    if (filterWorkflow.value) f.workflow = filterWorkflow.value;
    if (filterPhase.value) f.phase = filterPhase.value;
    if (filterTrigger.value) f.trigger = filterTrigger.value;
    if (filterStage.value.trim()) f.stage = filterStage.value.trim();
    if (filterOutcome.value) f.outcome = filterOutcome.value;
    if (filterPopulation.value) f.population = filterPopulation.value;
    if (filterNoWork.checked) f.showNoWork = true;
    if (filterSince.value) f.since = new Date(filterSince.value).toISOString();
    if (filterUntil.value) f.until = new Date(filterUntil.value).toISOString();
    return f;
  }

  function syncViewUrl(runId = "") {
    const query = new URLSearchParams(encodeViewState(currentFilters(), runId));
    const next = query.toString();
    window.history.replaceState(null, "", next ? "?" + next : window.location.pathname);
  }

  function restoreFiltersFromUrl() {
    if (restoredFilters) return;
    restoredFilters = true;
    const { filters, selectedRun } = decodeViewState(window.location.search);
    restoredRunId = selectedRun;
    for (const [key, element] of [
      ["gaggle", filterGaggle], ["workflow", filterWorkflow], ["phase", filterPhase],
      ["trigger", filterTrigger], ["stage", filterStage], ["outcome", filterOutcome],
      ["population", filterPopulation], ["since", filterSince], ["until", filterUntil],
    ]) {
      const value = filters[key];
      if (typeof value === "string") {
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
      if (!hasStage) el.value = "";
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
      const label = th.textContent.replace(/\s*[\u25b2\u25bc]$/, "");
      th.textContent = label;
      if (th.dataset.sort === sortKey) {
        const arrow = document.createElement("span");
        arrow.className = "sort-arrow";
        arrow.textContent = sortDir === "asc" ? "\u25b2" : "\u25bc";
        th.appendChild(arrow);
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
      const associations = renderRunAssociations(r.operator);
      tr.className = "clickable-row";
      tr.dataset.runId = runId;
      tr.innerHTML =
        "<td><code>" + runId + "</code>" + actionsLink + "</td>" +
        "<td>" + (r.workflow || "") + "</td>" +
        "<td>" + (r.gaggle || "") + "</td>" +
        "<td>" + escapeHtml(r.trigger?.kind || "\u2014") + "</td>" +
        '<td><span class="phase">' + (r.phase || "") + "</span></td>" +
        "<td>" + associations + "</td>" +
        "<td>" + fmtTime(r.startedAt) + "</td>" +
        "<td>" + fmtTime(r.lastActivityAt) + "</td>";
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

  function loadSavedFilters() {
    try {
      const entries = JSON.parse(localStorage.getItem("goobers-portal-filter-presets") || "{}");
      const options = ["<option value=\"\">Saved filters</option>"];
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
        if (value[key] !== undefined) element.value = typeof value[key] === "string" ? value[key] : String(value[key]);
      }
      filterNoWork.checked = value.showNoWork === true;
      setAdvancedFilterSupport(advancedFiltersSupported);
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
      const params = new URLSearchParams({ source: sourceId, ...currentFilters() });
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
      applyFilters();
    });
  }
  filterStage.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      setAdvancedFilterSupport(advancedFiltersSupported);
      applyFilters();
    }
  });
  filterStage.addEventListener("input", () => setAdvancedFilterSupport(advancedFiltersSupported));
  document.getElementById("filters-clear").addEventListener("click", () => {
    for (const el of [filterGaggle, filterWorkflow, filterPhase, filterTrigger, filterStage, filterOutcome, filterPopulation, filterSince, filterUntil]) {
      el.value = "";
    }
    filterNoWork.checked = false;
    setAdvancedFilterSupport(advancedFiltersSupported);
    lastCursor = "";
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
      filterGaggle.value = gaggle;
    }
    if (name && [...filterWorkflow.options].some((o) => o.value === name)) {
      filterWorkflow.value = name;
    }
    applyFilters();
    document.getElementById("runs-table")?.scrollIntoView({ behavior: "smooth", block: "start" });
  }
  document.querySelectorAll("#runs-table th[data-sort]").forEach((th) => {
    th.addEventListener("click", () => {
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

  function renderGraphSvg(graph, transitions, events, run, orientation = "horizontal") {
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
      if (event.type === "stage.finished") stageStates.set(event.stage, event.status || "completed");
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
      const state = stageStates.get(n.id);
      if (state === "running") cls += " running";
      else if (state === "success" || state === "completed" || state === "no-work") cls += " completed";
      else if (state === "failed" || state === "error" || state === "escalated") cls += " failed";
      if (n.id === finalNodeId) cls += " terminal " + (run.phase === "failed" || run.phase === "escalated" ? "failed" : "completed");
      svg += '<rect class="' + cls + '" x="' + p.x + '" y="' + p.y + '" width="' + layout.nodeW + '" height="' + layout.nodeH + '" rx="6" />';
      const label = n.id === finalNodeId ? "Final: " + finalLabel : n.id + (n.owner ? " (" + n.owner + ")" : "");
      svg += '<text class="node-label" x="' + (p.x + 7) + '" y="' + (p.y + 17) + '">' + escapeHtml(label.length > 24 ? label.slice(0, 23) + "\\u2026" : label) + "</text>";
      if (state && n.id !== finalNodeId) {
        svg += '<text class="node-status" x="' + (p.x + 7) + '" y="' + (p.y + 32) + '">' + escapeHtml(state) + "</text>";
      } else if (n.id === finalNodeId) {
        svg += '<text class="node-status" x="' + (p.x + 7) + '" y="' + (p.y + 32) + '">' + escapeHtml(run.terminal ? "terminal" : "current") + "</text>";
      }
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
      svg + "</g></svg></div>";
  }

  function initGraphInteractions(graph, transitions, events, run) {
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
      container.innerHTML = renderGraphSvg(graph, transitions, events, run, graphOrientation);
      initGraphInteractions(graph, transitions, events, run);
    });

    svg.addEventListener("wheel", (event) => {
      event.preventDefault();
      setZoom(zoom * Math.exp(-event.deltaY * 0.0015), event.clientX, event.clientY);
    }, { passive: false });

    svg.addEventListener("pointerdown", (event) => {
      if (event.button !== 0) return;
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
    svg.addEventListener("dblclick", (event) => setZoom(zoom * 1.5, event.clientX, event.clientY));
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

  function renderTransitions(transitions) {
    if (!transitions || transitions.length === 0) return '<p class="muted">No transitions recorded.</p>';
    const items = transitions.map((t) => {
      const verdictCls = t.verdict ? " badge-" + t.verdict : "";
      const arrow = t.terminal ? "\\u25a0" : "\\u2192";
      const verdictTxt = t.verdict ? ' <span class="' + verdictCls.trim() + '">[' + t.verdict + (t.repass ? ", repass" : "") + "]</span>" : "";
      const target = t.target ? " " + arrow + " <code>" + escapeHtml(t.target) + "</code>" : (t.status ? " (" + escapeHtml(t.status) + ")" : "");
      return '<li><span class="seq">#' + t.seq + '</span><code>' + escapeHtml(t.source) + "</code>" + target + verdictTxt + "</li>";
    });
    return '<ul class="transitions-list">' + items.join("") + "</ul>";
  }

  function safeExternalUrl(value) {
    try {
      const url = new URL(value);
      return url.protocol === "http:" || url.protocol === "https:" ? url.href : "";
    } catch {
      return "";
    }
  }

  const renderRunAssociations = ${renderRunAssociations.toString()
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

  function renderOperatorPanel(op, refs) {
    if (!op) return "";
    const parts = [];
    if (op.issue) {
      const issueRef = (refs || []).find((ref) =>
        String(ref.id) === String(op.issue.number) &&
        ["issue", "work-item", "workitem"].includes(String(ref.kind || "").toLowerCase()) &&
        safeExternalUrl(ref.url),
      );
      const issueLabel = "#" + op.issue.number + " " + escapeHtml(op.issue.title || "");
      parts.push(["Issue", issueRef
        ? '<a href="' + escapeHtml(safeExternalUrl(issueRef.url)) + '" target="_blank" rel="noopener noreferrer">' + issueLabel + "</a>"
        : issueLabel]);
    }
    if (op.pullRequest) {
      const pullUrl = safeExternalUrl(op.pullRequest.url);
      const pullTitle = String(op.pullRequestTitle || "").trim();
      const pullLabel = escapeHtml(op.pullRequest.provider + " " + op.pullRequest.kind + " #" +
        op.pullRequest.id + (pullTitle ? ": " + pullTitle : ""));
      parts.push(["Pull request", pullUrl
        ? '<a href="' + escapeHtml(pullUrl) + '" target="_blank" rel="noopener noreferrer">' + pullLabel + "</a>"
        : pullLabel]);
    }
    if (op.liveness) parts.push(["Liveness", op.liveness]);
    if (op.trajectory) parts.push(["Trajectory", op.trajectory]);
    if (op.latestError) parts.push(["Latest error", "<code>" + escapeHtml(op.latestError.code) + "</code> " + escapeHtml(op.latestError.message || "")]);
    let html = '<div class="kv-grid">' + parts.map(([label, value]) =>
      '<div class="kv' + (label === "Latest error" ? " kv-wide" : "") + '"><div class="label">' +
      label + '</div><div class="value">' + value + "</div></div>").join("") + "</div>";
    if (op.review) {
      html += '<h3>Review</h3><p><span class="badge-' + op.review.verdict + '">' + op.review.verdict + "</span></p>";
      if (op.review.rationale) html += '<p class="rationale">' + escapeHtml(op.review.rationale) + "</p>";
    }
    if (op.pullRequest && op.pullRequestBody) {
      html += '<h3>Pull request description</h3><div class="pr-description">' +
        escapeHtml(op.pullRequestBody) + "</div>";
    }
    if (op.potentialBlockers && op.potentialBlockers.length) {
      html += '<h3>Potential blockers</h3><ul class="blockers-list">' + op.potentialBlockers.map((b) => "<li>" + escapeHtml(b) + "</li>").join("") + "</ul>";
    }
    return html;
  }

  function renderRunEvents(events, sourceId, runId) {
    if (!events || !events.length) return '<p class="muted">No events recorded.</p>';
    return '<div class="event-list">' + events.map((event) => {
      const status = event.status || event.verdict || event.decision || "";
      const stage = event.stage ? " · " + escapeHtml(event.stage) : "";
      const summary =
        '<summary><span class="event-seq">#' + event.seq + "</span><code>" +
        escapeHtml(event.type) + "</code><span>" + stage +
        (status ? " · " + escapeHtml(status) : "") +
        '</span><span class="event-time">' + fmtTime(event.time) + "</span></summary>";
      const artifacts = [];
      if (event.artifact) artifacts.push(event.artifact);
      for (const artifact of event.artifacts || []) artifacts.push(artifact);
      const seenDigests = new Set();
      const artifactLinks = artifacts.filter((artifact) => {
        if (!artifact.digest || seenDigests.has(artifact.digest)) return false;
        seenDigests.add(artifact.digest);
        return true;
      }).map((artifact) => {
        const href = "/api/run-artifact?source=" + encodeURIComponent(sourceId) +
          "&id=" + encodeURIComponent(runId) + "&digest=" + encodeURIComponent(artifact.digest);
        const label = artifact.name || artifact.digest;
        return '<a href="' + href + '" target="_blank" rel="noopener">' +
          escapeHtml(label) + " (" + artifact.size + " bytes)</a>";
      });
      const isTranscript = event.name && String(event.name).toLowerCase().includes("transcript");
      if (isTranscript) {
        const href = "/api/run-transcript?source=" + encodeURIComponent(sourceId) +
          "&id=" + encodeURIComponent(runId) + "&seq=" + encodeURIComponent(event.seq);
        artifactLinks.push('<a href="' + href + '" target="_blank" rel="noopener">Agent transcript / messages</a>');
      }
      const refHtml = event.externalRef && safeExternalUrl(event.externalRef.url)
        ? '<p>External: <a href="' + escapeHtml(safeExternalUrl(event.externalRef.url)) +
          '" target="_blank" rel="noopener noreferrer">' +
          escapeHtml((event.externalRef.provider || "") + " " + (event.externalRef.kind || "") + " #" + (event.externalRef.id || "")) +
          "</a></p>"
        : "";
      const details = {};
      for (const key of ["outputs", "runner", "error", "rationale", "reason", "completeness", "raw"]) {
        if (event[key] !== undefined && event[key] !== null && event[key] !== "") details[key] = event[key];
      }
      const detailsHtml = Object.keys(details).length
        ? "<pre>" + escapeHtml(JSON.stringify(details, null, 2)) + "</pre>"
        : "";
      const linksHtml = artifactLinks.length
        ? '<div class="artifact-links">' + artifactLinks.join(" · ") + "</div>"
        : "";
      const body = '<div class="event-body">' + refHtml + detailsHtml + linksHtml + "</div>";
      return "<details>" + summary + body + "</details>";
    }).join("") + "</div>";
  }

  async function openRun(runId) {
    const sourceId = sourceSelect.value;
    syncViewUrl(runId);
    dashboardEl.style.display = "none";
    runViewEl.style.display = "block";
    runErrorEl.textContent = "";
    runContentEl.innerHTML = '<p class="muted">Loading run\\u2026</p>';
    try {
      const res = await fetch("/api/run?source=" + encodeURIComponent(sourceId) + "&id=" + encodeURIComponent(runId));
      const data = await res.json();
      if (!data.connected) {
        runErrorEl.textContent = data.reason || "Run unavailable.";
        runContentEl.innerHTML = "";
        return;
      }
      const r = data.run;
      const events = r.events || [];
      const refs = externalRefsFrom(events);
      const finalTransition = [...(r.transitions || [])].reverse().find((t) => t.terminal);
      const finishedStages = new Set(
        events.filter((event) => event.type === "stage.finished" && event.stage).map((event) => event.stage),
      );
      const kv = [
        ["Workflow", escapeHtml(r.workflow) + (r.workflowVersion ? " v" + r.workflowVersion : "")],
        ["Gaggle", escapeHtml(r.gaggle || "")],
        ["Phase", '<span class="phase">' + escapeHtml(r.phase) + "</span>"],
        ["Final state", escapeHtml((finalTransition && (finalTransition.status || finalTransition.verdict)) || (r.terminal ? r.phase : "in progress"))],
        ["Completed stages", String(finishedStages.size)],
        ["Started", fmtTime(r.startedAt)],
        ["Finished", fmtTime(r.finishedAt)],
        ["Duration", r.durationMillis ? Math.round(r.durationMillis / 1000) + "s" : "\\u2014"],
        ["Repasses", r.repassCount ?? 0],
        ["Retries", r.retryCount ?? 0],
        ["Trigger", escapeHtml((r.trigger && r.trigger.kind) || "")],
      ];
      const actionsRunUrl = safeExternalUrl(r.actionsRunUrl);
      const actionsLink = actionsRunUrl
        ? '<a class="actions-run-link" href="' + escapeHtml(actionsRunUrl) +
          '" target="_blank" rel="noopener noreferrer">View GitHub Action &#8599;</a>'
        : "";
      let html = '<div class="run-header"><h2>' + escapeHtml(r.workflow) + "</h2><code>" +
        escapeHtml(r.id) + "</code>" + actionsLink + "</div>";
      html += '<div class="kv-grid">' + kv.map(([label, value]) => '<div class="kv"><div class="label">' + label + '</div><div class="value">' + value + "</div></div>").join("") + "</div>";
      if (r.operator) {
        html += "<h2>Operator</h2>" + renderOperatorPanel(r.operator, refs);
      }
      if (refs.length) {
        html += "<h2>Associated work</h2>" + renderExternalRefs(refs);
      }
      html += '<h2>Workflow graph</h2><div id="graph-container">' +
        renderGraphSvg(r.graph, r.transitions, events, r, graphOrientation) + "</div>";
      html += "<h2>Transitions</h2>" + renderTransitions(r.transitions);
      html += "<h2>Events, logs, and messages</h2>" + renderRunEvents(events, sourceId, runId);
      runContentEl.innerHTML = html;
      initGraphInteractions(r.graph, r.transitions, events, r);
    } catch (err) {
      runErrorEl.textContent = String(err);
      runContentEl.innerHTML = "";
    }
  }

  document.getElementById("run-back").addEventListener("click", () => {
    runViewEl.style.display = "none";
    dashboardEl.style.display = "block";
    syncViewUrl();
  });

  async function loadSnapshot() {
    const sourceId = sourceSelect.value;
    if (!sourceId) {
      emptyEl.style.display = "block";
      dashboardEl.style.display = "none";
      startBarEl.style.display = "none";
      errorEl.textContent = "";
      return;
    }
    try {
      const data = await fetchSnapshot();
      if (data) renderSnapshot(data);
    } catch (err) {
      errorEl.textContent = portalRequestError(err);
    }
  }

  /** Fetch the snapshot for the selected source without rendering it. */
  async function fetchSnapshot() {
    const sourceId = sourceSelect.value;
    if (!sourceId) return null;
    const res = await fetch("/api/snapshot?source=" + encodeURIComponent(sourceId));
    return await res.json();
  }

  function setFreshnessState(state, freshAt = null) {
    const timestamp = freshAt || lastUpdatedAt;
    const timeStr = timestamp ? fmtTime(timestamp) : "never";
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
  sourceSelect.addEventListener("change", () => {
    liveConnectionEstablished = false;
    reconnectAttemptCount = 0;
    if (eventSource) eventSource.close();
    void loadSnapshot().then(connectLiveEvents);
  });
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
    await loadSnapshot();
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
