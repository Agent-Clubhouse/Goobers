import { PageHeading, SectionHeading } from "../ui/Heading";
import { formatTimestamp, formatPreciseTimestamp } from "../dateTime";
import { Action } from "../ui/Action";
import { Timestamp } from "../ui/Timestamp";
import { useEffect, useMemo, useRef, useState } from "react";
import { RunTiming } from "../components/RunTiming";
import type {
  DaemonClient,
  MaintenanceStatus,
  RecoveryInventoryStatus,
  RecoveryReclaimHold,
  StartupStatus,
  RunSummary,
} from "../api/types";
import { useAttentionCollapsed } from "../attentionCollapse";
import { useAttentionDismissals } from "../attentionDismissals";
import type { ConfigurationWarningsProps } from "../components/ConfigurationWarnings";
import { ConfigurationWarnings } from "../components/ConfigurationWarnings";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { RecoveryCommand } from "../components/RecoveryAction";
import { ScopePivot } from "../components/ScopePivot";
import {
  attentionSeverity,
  attentionSeverityRank,
  incompleteRunPhasesMessage,
  type AttentionSeverity,
  type OperationalOverview,
  useOperationalOverview,
} from "../operationalData";
import { routeHash } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
import { Icon } from "../ui/Icon";
import { StatusBadge } from "../ui/StatusBadge";
import type { FailureReasons } from "../overviewFailures";
import { configurationWarningKey } from "../configurationWarnings";

const attentionSelectionStorageKey = "goobers-overview-attention-selection";
const emptyFailureReasons: FailureReasons = new Map();

function readAttentionSelection(): ReadonlySet<string> {
  try {
    const parsed: unknown = JSON.parse(
      window.sessionStorage.getItem(attentionSelectionStorageKey) ?? "[]",
    );
    return Array.isArray(parsed)
      ? new Set(parsed.filter((value): value is string => typeof value === "string"))
      : new Set();
  } catch {
    return new Set();
  }
}

