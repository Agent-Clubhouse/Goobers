import { useEffect, useMemo, useRef, useState } from "react";
import { RunTiming } from "../components/RunTiming";
import type { DaemonClient, MaintenanceStatus, RunSummary } from "../api/types";
import { useAttentionCollapsed } from "../attentionCollapse";
import { useAttentionDismissals } from "../attentionDismissals";
import type { ConfigurationWarningsProps } from "../components/ConfigurationWarnings";
import { ConfigurationWarnings } from "../components/ConfigurationWarnings";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { RecoveryCommand } from "../components/RecoveryAction";
import { ScopePivot } from "../components/ScopePivot";
import {
  incompleteRunPhasesMessage,
  type OperationalOverview,
  useOperationalOverview,
  workflowDisplayName,
} from "../operationalData";
import { routeHash } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
import { Icon } from "../ui/Icon";
import { StatusBadge } from "../ui/StatusBadge";
import { useFailureReasons, type FailureReasons } from "../overviewFailures";
import { configurationWarningKey } from "../configurationWarnings";

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
  const attentionFailedIds =
    query.state.status === "ready" || query.state.status === "stale"
      ? query.state.data.groups.attention
          .filter((run) => run.phase === "failed")
          .map((run) => run.id)
      : [];
  const failureReasons = useFailureReasons(client, attentionFailedIds);

  if (query.state.status === "loading") {
    return <DaemonLoadingState standalone={standalone} />;
  }
  if (query.state.status === "error") {
    return <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />;
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  return (
    <Overview
      configurationWarnings={configurationWarnings}
      failureReasons={failureReasons}
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
  const activeConfigurationWarningCount =
    configurationWarnings.state.status === "ready" ||
    configurationWarnings.state.status === "stale"
      ? configurationWarnings.state.data.filter(
          (warning) =>
            !configurationWarnings.dismissedWarningKeys.has(configurationWarningKey(warning)),
        ).length
      : 0;

  const { dismissedRunIds, dismiss, restore } = useAttentionDismissals();
  const [attentionCollapsed, setAttentionCollapsed] = useAttentionCollapsed();
  const [selectedRunIds, setSelectedRunIds] = useState<ReadonlySet<string>>(() => new Set());
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
  const activeAttentionIds = new Set(activeAttention.map((run) => run.id));
  const visibleSelectedRunIds = [...selectedRunIds].filter((runId) =>
    activeAttentionIds.has(runId),
  );
  const allVisibleSelected =
    activeAttention.length > 0 && visibleSelectedRunIds.length === activeAttention.length;

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
    <>
      <header className="page-heading">
        <p className="page-kicker">Instance overview</p>
        {overview.instance.rootIdentity?.decommissionedAt && (
          <p role="alert">Historical root; do not use. Decommissioned {overview.instance.rootIdentity.decommissionedAt}: {overview.instance.rootIdentity.decommissionReason}</p>
        )}
        {overview.instance.rootIdentity?.identityProblem && <p role="status">{overview.instance.rootIdentity.identityProblem}</p>}
        {overview.instance.rootIdentity?.lifecycleProblem && <p role="alert">{overview.instance.rootIdentity.lifecycleProblem}</p>}
        <h1>
          {overview.loadingSections?.inventory || overview.loadingSections?.runs
            ? (
              <span aria-label="Loading overview" className="overview-loading-title" role="status">
                <span aria-hidden="true">.</span>
                <span aria-hidden="true">.</span>
                <span aria-hidden="true">.</span>
              </span>
            )
            : emptyInstance
              ? standalone
                ? overview.health.ready
                  ? "Instance is ready — Healthy."
                  : "Instance is starting."
                : !healthy
                  ? "Daemon is unhealthy."
                  : overview.health.ready
                    ? "Daemon is running — Healthy."
                    : "Daemon is starting."
              : !healthy
                ? "Daemon is unhealthy."
                : activeAttention.length === 0
                  ? standalone
                    ? "Instance is ready — Healthy."
                    : "Daemon is running — Healthy."
                  : attentionHeading(activeAttention.length)}
        </h1>
        {emptyInstance && (
          <p>No gaggles are configured. Add gaggle definitions to begin observing workflows and runs.</p>
        )}
        <dl className="instance-identity">
          <div>
            <dt>Instance name</dt>
            <dd>{overview.instance.name}</dd>
          </div>
          <div>
            <dt>Version</dt>
            <dd>
              {overview.health.build ? (
                <span title={`Commit ${overview.health.build.commit} · Built ${overview.health.build.date}`}>
                  {overview.health.build.version}
                  {overview.health.build.commit
                    ? ` · ${overview.health.build.commit.slice(0, 7)}`
                    : ""}
                </span>
              ) : (
                "Unavailable"
              )}
              {overview.health.update?.available ? (
                <span className="version-update-available">
                  {" "}
                  · {overview.health.update.latestVersion} available
                </span>
              ) : null}
            </dd>
          </div>
          <div>
            <dt>Computer name</dt>
            <dd><code>{overview.instance.computerName || "unavailable"}</code></dd>
          </div>
          <div>
            <dt>Instance root</dt>
            <dd><code>{overview.instance.instanceRoot}</code></dd>
          </div>
          <div>
            <dt>Instance ID</dt>
            <dd><code>{overview.instance.rootIdentity?.id || "unavailable"}</code></dd>
          </div>
        </dl>
      </header>

      {/* A section that failed to load must say so. Without this the page would
          render an empty run list identically to a genuinely idle instance,
          which is a worse failure than the blank page it replaced (#1709). */}
      {overview.sectionErrors?.inventory && (
        <div className="inline-empty section-error" role="alert">
          <span>
            The gaggle and workflow inventory could not be read just now. Inventory-backed names
            and empty-state guidance are unavailable until it loads; daemon health, counts, and run
            data below remain current.
          </span>
          <button className="text-button" onClick={retry} type="button">
            Retry inventory
          </button>
        </div>
      )}
      {overview.sectionErrors?.runs && (
        <div className="inline-empty section-error" role="alert">
          <span>
            Run activity could not be read just now, so the run groups below may be incomplete or
            out of date. Everything else on this page is current.
          </span>
          <button className="text-button" onClick={retry} type="button">
            Retry run activity
          </button>
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

      <InstanceStrip
        configurationWarningCount={activeConfigurationWarningCount}
        overview={overview}
        standalone={standalone}
      />

      {groups.attention.length > 0 && (
        <section className="content-section attention-section">
          <div className="section-heading">
            <h2>Needs attention</h2>
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
                <button
                  className="text-button"
                  onClick={() => dismissRuns(visibleSelectedRunIds)}
                  type="button"
                >
                  Dismiss {visibleSelectedRunIds.length} selected
                </button>
              )}
              {dismissedAttention.length > 0 && (
                <button
                  className="text-button"
                  onClick={() => setShowDismissed((current) => !current)}
                  type="button"
                >
                  {showDismissed ? "Hide dismissed" : `Show dismissed (${dismissedAttention.length})`}
                </button>
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
          </div>
          <div hidden={attentionCollapsed} id="attention-section-body">
            {activeAttention.length === 0 ? (
              <p className="inline-empty">Nothing needs attention right now.</p>
            ) : (
              <div className="attention-list">
                {attentionGroups.map((group) => {
                  const selectedCount = group.runs.filter((run) =>
                    selectedRunIds.has(run.id),
                  ).length;
                  const expanded = expandedAttentionGroups.has(group.key);
                  return (
                    <div className="attention-group" key={group.key}>
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
                          <span title={group.diagnosis}>
                            {group.runs.length} {group.runs.length === 1 ? "run" : "runs"} ·{" "}
                            {group.diagnosis}
                          </span>
                        </span>
                        <span className="attention-meta">
                          <span title={group.context}>{group.context}</span>
                          <time dateTime={group.latest.lastActivityAt}>
                            Latest {formatTimestamp(group.latest.lastActivityAt)}
                          </time>
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
                        {group.runs.map((run) => (
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
                              <span>{attentionDiagnosis(run, failureReasons)}</span>
                            </a>
                            <ScopePivot
                              label={workflowDisplayName(overview, run)}
                              scope={{ gaggle: run.gaggle, workflow: run.workflow }}
                            />
                            <time dateTime={run.lastActivityAt}>
                              {formatTimestamp(run.lastActivityAt)}
                            </time>
                            <button
                              aria-label={`Dismiss run ${run.id}`}
                              className="attention-dismiss"
                              onClick={() => dismissRuns([run.id])}
                              type="button"
                            >
                              Dismiss
                            </button>
                          </div>
                        ))}
                      </div>
                    </div>
                  );
                })}
              </div>
            )}
            {showDismissed && dismissedAttention.length > 0 && (
              <div className="attention-dismissed-list">
                <div className="section-heading">
                  <p className="section-kicker">Dismissed</p>
                  <button
                    className="text-button"
                    onClick={() => restore(dismissedAttention.map((run) => run.id))}
                    type="button"
                  >
                    Restore all
                  </button>
                </div>
                {dismissedAttention.map((run) => (
                  <div className="attention-row attention-row-dismissed" key={run.id}>
                    <span className="attention-copy">
                      <strong>{runLabel(run)}</strong>
                      <span>{workflowDisplayName(overview, run)}</span>
                    </span>
                    <button
                      aria-label={`Undo dismiss for run ${run.id}`}
                      className="text-button"
                      onClick={() => restore([run.id])}
                      type="button"
                    >
                      Undo
                    </button>
                  </div>
                ))}
              </div>
            )}
          </div>
        </section>
      )}

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
            <p>Add a workflow definition, then validate the instance before reloading the Portal.</p>
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

      <ConfigurationWarnings context="instance" {...configurationWarnings} />
    </>
  );
}

function renderMaintenanceStatus(maintenance: MaintenanceStatus) {
  const hasLastCompletedSweep = Boolean(maintenance.lastCompletedAt || maintenance.lastResult);
  const phase = maintenance.currentPhase;
  const triggerLabel = maintenance.trigger ? `${maintenance.trigger} trigger` : "";
  const lastProgressLabel = maintenance.lastProgressAt
    ? `last progress ${formatDuration(Math.max(0, Date.now() - Date.parse(maintenance.lastProgressAt)))} ago`
    : "";

  switch (maintenance.state) {
    case "running":
      return (
        <div className="maintenance-indicator" role="status" aria-live="polite">
          <strong>Retention sweep running</strong>
          {triggerLabel && <span>{triggerLabel}</span>}
          {maintenance.startedAt && (
            <span>running for {formatDuration(Math.max(0, Date.now() - Date.parse(maintenance.startedAt)))}</span>
          )}
          {lastProgressLabel && <span>{lastProgressLabel}</span>}
          {phase && <span>{phase}</span>}
          <span>{maintenance.removed} removed, {maintenance.candidates} candidates</span>
        </div>
      );
    case "failed":
      return (
        <div className="maintenance-indicator maintenance-indicator-error" role="alert">
          <strong>Retention sweep failed</strong>
          {triggerLabel && <span>{triggerLabel}</span>}
          {maintenance.errorSummary && <span>{maintenance.errorSummary}</span>}
          {maintenance.lastProgressAt && (
            <span>last progress {formatTimestamp(maintenance.lastProgressAt)}</span>
          )}
        </div>
      );
    case "completed":
      return (
        <div className="maintenance-indicator" role="status" aria-live="polite">
          <strong>Retention sweep completed</strong>
          {triggerLabel && <span>{triggerLabel}</span>}
          {maintenance.lastCompletedAt && (
            <span>latest at {formatTimestamp(maintenance.lastCompletedAt)}</span>
          )}
          {lastProgressLabel && <span>{lastProgressLabel}</span>}
          {phase && <span>{phase}</span>}
          <span>{maintenance.removed} removed, {maintenance.candidates} candidates</span>
        </div>
      );
    case "cancelled":
      return (
        <div className="maintenance-indicator" role="status" aria-live="polite">
          <strong>Retention sweep cancelled</strong>
          {triggerLabel && <span>{triggerLabel}</span>}
          {maintenance.lastCompletedAt && (
            <span>latest at {formatTimestamp(maintenance.lastCompletedAt)}</span>
          )}
        </div>
      );
    case "queued":
      return (
        <div className="maintenance-indicator" role="status" aria-live="polite">
          <strong>Retention sweep queued</strong>
          {triggerLabel && <span>{triggerLabel}</span>}
          {maintenance.lastCompletedAt && (
            <span>last completed at {formatTimestamp(maintenance.lastCompletedAt)}</span>
          )}
        </div>
      );
    case "none":
      if (!hasLastCompletedSweep) {
        return null;
      }
      return (
        <div className="maintenance-indicator" role="status" aria-live="polite">
          <strong>No retention sweep running</strong>
          {triggerLabel && <span>{triggerLabel}</span>}
          {maintenance.lastCompletedAt && (
            <span>last completed at {formatTimestamp(maintenance.lastCompletedAt)}</span>
          )}
        </div>
      );
    default:
      return null;
  }
}

function InstanceStrip({
  configurationWarningCount,
  overview,
  standalone,
}: {
  configurationWarningCount: number;
  overview: OperationalOverview;
  standalone: boolean;
}) {
  const healthy = standalone || overview.health.healthy;
  const tickAge = overview.health.freshness.lastTickAgeMillis;
  const lastTickAt = overview.health.freshness.lastSchedulerTickAt;
  const maintenance = overview.instance.maintenance;
  const telemetryRetention = overview.instance.telemetryRetention;

  return (
    <section
      aria-label={standalone ? "Local instance status and counts" : "Daemon connection and instance counts"}
      className="instance-strip"
    >
      <div className="instance-status">
        <span
          aria-hidden="true"
          className={healthy && overview.health.ready ? "live-mark" : "live-mark pending"}
        />
        <strong>
          {standalone
            ? overview.health.ready
              ? "Local instance loaded"
              : "Local instance not ready"
            : !healthy
              ? "Daemon unhealthy"
              : overview.health.ready
                ? "Daemon ready"
                : "Daemon starting"}
        </strong>
        {configurationWarningCount > 0 && (
          <button
            className="instance-warning-link"
            onClick={() =>
              document.getElementById("instance-configuration-warnings")?.scrollIntoView({
                behavior: "smooth",
                block: "start",
              })
            }
            type="button"
          >
            {configurationWarningCount} configuration{" "}
            {configurationWarningCount === 1 ? "warning" : "warnings"}
          </button>
        )}
        {!standalone && tickAge !== null && lastTickAt !== null ? (
          <span>
            last scheduler tick {formatDuration(tickAge)} ago at{" "}
            <time dateTime={lastTickAt}>{formatTimestamp(lastTickAt)}</time>
          </span>
        ) : (
          <span>
            observed{" "}
            <time dateTime={overview.health.freshness.observedAt}>
              {formatTimestamp(overview.health.freshness.observedAt)}
            </time>
          </span>
        )}
      </div>
      {maintenance && renderMaintenanceStatus(maintenance)}
      {telemetryRetention && (
        <div className="maintenance-indicator" role="status">
          <strong>Telemetry retention {telemetryRetention.enabled ? "enabled" : "disabled"}</strong>
          <span>{telemetryRetention.window} window, maximum {telemetryRetention.maxRuns} runs</span>
          <span>first enable: {telemetryRetention.firstEnable}</span>
          {telemetryRetention.lastPassAt ? (
            <span>
              last pass {telemetryRetention.lastPassMode} at {formatTimestamp(telemetryRetention.lastPassAt)}, {telemetryRetention.candidateCount} candidates
            </span>
          ) : (
            <span>no retention pass recorded</span>
          )}
          {telemetryRetention.enabled && telemetryRetention.enforceAt && (
            <span>enforcement begins {formatTimestamp(telemetryRetention.enforceAt)}</span>
          )}
        </div>
      )}
      <dl>
        <div>
          <dt>Gaggles</dt>
          <dd>{overview.instance.counts.gaggles}</dd>
        </div>
        <div>
          <dt>Active runs</dt>
          <dd>{overview.instance.counts.activeRuns}</dd>
        </div>
      </dl>
    </section>
  );
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
      <div className="section-heading">
        <h2>{title}</h2>
        <span className="section-count">{runs.length}</span>
      </div>
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
                <span className="row-title" title={runLabel(run)}>{runLabel(run)}</span>
                <span className="row-subtitle" title={runContextSubtitle(overview, run, active)}>
                  {active && run.operator
                    ? operatorSubtitle(run)
                    : runContextSubtitle(overview, run, active)}
                </span>
                {active && operatorContext(run) ? (
                  <span className="row-subtitle">{operatorContext(run)}</span>
                ) : null}
              </span>
              {active ? (
                <>
                  <span className="row-workflow">
                    <span>{run.gaggle} / {workflowDisplayName(overview, run)}</span>
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
                    <span>{run.gaggle} / {workflowDisplayName(overview, run)}</span>
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

function runContextSubtitle(
  overview: OperationalOverview,
  run: RunSummary,
  active: boolean,
): string {
  if (active && run.operator) {
    return operatorSubtitle(run);
  }
  const context = workflowDisplayName(overview, run);
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
  latest: RunSummary;
  runs: RunSummary[];
}

function groupAttentionRuns(
  runs: RunSummary[],
  overview: OperationalOverview,
  failureReasons: FailureReasons,
): AttentionGroup[] {
  const grouped = new Map<string, AttentionGroup>();
  for (const run of runs) {
    const issue = run.operator?.issue;
    const category = attentionCategory(run, failureReasons);
    const key = issue
      ? `issue:${issue.number}`
      : `workflow:${run.gaggle}/${run.workflow}/${category}`;
    const existing = grouped.get(key);
    if (existing) {
      existing.runs.push(run);
      continue;
    }
    grouped.set(key, {
      key,
      domId: key.replace(/[^a-zA-Z0-9_-]/g, "-"),
      label: issue
        ? `#${issue.number}${issue.title ? ` ${issue.title}` : ""}`
        : `${workflowDisplayName(overview, run)} · ${attentionCategoryLabel(run, failureReasons)}`,
      context: issue
        ? workflowDisplayName(overview, run)
        : `${run.gaggle} / ${run.workflow}`,
      diagnosis: attentionDiagnosis(run, failureReasons),
      latest: run,
      runs: [run],
    });
  }
  return [...grouped.values()];
}

function attentionCategory(run: RunSummary, failureReasons: FailureReasons): string {
  if (run.phase === "escalated") {
    return `escalated:${run.terminalReason ?? "review"}`;
  }
  const reason = failureReasons.get(run.id);
  return `failed:${reason?.code ?? run.operator?.latestError?.code ?? run.terminalReason ?? "unknown"}`;
}

function attentionCategoryLabel(run: RunSummary, failureReasons: FailureReasons): string {
  if (run.phase === "escalated") {
    return "Escalated for review";
  }
  const reason = failureReasons.get(run.id);
  return reason?.code ?? run.operator?.latestError?.code ?? "Failed";
}

function attentionDiagnosis(run: RunSummary, failureReasons: FailureReasons): string {
  if (run.phase === "escalated") {
    return run.terminalReason ?? "Escalated and needs human review.";
  }
  const reason = failureReasons.get(run.id);
  if (reason) {
    return `${reason.code || "failed"}${reason.message ? ` · ${reason.message}` : ""}`;
  }
  return run.terminalReason ?? "Failed and needs investigation.";
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
    details.push(`Error ${operator.latestError.code}${operator.latestError.message ? `: ${operator.latestError.message}` : ""}`);
  }
  if (operator.review) {
    details.push(`Review ${operator.review.verdict}${operator.review.rationale ? `: ${operator.review.rationale}` : ""}`);
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

function formatTimestamp(value: string): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}
