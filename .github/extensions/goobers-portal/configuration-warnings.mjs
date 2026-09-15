function escapeWarningHtml(value) {
    return String(value).replace(/[&<>"']/g, (character) => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;",
    })[character]);
}

export function configurationWarningKey(warning = {}) {
    return JSON.stringify([
        warning.scope || "",
        warning.code || "",
        warning.severity || "",
        warning.explanation || "",
    ]);
}

export function warningRemediation(_warning = {}) {
    return {
        key: "config-validate",
        html: 'Update the referenced definition under <code>config/</code>, then run ' +
            '<code>goobers validate</code> before reloading. The portal is read-only.',
    };
}

export function sortConfigurationWarnings(warnings = []) {
    const compare = (left, right) => left < right ? -1 : left > right ? 1 : 0;
    return [...warnings].sort((left = {}, right = {}) =>
        compare(left.scope || "", right.scope || "") ||
        compare(left.code || "", right.code || "") ||
        compare(left.explanation || "", right.explanation || ""));
}

export function groupConfigurationWarnings(warnings = []) {
    const groups = [];
    for (const warning of sortConfigurationWarnings(warnings)) {
        const remediation = warningRemediation(warning);
        const key = JSON.stringify([warning.scope || "", remediation.key]);
        const current = groups.at(-1);
        if (current?.key === key) {
            current.warnings.push(warning);
        } else {
            groups.push({
                key,
                scope: warning.scope || "",
                remediation,
                warnings: [warning],
            });
        }
    }
    return groups;
}

export function renderConfigurationWarnings(warnings = [], context = "instance", options = {}) {
    const dismissedWarningKeys = options.dismissedWarningKeys || new Set();
    const visibleWarnings = sortConfigurationWarnings(warnings)
        .filter((warning) => !dismissedWarningKeys.has(configurationWarningKey(warning)));
    const contextName = context === "workflow" ? "workflow" : "instance";
    const activeLabel = `${visibleWarnings.length} active ${visibleWarnings.length === 1 ? "warning" : "warnings"}`;

    let body;
    if (warnings.length === 0) {
        const empty = contextName === "workflow"
            ? "No active configuration warnings for this workflow."
            : "No active configuration warnings.";
        body = '<div class="configuration-warning-empty"><strong>' +
            escapeWarningHtml(empty) +
            "</strong><span>The latest configuration read completed without warning findings.</span></div>";
    } else if (visibleWarnings.length === 0) {
        body = '<div class="configuration-warning-empty">' +
            "<strong>Warnings dismissed for this portal session.</strong>" +
            "<span>Warnings will reappear if their content changes.</span></div>";
    } else {
        body = '<div class="configuration-warning-groups">' +
            groupConfigurationWarnings(visibleWarnings).map((group) => {
                const count = group.warnings.length;
                const warningLabel = count === 1 ? "warning" : "warnings";
                const warningItems = group.warnings.map((warning) => {
                    const warningKey = configurationWarningKey(warning);
                    return '<article class="configuration-warning" data-warning-key="' +
                        escapeWarningHtml(warningKey) + '">' +
                        '<div class="configuration-warning-identity">' +
                        '<code class="warning-code">' + escapeWarningHtml(warning.code || "") + "</code>" +
                        '<span class="warning-severity">' + escapeWarningHtml(warning.severity || "") + "</span>" +
                        "</div><p>" + escapeWarningHtml(warning.explanation || "") + "</p>" +
                        '<button type="button" class="configuration-warning-dismiss" data-dismiss-warning="' +
                        escapeWarningHtml(warningKey) + '" aria-label="Dismiss ' +
                        escapeWarningHtml(warning.code || "") + " warning for " +
                        escapeWarningHtml(group.scope) + '">Dismiss</button></article>';
                }).join("");
                return '<details class="configuration-warning-group" open>' +
                    '<summary><span><strong>Scope</strong> <code>' +
                    escapeWarningHtml(group.scope) + "</code></span>" +
                    '<span class="configuration-warning-group-count">' +
                    count + " " + warningLabel + "</span></summary>" +
                    '<div class="configuration-warning-group-content">' +
                    '<div class="configuration-warning-group-actions"><button type="button" ' +
                    'class="configuration-warning-dismiss" data-dismiss-warning-group ' +
                    'aria-label="Dismiss all ' + count + " " + warningLabel + " for " +
                    escapeWarningHtml(group.scope) + '">Dismiss group</button></div>' +
                    '<p class="configuration-warning-remediation"><strong>Shared remediation</strong> ' +
                    group.remediation.html + "</p>" +
                    '<div class="configuration-warning-list">' + warningItems + "</div></div></details>";
            }).join("") + "</div>";
    }

    return '<section class="configuration-warning-section configuration-warning-section-' +
        contextName + '" aria-label="Configuration warnings">' +
        '<div class="configuration-warning-heading"><h2>Configuration warnings</h2>' +
        '<span class="section-count">' + activeLabel + "</span></div>" +
        body + "</section>";
}