export function OverviewPage({
  client,
  configurationWarnings,
  standalone,
}: {
  client: DaemonClient;
  configurationWarnings: Omit<ConfigurationWarningsProps, "context">;
  standalone: boolean;
}) {
  const query = useOperationalOverview(client);

  if (query.state.status === "loading") {
    return <DaemonLoadingState standalone={standalone} />;
  }
  if (query.state.status === "error") {
    return (
      <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />
    );
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  return (
    <Overview
      configurationWarnings={configurationWarnings}
      failureReasons={query.state.data.groups.failureReasons ?? emptyFailureReasons}
      overview={query.state.data}
      retry={query.retry}
      standalone={standalone}
    />
  );
}

function Overview({
  configurationWarnings,
  failureReasons,
  overview,
  retry,
  standalone,
}: {
  configurationWarnings: Omit<ConfigurationWarningsProps, "context">;
  failureReasons: FailureReasons;
  overview: OperationalOverview;
  retry: () => void;
  standalone: boolean;
}) {
  const groups = overview.groups;
  const overviewRuns = [...groups.active, ...groups.attention, ...groups.recent];
  const inventoryLoaded =
    !overview.loadingSections?.inventory && !overview.sectionErrors?.inventory;
  const emptyInstance = inventoryLoaded && overview.gaggleCount === 0;
  const emptyWorkflows =
    inventoryLoaded && !emptyInstance && overview.instance.counts.workflows === 0;
  const emptyRuns =
    !emptyInstance &&
    !emptyWorkflows &&
    !overview.sectionErrors?.runs &&
    !groups.incomplete &&
    groups.active.length === 0 &&
    groups.attention.length === 0 &&
    groups.recent.length === 0;
  const healthy = standalone || overview.health.healthy;
  const starting = !overview.health.ready && overview.instance.status === "starting";
  const activeConfigurationWarningCount =
    configurationWarnings.state.status === "ready" || configurationWarnings.state.status === "stale"
      ? configurationWarnings.state.data.filter(
          (warning) =>
            !configurationWarnings.dismissedWarningKeys.has(configurationWarningKey(warning)),
        ).length
      : 0;

  const { dismissedRunIds, dismiss, restore } = useAttentionDismissals();
  const [attentionCollapsed, setAttentionCollapsed] = useAttentionCollapsed();
  const [selectedRunIds, setSelectedRunIds] = useState<ReadonlySet<string>>(readAttentionSelection);
  const [expandedAttentionGroups, setExpandedAttentionGroups] = useState<ReadonlySet<string>>(
    () => new Set(),
  );
  const [showDismissed, setShowDismissed] = useState(false);
  const activeAttention = groups.attention.filter((run) => !dismissedRunIds.has(run.id));
  const dismissedAttention = groups.attention.filter((run) => dismissedRunIds.has(run.id));
  const attentionGroups = useMemo(
    () => groupAttentionRuns(activeAttention, overview, failureReasons),
    [activeAttention, failureReasons, overview],
  );
  const attentionSections = [
    {
      key: "action-required",
      label: "Action required",
      groups: attentionGroups.filter((group) => group.severity !== "warning"),
    },
    {
      key: "fyi",
      label: "FYI failures",
      groups: attentionGroups.filter((group) => group.severity === "warning"),
    },
  ]
    .filter((section) => section.groups.length > 0)
    .map((section) => ({
      ...section,
      runCount: section.groups.reduce((count, group) => count + group.runs.length, 0),
    }));
  const activeAttentionIds = new Set(activeAttention.map((run) => run.id));
  const visibleSelectedRunIds = [...selectedRunIds].filter((runId) =>
    activeAttentionIds.has(runId),
  );
  const allVisibleSelected =
    activeAttention.length > 0 && visibleSelectedRunIds.length === activeAttention.length;

  useEffect(() => {
    try {
      window.sessionStorage.setItem(
        attentionSelectionStorageKey,
        JSON.stringify([...selectedRunIds]),
      );
    } catch {
      // Selection remains available until this page unmounts when storage is unavailable.
    }
  }, [selectedRunIds]);

  const toggleSelected = (runId: string) => {
    setSelectedRunIds((current) => {
      const next = new Set(current);
      if (next.has(runId)) {
        next.delete(runId);
      } else {
        next.add(runId);
      }
      return next;
    });
  };
  const dismissRuns = (runIds: readonly string[]) => {
    dismiss(runIds);
    setSelectedRunIds((current) => {
      const next = new Set(current);
      for (const runId of runIds) {
        next.delete(runId);
      }
      return next;
    });
  };
  const toggleAllVisible = () => {
    setSelectedRunIds((current) => {
      const next = new Set(current);
      if (allVisibleSelected) {
        for (const run of activeAttention) {
          next.delete(run.id);
        }
      } else {
        for (const run of activeAttention) {
          next.add(run.id);
        }
      }
      return next;
    });
  };
  const toggleGroupExpanded = (key: string) => {
    setExpandedAttentionGroups((current) => {
      const next = new Set(current);
      if (next.has(key)) {
        next.delete(key);
      } else {
        next.add(key);
      }
      return next;
    });
  };

  return (
    <div className="overview-page">
      <PageHeading
        title={
          <>
            Overview -{" "}
            {overview.loadingSections?.inventory || overview.loadingSections?.runs ? (
              <span aria-label="Loading overview" className="overview-loading-title" role="status">
                <span aria-hidden="true">.</span>
                <span aria-hidden="true">.</span>
                <span aria-hidden="true">.</span>
              </span>
            ) : starting ? (
              "Starting"
            ) : !healthy ? (
              "Unhealthy"
            ) : !overview.health.ready ? (
              "Starting"
            ) : (
              attentionHeading(activeAttention.length)
            )}
          </>
        }
        className=""
        contentClassName="overview-heading-copy"
        beforeTitle={
          <>
            {overview.instance.rootIdentity?.decommissionedAt && (
              <p role="alert">
                Historical root; do not use. Decommissioned{" "}
                {overview.instance.rootIdentity.decommissionedAt}:{" "}
                {overview.instance.rootIdentity.decommissionReason}
              </p>
            )}
            {overview.instance.rootIdentity?.identityProblem && (
              <p role="status">{overview.instance.rootIdentity.identityProblem}</p>
            )}
            {overview.instance.rootIdentity?.lifecycleProblem && (
              <p role="alert">{overview.instance.rootIdentity.lifecycleProblem}</p>
            )}
          </>
        }
      >
        {emptyInstance && (
          <p>
            No gaggles are configured. Add gaggle definitions to begin observing workflows and runs.
          </p>
        )}
      </PageHeading>

      <div className="overview-notices">
        {/* A failed health or instance refresh falls back to the previous data so
          the page keeps rendering something, but that fallback must say it's
          stale — otherwise degraded daemon health or instance state reads as
          current with no diagnostic at all (#3659). */}
        {(overview.sectionErrors?.health || overview.sectionErrors?.instance) && (
          <div className="inline-empty section-error" role="alert">
            <span>
              {overview.sectionErrors?.health && overview.sectionErrors?.instance
                ? "Daemon health and instance data could not be refreshed just now, so the status above may be stale."
                : overview.sectionErrors?.health
                  ? "Daemon health could not be refreshed just now, so the status above may be stale."
                  : "Instance data could not be refreshed just now, so the status above may be stale."}
            </span>
            <Action variant="text" className="text-button" onClick={retry} type="button">
              Retry
            </Action>
          </div>
        )}

        {/* A section that failed to load must say so. Without this the page would
          render an empty run list identically to a genuinely idle instance,
          which is a worse failure than the blank page it replaced (#1709). */}
        {overview.sectionErrors?.inventory && (
          <div className="inline-empty section-error" role="alert">
            <span>
              The gaggle and workflow inventory could not be read just now. Inventory-backed names
              and empty-state guidance are unavailable until it loads; daemon health, counts, and
              run data below remain current.
            </span>
            <Action variant="text" className="text-button" onClick={retry} type="button">
              Retry inventory
            </Action>
          </div>
        )}
        {overview.sectionErrors?.runs && (
          <div className="inline-empty section-error" role="alert">
            <span>
              Run activity could not be read just now, so the run groups below may be incomplete or
              out of date. Everything else on this page is current.
            </span>
            <Action variant="text" className="text-button" onClick={retry} type="button">
              Retry run activity
            </Action>
          </div>
        )}
        {(overview.loadingSections?.inventory || overview.loadingSections?.runs) && (
          <div className="inline-empty section-loading" role="status">
            <span aria-hidden="true" className="loading-mark" />
            <span>
              {overview.loadingSections.inventory && overview.loadingSections.runs
                ? "Loading inventory and run activity"
                : overview.loadingSections.inventory
                  ? "Loading inventory"
                  : "Loading run activity"}
            </span>
          </div>
        )}

        {/* A phase that failed while its siblings succeeded is the same trap one
          level down: the surviving groups are real, but the failed phase's
          runs are missing and would otherwise read as "nothing happened"
          (#3658). */}
        {!overview.sectionErrors?.runs && groups.incomplete && (
          <p className="inline-empty" role="alert">
            {incompleteRunPhasesMessage(groups.incomplete)}
          </p>
        )}
        {groups.attentionCandidatesTruncated && (
          <p className="inline-empty" role="alert">
            The attention candidate window was truncated; additional actionable runs may exist.
            Inspect the Runs page for the full list.
          </p>
        )}
      </div>

      <InstanceSummaryPanel
        configurationWarningCount={activeConfigurationWarningCount}
        overview={overview}
        retry={retry}
        standalone={standalone}
      />

      {groups.attention.length > 0 && (
        <section className="content-section attention-section">
          <SectionHeading
            title="Needs attention"
            className=""
            actions={
              <>
                <div className="attention-actions">
                  {activeAttention.length > 0 && (
                    <label className="attention-select-all">
                      <SelectionCheckbox
                        ariaLabel="Select all visible attention runs"
                        checked={allVisibleSelected}
                        indeterminate={
                          visibleSelectedRunIds.length > 0 &&
                          visibleSelectedRunIds.length < activeAttention.length
                        }
                        onChange={toggleAllVisible}
                      />
                      Select all
                    </label>
                  )}
                  {visibleSelectedRunIds.length > 0 && (
                    <Action
                      variant="text"
                      className="text-button"
                      onClick={() => dismissRuns(visibleSelectedRunIds)}
                      type="button"
                    >
                      Dismiss {visibleSelectedRunIds.length} selected
                    </Action>
                  )}
                  {dismissedAttention.length > 0 && (
                    <Action
                      variant="text"
                      className="text-button"
                      onClick={() => setShowDismissed((current) => !current)}
                      type="button"
                    >
                      {showDismissed
                        ? "Hide dismissed"
                        : `Show dismissed (${dismissedAttention.length})`}
                    </Action>
                  )}
                  <span className="section-count">
                    {activeAttention.length} {activeAttention.length === 1 ? "run" : "runs"}
                  </span>
                  <button
                    aria-controls="attention-section-body"
                    aria-expanded={!attentionCollapsed}
                    className="attention-collapse-toggle"
                    onClick={() => setAttentionCollapsed(!attentionCollapsed)}
                    type="button"
                  >
                    <span className="sr-only">
                      {attentionCollapsed ? "Expand needs attention" : "Collapse needs attention"}
                    </span>
                    <span aria-hidden="true" className="attention-collapse-chevron">
                      <Icon name="chevron" size={14} />
                    </span>
                  </button>
                </div>
              </>
            }
          />
          <div hidden={attentionCollapsed} id="attention-section-body">
            {activeAttention.length === 0 ? (
              <p className="inline-empty">Nothing needs attention right now.</p>
            ) : (
              <div className="attention-list">
                {attentionSections.map((section) => (
                  <div
                    className={`attention-severity-section attention-severity-section-${section.key}`}
                    key={section.key}
                  >
                    <div className="attention-severity-heading">
                      <strong>{section.label}</strong>
                      <span>
                        {section.runCount} {section.runCount === 1 ? "run" : "runs"}
                      </span>
                    </div>
                    {section.groups.map((group) => {
                      const selectedCount = group.runs.filter((run) =>
                        selectedRunIds.has(run.id),
                      ).length;
                      const expanded = expandedAttentionGroups.has(group.key);
                      return (
                        <div
                          className={`attention-group attention-group-${group.severity}`}
                          key={group.key}
                        >
                          <div className="attention-row attention-group-summary">
                            <SelectionCheckbox
                              ariaLabel={`Select all ${group.runs.length} runs in ${group.label}`}
                              checked={selectedCount === group.runs.length}
                              indeterminate={selectedCount > 0 && selectedCount < group.runs.length}
                              onChange={() => {
                                const shouldSelect = selectedCount !== group.runs.length;
                                setSelectedRunIds((current) => {
                                  const next = new Set(current);
                                  for (const run of group.runs) {
                                    if (shouldSelect) {
                                      next.add(run.id);
                                    } else {
                                      next.delete(run.id);
                                    }
                                  }
                                  return next;
                                });
                              }}
                            />
                            <span className="attention-icon">
                              <Icon name="alert" />
                            </span>
                            <span className="attention-copy">
                              <strong title={group.label}>{group.label}</strong>
                              <span
                                className={`attention-severity-badge attention-severity-${group.severity}`}
                              >
                                {attentionSeverityLabel(group.severity)}
                              </span>
                              <span title={group.diagnosis}>
                                {group.runs.length} {group.runs.length === 1 ? "run" : "runs"} ·{" "}
                                {group.diagnosis}
                              </span>
                            </span>
                            <span className="attention-meta">
                              <span title={group.context}>{group.context}</span>
                              <Timestamp value={group.latest.lastActivityAt} prefix="Latest " />
                            </span>
                            <button
                              aria-controls={`attention-group-${group.domId}`}
                              aria-expanded={expanded}
                              className="attention-expand"
                              onClick={() => toggleGroupExpanded(group.key)}
                              type="button"
                            >
                              {expanded ? "Hide runs" : "Show runs"}
                            </button>
                            <button
                              aria-label={`Dismiss all runs in ${group.label}`}
                              className="attention-dismiss"
                              onClick={() => dismissRuns(group.runs.map((run) => run.id))}
                              type="button"
                            >
                              Dismiss group
                            </button>
                          </div>
                          <div
                            className="attention-group-runs"
                            hidden={!expanded}
                            id={`attention-group-${group.domId}`}
                          >
                            {group.runs.map((run) => {
                              const severity = attentionSeverity(
                                run,
                                overviewRuns,
                                failureReasons.get(run.id),
                              );
                              return (
                                <div className="attention-run-row" key={run.id}>
                                  <input
                                    aria-label={`Select run ${run.id} for bulk actions`}
                                    checked={selectedRunIds.has(run.id)}
                                    className="attention-select"
                                    onChange={() => toggleSelected(run.id)}
                                    type="checkbox"
                                  />
                                  <a
                                    className="attention-run-link"
                                    href={routeHash({ page: "run", id: run.id })}
                                    title={run.id}
                                  >
                                    <strong>{run.id}</strong>
                                    <span
                                      className={`attention-severity-badge attention-severity-${severity}`}
                                    >
                                      {attentionSeverityLabel(severity)}
                                    </span>
                                    <span>
                                      {attentionDiagnosis(run, failureReasons, severity)}
                                    </span>
                                  </a>
                                  <ScopePivot
                                    label={workflowIdentity(run)}
                                    scope={{ gaggle: run.gaggle, workflow: run.workflow }}
                                  />
                                  <Timestamp value={run.lastActivityAt} />
                                  <button
                                    aria-label={`Dismiss run ${run.id}`}
                                    className="attention-dismiss"
                                    onClick={() => dismissRuns([run.id])}
                                    type="button"
                                  >
                                    Dismiss
                                  </button>
                                </div>
                              );
                            })}
                          </div>
                        </div>
                      );
                    })}
                  </div>
                ))}
              </div>
            )}
            {showDismissed && dismissedAttention.length > 0 && (
              <div className="attention-dismissed-list">
                <div className="section-heading">
                  <p className="section-kicker">Dismissed</p>
                  <Action
                    variant="text"
                    className="text-button"
                    onClick={() => restore(dismissedAttention.map((run) => run.id))}
                    type="button"
                  >
                    Restore all
                  </Action>
                </div>
                {dismissedAttention.map((run) => (
                  <div className="attention-row attention-row-dismissed" key={run.id}>
                    <span className="attention-copy">
                      <strong>{runLabel(run)}</strong>
                      <span>{workflowIdentity(run)}</span>
                    </span>
                    <Action
                      variant="text"
                      aria-label={`Undo dismiss for run ${run.id}`}
                      className="text-button"
                      onClick={() => restore([run.id])}
                      type="button"
                    >
                      Undo
                    </Action>
                  </div>
                ))}
              </div>
            )}
          </div>
        </section>
      )}

      <div className="overview-activity">
        {!inventoryLoaded ? null : emptyInstance ? (
          <section className="empty-state">
            <img alt="" src="/goober-mascot.png" />
            <div>
              <h2>No gaggles configured</h2>
              <p>
                No configuration is available to the Portal yet. New to Goobers? The guided
                walkthrough builds a working instance step by step.
              </p>
              <RecoveryCommand command="goobers init --guided" />
            </div>
          </section>
        ) : emptyWorkflows ? (
          <section className="empty-state">
            <img alt="" src="/goober-mascot.png" />
            <div>
              <h2>No workflows configured</h2>
              <p>
                Add a workflow definition, then validate the instance before reloading the Portal.
              </p>
              <RecoveryCommand command="goobers validate <instance>" />
            </div>
          </section>
        ) : emptyRuns ? (
          <section className="empty-state">
            <img alt="" src="/goober-mascot.png" />
            <div>
              <h2>No runs recorded</h2>
              <p>Start a configured workflow to create the first run journal.</p>
              <RecoveryCommand command="goobers run <workflow> <instance>" />
            </div>
          </section>
        ) : (
          <>
            <RunSection
              ariaLabel="Active runs"
              overview={overview}
              runs={groups.active}
              title="Active runs"
            />
            <RunSection
              ariaLabel="Recent outcomes"
              overview={overview}
              runs={groups.recent}
              title="Recent outcomes"
            />
          </>
        )}
      </div>

      <div className="overview-configuration">
        <ConfigurationWarnings context="instance" {...configurationWarnings} />
      </div>
    </div>
  );
}

function InstanceSummaryPanel({
  configurationWarningCount,
  overview,
  retry,
  standalone,
}: {
  configurationWarningCount: number;
  overview: OperationalOverview;
  retry: () => void;
  standalone: boolean;
}) {
  const healthy = standalone || overview.health.healthy;
  const starting = !overview.health.ready && overview.instance.status === "starting";
  const tickAge = overview.health.freshness.lastTickAgeMillis;
  const lastTickAt = overview.health.freshness.lastSchedulerTickAt;
  const maintenance = overview.instance.maintenance;
  const recoveryInventory = overview.instance.recoveryInventory;
  const telemetryRetention = overview.instance.telemetryRetention;
  const diagnosticsRequireAttention =
    (recoveryInventory !== undefined && recoveryInventory.state !== "healthy") ||
    maintenance?.state === "failed";
  const [diagnosticsOpen, setDiagnosticsOpen] = useState(
    () =>
      diagnosticsRequireAttention ||
      typeof window.matchMedia !== "function" ||
      !window.matchMedia("(max-width: 1024px)").matches,
  );
  const daemonTitle = standalone
    ? overview.health.ready
      ? "Healthy"
      : "Instance not ready"
    : starting
      ? "Daemon starting"
      : !healthy
        ? "Daemon unhealthy"
        : overview.health.ready
          ? "Healthy"
          : "Daemon starting";

  return (
    <section
      aria-label={
        standalone ? "Local instance status and counts" : "Daemon connection and instance counts"
      }
      className="instance-summary-card"
    >
      <div className="instance-summary-row daemon-summary-row">
        <div className="instance-summary-kind">
          <span
            aria-hidden="true"
            className={
              healthy && overview.health.ready
                ? "instance-summary-icon instance-summary-icon-healthy"
                : "instance-summary-icon instance-summary-icon-warning"
            }
          >
            <Icon name={healthy && overview.health.ready ? "check" : "clock"} size={23} />
          </span>
          <span className="instance-summary-copy">
            <strong>{daemonTitle}</strong>
            <span>
              {healthy && overview.health.ready
                ? "Running normally"
                : "Operator attention required"}
            </span>
            {configurationWarningCount > 0 && (
              <span className="daemon-summary-notices">
                <a
                  className="instance-warning-link"
                  data-focus-restore="instance-configuration-warnings"
                  href={routeHash({ page: "instance-detail", detail: "warnings" })}
                >
                  {configurationWarningCount} configuration{" "}
                  {configurationWarningCount === 1 ? "warning" : "warnings"}
                </a>
              </span>
            )}
          </span>
        </div>
        <dl className="daemon-summary-metrics">
          <div>
            <dt>Gaggles</dt>
            <dd>{overview.instance.counts.gaggles}</dd>
            <span>Configured</span>
          </div>
          <div>
            <dt>Active runs</dt>
            <dd>{overview.instance.counts.activeRuns}</dd>
            <span>
              {overview.instance.counts.activeRuns === 0 ? "None executing" : "Currently executing"}
            </span>
          </div>
          <div>
            <dt>Data freshness</dt>
            <dd className="daemon-freshness-value">
              {tickAge === null ? "Unavailable" : `${formatDuration(tickAge)} ago`}
              <button aria-label="Refresh instance status" onClick={retry} type="button">
                <Icon name="refresh" size={18} />
              </button>
            </dd>
            <span>
              {lastTickAt ? <Timestamp value={lastTickAt} /> : "Latest scheduler tick unavailable"}
            </span>
          </div>
        </dl>
      </div>

      {overview.health.startup && <StartupRecoverySummary startup={overview.health.startup} />}

      <details
        className="overview-diagnostics"
        onToggle={(event) => setDiagnosticsOpen(event.currentTarget.open)}
        open={diagnosticsRequireAttention || diagnosticsOpen}
      >
        <summary>
          <span>
            <strong>Diagnostics and capacity</strong>
            <span>
              {diagnosticsRequireAttention
                ? "Operator attention required"
                : "Recovery and retention"}
            </span>
          </span>
          <Icon name="chevron" size={14} />
        </summary>
        <div className="overview-diagnostics-content">
          {recoveryInventory && <RecoveryInventorySummary inventory={recoveryInventory} />}

          {maintenance && <MaintenanceSummary maintenance={maintenance} />}

          {telemetryRetention && (
            <div
              aria-label={`Telemetry retention ${telemetryRetention.enabled ? "enabled" : "disabled"}`}
              className="instance-summary-row"
              role="status"
            >
              <div className="instance-summary-kind">
                <span aria-hidden="true" className="instance-summary-icon">
                  <Icon name="chart" size={24} />
                </span>
                <span className="instance-summary-copy">
                  <strong>Telemetry retention</strong>
                  <span>Run history and diagnostics</span>
                </span>
              </div>
              <div className="instance-summary-result">
                <strong>
                  <span aria-hidden="true" className="result-check">
                    <Icon name="check" size={16} />
                  </span>
                  {telemetryRetention.enabled ? "Enabled" : "Disabled"}
                </strong>
                <span>
                  {formatRetentionWindow(telemetryRetention.window)} · Max{" "}
                  {telemetryRetention.maxRuns} runs
                </span>
                {telemetryRetention.enabled && telemetryRetention.enforceAt && (
                  <span>Enforcement begins {formatTimestamp(telemetryRetention.enforceAt)}</span>
                )}
                {telemetryRetention.lastPassAt && (
                  <span>
                    Last pass {telemetryRetention.lastPassMode ?? "completed"} ·{" "}
                    {telemetryRetention.candidateCount} candidates ·{" "}
                    {formatTimestamp(telemetryRetention.lastPassAt)}
                  </span>
                )}
              </div>
            </div>
          )}
        </div>
      </details>
    </section>
  );
}

function StartupRecoverySummary({ startup }: { startup: StartupStatus }) {
  const candidate = startup.blockingCandidate;
  const progress = candidate?.progress;
  return (
    <div aria-label="Startup recovery progress" className="instance-summary-row" role="status">
      <div className="instance-summary-kind">
        <span aria-hidden="true" className="instance-summary-icon instance-summary-icon-warning">
          <Icon name="clock" size={24} />
        </span>
        <span className="instance-summary-copy">
          <strong>Startup recovery</strong>
          <span>
            API plane is available; scheduler readiness is waiting on {startup.phase}
            {startup.target ? ` (${startup.target})` : ""}.
          </span>
        </span>
      </div>
      <div className="instance-summary-result">
        <strong>
          {progress
            ? `${progress.examined}/${progress.total} candidates examined`
            : "Recovery in progress"}
        </strong>
        <span>
          Elapsed {formatSeconds(startup.elapsedSeconds)} · Budget{" "}
          {formatSeconds(startup.budgetSeconds)}
          {startup.budgetState ? ` · ${startup.budgetState}` : ""}
        </span>
        {candidate ? (
          <span>
            Blocking{" "}
            {candidate.runId ? `run ${candidate.runId}` : "candidate"}
            {candidate.gaggle || candidate.workflow
              ? ` (${candidate.gaggle ?? "-"}/${candidate.workflow ?? "-"})`
              : ""}
            {candidate.disposition ? ` · ${candidate.disposition}` : ""}
            {candidate.phase ? ` · ${candidate.phase}` : ""}
            {candidate.operation ? ` · ${candidate.operation}` : ""}
          </span>
        ) : startup.phase === "crash-resume" ? (
          <span>Waiting for the resumeComplete gate after candidate classification.</span>
        ) : null}
        {candidate?.lastProgressAt && (
          <span>Last progress {formatDuration(Math.max(0, Date.now() - Date.parse(candidate.lastProgressAt)))} ago</span>
        )}
      </div>
    </div>
  );
}

function formatSeconds(seconds: number | undefined): string {
  if (!Number.isFinite(seconds)) {
    return "unavailable";
  }
  return formatDuration(Math.max(0, Math.round((seconds ?? 0) * 1000)));
}

/**
 * Recovery-snapshot inventory occupancy (#5343).
 *
 * This remains summarized on Overview because of what a full inventory does:
 * worktree cleanup cannot complete without a durable
 * recovery handoff, an uncleaned worktree cannot be reused, and stages then
 * fail at `create worktree` - so the runs that break are unrelated to whatever
 * filled the inventory, and the only symptom an operator sees today is a
 * scattering of individual run failures. It is an instance-wide condition and
 * belongs next to the instance's other capacity signals.
 */
function RecoveryInventorySummary({ inventory }: { inventory: RecoveryInventoryStatus }) {
  const { state } = inventory;
  const critical = state === "exhausted";
  const elevated = critical || state === "warning";
  const percent =
    inventory.limit > 0 ? Math.round((inventory.used / inventory.limit) * 100) : undefined;

  // Only the elevated states are a live region: an ordinary capacity reading
  // announcing itself on every poll is noise, and it would also make this row
  // indistinguishable from the daemon's own freshness status.
  return (
    <div
      aria-label={`Recovery inventory ${state}`}
      className={`instance-summary-row${critical ? " instance-summary-row-error" : ""}${
        state === "warning" ? " instance-summary-row-warning" : ""
      }`}
      role={elevated ? "alert" : "group"}
    >
      <div className="instance-summary-kind">
        <span
          aria-hidden="true"
          className={
            elevated
              ? "instance-summary-icon instance-summary-icon-warning"
              : "instance-summary-icon"
          }
        >
          <Icon name={elevated ? "alert" : "artifact"} size={24} />
        </span>
        <span className="instance-summary-copy">
          <strong>Recovery inventory</strong>
          <span>Durable handoffs for worktree cleanup</span>
        </span>
      </div>
      <div className="instance-summary-result instance-summary-result-with-actions">
        <div className="instance-summary-result-copy">
          <strong>
            <span aria-hidden="true" className="result-check">
              <Icon name={elevated ? "alert" : "check"} size={16} />
            </span>
            {RECOVERY_INVENTORY_HEADLINES[state]}
            {state !== "unavailable" && (
              <span>
                {" "}
                &middot; {inventory.used}/{inventory.limit} slots
                {percent === undefined ? "" : ` (${percent}%)`}
              </span>
            )}
          </strong>
          <span>{recoveryInventoryExplanation(inventory)}</span>
          {inventory.overflow > 0 && (
            <span>
              {inventory.overflow} {inventory.overflow === 1 ? "snapshot" : "snapshots"} held as
              mirror refs without a bundle until capacity frees
            </span>
          )}
          {inventory.unreadable > 0 && (
            <span>
              {inventory.unreadable} incomplete{" "}
              {inventory.unreadable === 1 ? "reservation" : "reservations"} still occupying slots
            </span>
          )}
          {inventory.earliestRetainUntil && (
            <span>
              Earliest retention deadline {formatTimestamp(inventory.earliestRetainUntil)}
            </span>
          )}
          {inventory.error && <span>{inventory.error}</span>}
          {elevated && <RecoveryReclaimGuidance inventory={inventory} />}
        </div>
        <div className="instance-summary-actions">
          <a
            className="instance-warning-link"
            data-focus-restore="instance-recovery-detail"
            href={routeHash({ page: "instance-detail", detail: "recovery" })}
          >
            View recovery metadata
          </a>
        </div>
      </div>
    </div>
  );
}

/**
 * What to run when the inventory is filling (#5343 follow-up). Commands, not
 * controls: abandoning a snapshot discards retained work, so it stays a
 * deliberate operator decision taken at a terminal, but the operator no
 * longer has to reconstruct run ids, refs and digests from a guide.
 */
function RecoveryReclaimGuidance({ inventory }: { inventory: RecoveryInventoryStatus }) {
  const candidates = inventory.reclaimCandidates ?? [];
  const total = inventory.reclaimCandidatesTotal ?? candidates.length;
  return (
    <div className="recovery-reclaim">
      {inventory.reclaimHold && <RecoveryReclaimHoldNotice hold={inventory.reclaimHold} />}
      {candidates.length > 0 ? (
        <>
          <span>
            {total > candidates.length
              ? `Oldest ${candidates.length} of ${total} snapshots from finished runs. Restore what you still want, abandon the rest.`
              : "Snapshots from finished runs. Restore what you still want, abandon the rest."}
          </span>
          <ul className="recovery-reclaim-list">
            {candidates.map((candidate) => (
              <li aria-label={`Recovery snapshot ${candidate.runId}`} key={`${candidate.runId}\u0000${candidate.ref}`}>
                <span className="recovery-reclaim-run">
                  <a href={routeHash({ page: "run", id: candidate.runId })}>{candidate.runId}</a>
                  {" "}&middot; {candidate.phase} &middot; retained until {formatTimestamp(candidate.retainUntil)}
                </span>
                <RecoveryCommand command={candidate.inspectCommand} label="Inspect:" />
                {candidate.restoreCommand && (
                  <RecoveryCommand command={candidate.restoreCommand} label="Restore:" />
                )}
                {candidate.abandonCommand ? (
                  <RecoveryCommand command={candidate.abandonCommand} label="Abandon:" />
                ) : (
                  candidate.abandoned && <span>Abandoned; waiting for the next retention pass to free its slot.</span>
                )}
              </li>
            ))}
          </ul>
        </>
      ) : (
        <span>No snapshot belongs to a finished run yet, so none can be abandoned. Slots free as runs finish.</span>
      )}
      {inventory.statusCommand && (
        <RecoveryCommand command={inventory.statusCommand} label="List all:" />
      )}
    </div>
  );
}

function RecoveryReclaimHoldNotice({ hold }: { hold: RecoveryReclaimHold }) {
  const reason =
    hold.reason === "disabled"
      ? "Retention is disabled"
      : hold.reason === "dry-run"
        ? "Retention is in dry-run mode"
        : hold.until
          ? `Retention is in its first-enable grace window until ${formatTimestamp(hold.until)}`
          : "Retention will hold deletions for a 7-day first-enable grace window";
  return (
    <span className="recovery-reclaim-hold">
      {reason}, so abandoned snapshots free no slots. Set <code>{hold.setting}</code>
      {hold.configFile ? (
        <>
          {" "}in <code>{hold.configFile}</code>
        </>
      ) : (
        " in instance.yaml"
      )}{" "}
      to release it.
    </span>
  );
}

/**
 * The consequence, stated in the operator's terms, because the failure they
 * will otherwise see names neither recovery nor capacity.
 */
function recoveryInventoryExplanation(inventory: RecoveryInventoryStatus): string {
  switch (inventory.state) {
    case "exhausted":
      return "Every configured slot is in use. Durable handoffs, worktree cleanup and unrelated runs fail until capacity is freed.";
    case "warning":
      return `Occupancy has passed the ${inventory.highWaterPercent}% high-water mark. When it fills, durable handoffs, worktree cleanup and unrelated runs fail, including runs that did not fill it.`;
    case "unavailable":
      return "Occupancy could not be measured, so it cannot be reported as healthy.";
    default:
      return "Durable handoffs, worktree cleanup and unrelated runs fail when it is full.";
  }
}

/**
 * Capacity-specific wording, deliberately not reusing the daemon row's
 * "Healthy"/"Unavailable": two rows in the same card reading identically tell
 * an operator less than two rows that each say what they are about.
 */
const RECOVERY_INVENTORY_HEADLINES: Record<RecoveryInventoryStatus["state"], string> = {
  exhausted: "Full",
  healthy: "Within limits",
  unavailable: "Not measured",
  warning: "Filling up",
};

function MaintenanceSummary({ maintenance }: { maintenance: MaintenanceStatus }) {
  const completedAt = maintenance.lastCompletedAt;
  const state = maintenance.state;
  const stateLabel =
    state === "none" ? "Idle" : `${state.slice(0, 1).toUpperCase()}${state.slice(1)}`;
  const statusLabel = state === "none" ? "No retention sweep running" : `Retention sweep ${state}`;
  const age = completedAt
    ? `${formatDuration(Math.max(0, Date.now() - Date.parse(completedAt)))} ago`
    : undefined;

  return (
    <div
      aria-label={statusLabel}
      className={`instance-summary-row${state === "failed" ? " instance-summary-row-error" : ""}`}
      role={state === "failed" ? "alert" : "status"}
    >
      <div className="instance-summary-kind">
        <span aria-hidden="true" className="instance-summary-icon">
          <Icon name="database" size={25} />
        </span>
        <span className="instance-summary-copy">
          <strong>Retention sweep</strong>
          <span>Periodic cleanup of run data</span>
        </span>
      </div>
      <div className="instance-summary-result instance-summary-result-with-actions">
        <div className="instance-summary-result-copy">
          <strong>
            <span aria-hidden="true" className="result-check">
              <Icon name={state === "failed" ? "alert" : "check"} size={16} />
            </span>
            {stateLabel}
            {age && <span> · {age}</span>}
          </strong>
          {maintenance.errorSummary ? (
            <span>
              {`${maintenance.trigger.slice(0, 1).toUpperCase()}${maintenance.trigger.slice(1)}`}{" "}
              trigger · {maintenance.errorSummary}
            </span>
          ) : (
            <span>
              {`${maintenance.trigger.slice(0, 1).toUpperCase()}${maintenance.trigger.slice(1)}`}{" "}
              trigger · {maintenance.removed} removed · {maintenance.candidates} candidates
            </span>
          )}
          {state === "none" && completedAt && (
            <span>Last completed at {formatTimestamp(completedAt)}</span>
          )}
          {maintenance.currentPhase && state === "running" && (
            <span>{maintenance.currentPhase}</span>
          )}
          {maintenance.lastProgressAt && state === "running" && (
            <span>
              Last progress{" "}
              {formatDuration(Math.max(0, Date.now() - Date.parse(maintenance.lastProgressAt)))} ago
            </span>
          )}
        </div>
        <div className="instance-summary-actions">
          <a
            className="instance-warning-link"
            data-focus-restore="instance-retention-detail"
            href={routeHash({ page: "instance-detail", detail: "retention" })}
          >
            View retention details
          </a>
        </div>
      </div>
    </div>
  );
}

function formatRetentionWindow(window: string): string {
  const match = /^(\d+)d$/.exec(window);
  return match ? `${match[1]} days` : window;
}

function RunSection({
  ariaLabel,
  overview,
  runs,
  title,
}: {
  ariaLabel: string;
  overview: OperationalOverview;
  runs: RunSummary[];
  title: string;
}) {
  const active = title === "Active runs";
  return (
    <section className="content-section">
      <SectionHeading
        title={title}
        className=""
        actions={
          <>
            <span className="section-count">{runs.length}</span>
          </>
        }
      />
      {runs.length === 0 ? (
        <p className="inline-empty">{active ? "No runs are active." : "No recent outcomes."}</p>
      ) : (
        <DataList
          ariaLabel={ariaLabel}
          columns={
            active
              ? ["Run", "Workflow", "Current stage", "Elapsed"]
              : ["Run", "Outcome", "Workflow", "Duration"]
          }
          gridClassName={active ? "run-grid" : "outcome-grid"}
        >
          {runs.map((run) => (
            <DataRow
              href={routeHash({ page: "run", id: run.id })}
              interactiveChildren
              key={run.id}
              label={`Open run ${run.id}`}
            >
              <span className="row-primary">
                <span className="row-title" title={runLabel(run)}>
                  {runLabel(run)}
                </span>
                <span className="row-subtitle" title={runContextSubtitle(run, active)}>
                  {active && run.operator ? operatorSubtitle(run) : runContextSubtitle(run, active)}
                  {!active && run.finishedAt && (
                    <>
                      {" · "}
                      <Timestamp
                        aria-label={`Completed ${formatPreciseTimestamp(run.finishedAt)}`}
                        value={run.finishedAt}
                        title={`Completed ${formatPreciseTimestamp(run.finishedAt)}`}
                        prefix="Completed "
                      />
                    </>
                  )}
                </span>
                {active && operatorContext(run) ? (
                  <span className="row-subtitle">{operatorContext(run)}</span>
                ) : null}
              </span>
              {active ? (
                <>
                  <span className="row-workflow">
                    <span>{workflowIdentity(run)}</span>
                    <a
                      className="workflow-detail-link"
                      href={routeHash({
                        page: "workflow",
                        gaggle: run.gaggle,
                        id: run.workflow,
                      })}
                    >
                      Open workflow
                    </a>
                  </span>
                  <span className="stage-progress">
                    <span aria-hidden="true" className="stage-progress-mark" />
                    {operatorProgress(run)}
                  </span>
                </>
              ) : (
                <>
                  <StatusBadge status={run.phase} />
                  <span className="row-workflow">
                    <span>{workflowIdentity(run)}</span>
                    <a
                      className="workflow-detail-link"
                      href={routeHash({
                        page: "workflow",
                        gaggle: run.gaggle,
                        id: run.workflow,
                      })}
                    >
                      Open workflow
                    </a>
                  </span>
                </>
              )}
              <RunTiming run={run} />
            </DataRow>
          ))}
        </DataList>
      )}
    </section>
  );
}

function runLabel(run: RunSummary): string {
  if (run.operator?.issue) {
    return `#${run.operator.issue.number}${run.operator.issue.title ? ` ${run.operator.issue.title}` : ""}`;
  }
  return run.id;
}

function runContextSubtitle(run: RunSummary, active: boolean): string {
  if (active && run.operator) {
    return operatorSubtitle(run);
  }
  const context = workflowIdentity(run);
  const ref =
    run.trigger.ref && run.trigger.ref !== run.id && run.trigger.ref !== run.workflow
      ? ` · ${run.trigger.kind} ${run.trigger.ref}`
      : "";
  return `${context}${ref}`;
}

interface AttentionGroup {
  key: string;
  domId: string;
  label: string;
  context: string;
  diagnosis: string;
  severity: AttentionSeverity;
  severitySource: RunSummary;
  latest: RunSummary;
  runs: RunSummary[];
}

function groupAttentionRuns(
  runs: RunSummary[],
  overview: OperationalOverview,
  failureReasons: FailureReasons,
): AttentionGroup[] {
  const grouped = new Map<string, AttentionGroup>();
  const availableRuns = [
    ...overview.groups.active,
    ...overview.groups.attention,
    ...overview.groups.recent,
  ];
  for (const run of runs) {
    const issue = run.operator?.issue;
    const severity = attentionSeverity(
      run,
      availableRuns,
      failureReasons.get(run.id),
    );
    const category = attentionCategory(run, failureReasons, severity);
    const key = issue
      ? `issue:${issue.number}`
      : `workflow:${run.gaggle}/${run.workflow}/${category}`;
    const existing = grouped.get(key);
    if (existing) {
      existing.runs.push(run);
      if (Date.parse(run.lastActivityAt) > Date.parse(existing.latest.lastActivityAt)) {
        existing.latest = run;
      }
      const severityRank = attentionSeverityRank(severity);
      const existingSeverityRank = attentionSeverityRank(existing.severity);
      if (
        severityRank < existingSeverityRank ||
        (severityRank === existingSeverityRank &&
          Date.parse(run.lastActivityAt) >
            Date.parse(existing.severitySource.lastActivityAt))
      ) {
        existing.label = issue
          ? `#${issue.number}${issue.title ? ` ${issue.title}` : ""}`
          : `${workflowIdentity(run)} · ${attentionCategoryLabel(run, failureReasons, severity)}`;
        existing.context = workflowIdentity(run);
        existing.diagnosis = attentionDiagnosis(run, failureReasons, severity);
        existing.severity = severity;
        existing.severitySource = run;
      }
      continue;
    }
    grouped.set(key, {
      key,
      domId: key.replace(/[^a-zA-Z0-9_-]/g, "-"),
      label: issue
        ? `#${issue.number}${issue.title ? ` ${issue.title}` : ""}`
        : `${workflowIdentity(run)} · ${attentionCategoryLabel(run, failureReasons, severity)}`,
      context: workflowIdentity(run),
      diagnosis: attentionDiagnosis(run, failureReasons, severity),
      severity,
      severitySource: run,
      latest: run,
      runs: [run],
    });
  }

  return [...grouped.values()].sort(
    (left, right) =>
      attentionSeverityRank(left.severity) - attentionSeverityRank(right.severity) ||
      Date.parse(right.latest.lastActivityAt) - Date.parse(left.latest.lastActivityAt),
  );
}

function workflowIdentity(run: Pick<RunSummary, "gaggle" | "workflow">): string {
  return `${run.gaggle} / ${run.workflow}`;
}

function attentionCategory(
  run: RunSummary,
  failureReasons: FailureReasons,
  severity: AttentionSeverity,
): string {
  if (severity === "stalled") {
    return "stalled";
  }
  if (run.phase === "escalated") {
    return `escalated:${run.terminalReason ?? "review"}`;
  }
  const reason = failureReasons.get(run.id);
  return `failed:${reason?.code ?? run.operator?.latestError?.code ?? run.terminalReason ?? "unknown"}`;
}

function attentionCategoryLabel(
  run: RunSummary,
  failureReasons: FailureReasons,
  severity: AttentionSeverity,
): string {
  if (severity === "stalled") {
    return "No recent progress";
  }
  if (run.phase === "escalated") {
    return "Escalated for review";
  }
  const reason = failureReasons.get(run.id);
  return reason?.code ?? run.operator?.latestError?.code ?? "Failed";
}

function attentionDiagnosis(
  run: RunSummary,
  failureReasons: FailureReasons,
  severity: AttentionSeverity,
): string {
  if (severity === "stalled") {
    return "No progress beyond the runner liveness threshold.";
  }
  if (run.phase === "escalated") {
    return run.terminalReason ?? "Escalated and needs human review.";
  }
  const reason = failureReasons.get(run.id);
  if (reason) {
    return `${reason.code || "failed"}${reason.message ? ` · ${reason.message}` : ""}`;
  }
  return run.terminalReason ?? "Failed and needs investigation.";
}

function attentionSeverityLabel(severity: AttentionSeverity): string {
  return severity === "blocked" ? "Blocked" : severity === "stalled" ? "Stalled" : "Warning";
}

function SelectionCheckbox({
  ariaLabel,
  checked,
  indeterminate,
  onChange,
}: {
  ariaLabel: string;
  checked: boolean;
  indeterminate: boolean;
  onChange: () => void;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) {
      ref.current.indeterminate = indeterminate;
    }
  }, [indeterminate]);
  return (
    <input
      aria-label={ariaLabel}
      checked={checked}
      className="attention-select"
      onChange={onChange}
      ref={ref}
      type="checkbox"
    />
  );
}

