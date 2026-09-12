import { useId, useState } from "react";
import type { QueryState } from "../api/queryState";
import { QueryStateBoundary } from "../api/queryState";
import type { ValidationWarning } from "../api/types";
import { SectionQueryStatus } from "./SectionQueryStatus";
import {
  configurationWarningKey,
  sortConfigurationWarnings,
} from "../configurationWarnings";

export interface ConfigurationWarningsProps {
  context: "instance" | "workflow";
  state: QueryState<readonly ValidationWarning[]>;
  dismissedWarningKeys: ReadonlySet<string>;
  onDismiss: (warning: ValidationWarning) => void;
  onRefresh: () => void;
}

function WarningReadError({
  error,
  onRefresh,
  stale = false,
}: {
  error: Error;
  onRefresh: () => void;
  stale?: boolean;
}) {
  return (
    <SectionQueryStatus
      error
      message={`${stale ? "Configuration warnings may be stale" : "Configuration warnings unavailable"}: ${error.message}`}
      retry={onRefresh}
    />
  );
}

function WarningList({
  dismissedWarningKeys,
  onDismiss,
  warnings,
}: {
  dismissedWarningKeys: ReadonlySet<string>;
  onDismiss: (warning: ValidationWarning) => void;
  warnings: readonly ValidationWarning[];
}) {
  const visibleWarnings = sortConfigurationWarnings(warnings).filter(
    (warning) => !dismissedWarningKeys.has(configurationWarningKey(warning)),
  );

  if (visibleWarnings.length === 0) {
    return (
      <div className="configuration-warning-empty">
        <strong>Warnings dismissed for this portal session.</strong>
        <span>Refresh to show warnings that are still active.</span>
      </div>
    );
  }

  const groups: Array<{
    key: string;
    remediation: string;
    scope: string;
    warnings: ValidationWarning[];
  }> = [];
  for (const warning of visibleWarnings) {
    const remediation = warningRemediation(warning);
    const key = `${warning.scope}\u0000${remediation}`;
    const current = groups.at(-1);
    if (current?.key === key) {
      current.warnings.push(warning);
    } else {
      groups.push({ key, remediation, scope: warning.scope, warnings: [warning] });
    }
  }

  return (
    <div className="configuration-warning-groups">
      {groups.map((group) => (
        <WarningGroup group={group} key={group.key} onDismiss={onDismiss} />
      ))}
    </div>
  );
}

function WarningGroup({
  group,
  onDismiss,
}: {
  group: {
    remediation: string;
    scope: string;
    warnings: ValidationWarning[];
  };
  onDismiss: (warning: ValidationWarning) => void;
}) {
  const [expanded, setExpanded] = useState(true);
  const contentId = useId();
  const remediationId = useId();

  return (
    <section
      aria-label={`${group.scope} configuration warnings`}
      className="configuration-warning-group"
    >
      <header>
        <button
          aria-controls={contentId}
          aria-expanded={expanded}
          className="configuration-warning-group-toggle"
          onClick={() => setExpanded((current) => !current)}
          type="button"
        >
          <span>
            <strong>Scope</strong>
            <code>{group.scope}</code>
          </span>
          <span className="configuration-warning-group-count">
            {group.warnings.length} {group.warnings.length === 1 ? "warning" : "warnings"}
          </span>
        </button>
        <button
          aria-label={`Dismiss all ${group.warnings.length} ${
            group.warnings.length === 1 ? "warning" : "warnings"
          } for ${group.scope}`}
          className="configuration-warning-dismiss"
          onClick={() => group.warnings.forEach(onDismiss)}
          type="button"
        >
          Dismiss group
        </button>
      </header>
      {expanded && (
        <div className="configuration-warning-group-content" id={contentId}>
          <p className="configuration-warning-remediation" id={remediationId}>
            <strong>Shared remediation</strong>
            Update the referenced definition under <code>config/</code>, then run{" "}
            <code>goobers validate</code> before reloading. The portal is read-only.
          </p>
          <div className="configuration-warning-list">
            {group.warnings.map((warning) => (
              <article
                aria-describedby={remediationId}
                className="configuration-warning"
                data-testid="configuration-warning"
                key={configurationWarningKey(warning)}
              >
                <div className="configuration-warning-identity">
                  <code className="warning-code">{warning.code}</code>
                  <span className="warning-severity">{warning.severity}</span>
                </div>
                <p>{warning.explanation}</p>
                <button
                  aria-label={`Dismiss ${warning.code} warning for ${warning.scope}`}
                  className="configuration-warning-dismiss"
                  onClick={() => onDismiss(warning)}
                  type="button"
                >
                  Dismiss
                </button>
              </article>
            ))}
          </div>
        </div>
      )}
    </section>
  );
}

function warningRemediation(_warning: ValidationWarning): string {
  return "config-validate";
}

export function ConfigurationWarnings({
  context,
  dismissedWarningKeys,
  onDismiss,
  onRefresh,
  state,
}: ConfigurationWarningsProps) {
  const titleId = `${context}-configuration-warnings`;
  const refreshing = state.status === "stale" && !state.error;
  const activeWarningCount =
    state.status === "ready" || state.status === "stale"
      ? state.data.filter(
          (warning) => !dismissedWarningKeys.has(configurationWarningKey(warning)),
        ).length
      : undefined;

  return (
    <section
      aria-labelledby={titleId}
      className={`content-section configuration-warning-section configuration-warning-section-${context}`}
    >
      <div className="section-heading configuration-warning-heading">
        <h2 id={titleId}>Configuration warnings</h2>
        {activeWarningCount !== undefined && (
          <div className="configuration-warning-actions">
            <span className="section-count">
              {activeWarningCount} active {activeWarningCount === 1 ? "warning" : "warnings"}
            </span>
            <span aria-hidden="true">|</span>
            <button
              aria-label={refreshing ? "Refreshing warnings" : "Refresh warnings"}
              className="text-button"
              disabled={refreshing}
              onClick={onRefresh}
              type="button"
            >
              {refreshing ? "Refreshing…" : "Refresh"}
            </button>
          </div>
        )}
      </div>
      <QueryStateBoundary
        empty={
          <div className="configuration-warning-empty">
            <strong>
              {context === "workflow"
                ? "No active configuration warnings for this workflow."
                : "No active configuration warnings."}
            </strong>
            <span>The latest configuration read completed without warning findings.</span>
          </div>
        }
        error={(error) => <WarningReadError error={error} onRefresh={onRefresh} />}
        loading={
          <SectionQueryStatus loading message="Loading configuration warnings…" />
        }
        stale={(warnings, error) => (
          <>
            {error && <WarningReadError error={error} onRefresh={onRefresh} stale />}
            <WarningList
              dismissedWarningKeys={dismissedWarningKeys}
              onDismiss={onDismiss}
              warnings={warnings}
            />
          </>
        )}
        state={state}
      >
        {(warnings) => (
          <WarningList
            dismissedWarningKeys={dismissedWarningKeys}
            onDismiss={onDismiss}
            warnings={warnings}
          />
        )}
      </QueryStateBoundary>
    </section>
  );
}
