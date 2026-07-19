import type { QueryState } from "../api/queryState";
import { QueryStateBoundary } from "../api/queryState";
import type { ValidationWarning } from "../api/types";
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
    <div className="configuration-warning-error" role="alert">
      <span>
        <strong>
          {stale ? "Configuration warnings may be stale" : "Configuration warnings unavailable"}
        </strong>
        <small>{error.message}</small>
      </span>
      <button className="text-button" onClick={onRefresh} type="button">
        Try again
      </button>
    </div>
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

  const groups: Array<{ scope: string; warnings: ValidationWarning[] }> = [];
  for (const warning of visibleWarnings) {
    const current = groups.at(-1);
    if (current?.scope === warning.scope) {
      current.warnings.push(warning);
    } else {
      groups.push({ scope: warning.scope, warnings: [warning] });
    }
  }

  return (
    <div className="configuration-warning-groups">
      {groups.map((group) => (
        <section
          aria-label={`${group.scope} configuration warnings`}
          className="configuration-warning-group"
          key={group.scope}
        >
          <header>
            <strong>Scope</strong>
            <code>{group.scope}</code>
          </header>
          <div className="configuration-warning-list">
            {group.warnings.map((warning) => (
              <article
                className="configuration-warning"
                data-testid="configuration-warning"
                key={configurationWarningKey(warning)}
              >
                <div className="configuration-warning-identity">
                  <code className="warning-code">{warning.code}</code>
                  <span className="warning-severity">{warning.severity}</span>
                </div>
                <p>{warning.explanation}</p>
                <p className="configuration-warning-remediation">
                  <strong>Remediation</strong>
                  Update the referenced definition under <code>config/</code>, then run{" "}
                  <code>goobers validate</code> before reloading. The portal is read-only.
                </p>
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
        </section>
      ))}
    </div>
  );
}

export function ConfigurationWarnings({
  context,
  dismissedWarningKeys,
  onDismiss,
  onRefresh,
  state,
}: ConfigurationWarningsProps) {
  const titleId = `${context}-configuration-warnings`;
  const activeWarningCount =
    state.status === "ready" || state.status === "stale" ? state.data.length : undefined;

  return (
    <section
      aria-labelledby={titleId}
      className={`content-section configuration-warning-section configuration-warning-section-${context}`}
    >
      <div className="section-heading configuration-warning-heading">
        <div>
          <p className="section-kicker">Configuration</p>
          <h2 id={titleId}>Configuration warnings</h2>
          <p>
            Advisory config-as-code findings for {context === "workflow" ? "this workflow" : "this instance"}.
            Run failures and daemon errors remain separate.
          </p>
        </div>
        {activeWarningCount !== undefined && (
          <div className="configuration-warning-actions">
            <span className="section-count">
              {activeWarningCount} active {activeWarningCount === 1 ? "warning" : "warnings"}
            </span>
            <button className="text-button" onClick={onRefresh} type="button">
              Refresh warnings
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
          <div
            aria-live="polite"
            className="configuration-warning-loading"
            role="status"
          >
            Loading configuration warnings
          </div>
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