function operatorSubtitle(run: RunSummary): string {
  const operator = run.operator;
  if (!operator) {
    return run.id;
  }
  const heartbeat =
    operator.heartbeatAgeMillis === undefined
      ? "no heartbeat"
      : `${operator.liveness} heartbeat ${formatDuration(operator.heartbeatAgeMillis)} ago`;
  return `${operator.trajectory} · ${heartbeat} · claim ${operator.claim.leaseStatus}/${operator.claim.providerMarker}`;
}

function operatorProgress(run: RunSummary): string {
  const operator = run.operator;
  if (!operator) {
    return run.currentStage ?? "Awaiting stage";
  }
  const pr = operator.pullRequest
    ? `PR #${operator.pullRequest.id}`
    : operator.prOpenerStage
      ? `PR via ${operator.prOpenerStage}`
      : "no PR stage";
  return `${operator.currentStage ?? "Awaiting stage"} · ${pr} · ${operator.nextTransition ?? "no next transition"}`;
}

function operatorContext(run: RunSummary): string {
  const operator = run.operator;
  if (!operator) {
    return "";
  }
  const details: string[] = [];
  if (operator.latestError) {
    details.push(
      `Error ${operator.latestError.code}${operator.latestError.message ? `: ${operator.latestError.message}` : ""}`,
    );
  }
  if (operator.review) {
    details.push(
      `Review ${operator.review.verdict}${operator.review.rationale ? `: ${operator.review.rationale}` : ""}`,
    );
  }
  if (operator.potentialBlockers.length > 0) {
    details.push(`Blockers: ${operator.potentialBlockers.join("; ")}`);
  }
  // Kept out of "Blockers" and labelled as a reader limitation: this is what the
  // read invocation could not verify, not something impeding the run (#3346).
  if (operator.diagnosticsLimitations && operator.diagnosticsLimitations.length > 0) {
    details.push(
      `Diagnostics limited (not a run blocker): ${operator.diagnosticsLimitations.join("; ")}`,
    );
  }
  return details.join(" · ");
}

function attentionHeading(count: number): string {
  if (count === 0) {
    return "No runs need attention.";
  }
  return count === 1 ? "One run needs attention." : `${count} runs need attention.`;
}

function formatDuration(milliseconds: number): string {
  const totalSeconds = Math.max(0, Math.round(milliseconds / 1_000));
  const hours = Math.floor(totalSeconds / 3_600);
  const minutes = Math.floor((totalSeconds % 3_600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) {
    return `${hours}h ${minutes}m`;
  }
  return minutes > 0 ? `${minutes}m ${seconds}s` : `${seconds}s`;
}
