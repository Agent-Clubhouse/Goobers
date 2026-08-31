import { useState } from "react";
import type { DaemonClient, RunSummary } from "../api/types";
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
      standalone={standalone}
    />
  );
}

function Overview({
  configurationWarnings,
  failureReasons,
  overview,
  standalone,
}: {
  configurationWarnings: Omit<ConfigurationWarningsProps, "context">;
  failureReasons: FailureReasons;
  overview: OperationalOverview;
  standalone: boolean;
}) {
  const groups = overview.groups;
  const emptyInstance = overview.gaggleCount === 0;
  const emptyWorkflows = !emptyInstance && overview.instance.counts.workflows === 0;
  const emptyRuns =
    !emptyInstance &&
    !emptyWorkflows &&
    !overview.sectionErrors?.runs &&
    !groups.incomplete &&
    groups.active.length === 0 &&
    groups.attention.length === 0 &&
    groups.recent.length === 0;
  const healthy = standalone || overview.health.healthy;

  const { dismissedRunIds, dismiss, restore } = useAttentionDismissals();
  const [attentionCollapsed, setAttentionCollapsed] = useAttentionCollapsed();
  const [selectedRunIds, setSelectedRunIds] = useState<ReadonlySet<string>>(() => new Set());
  const [showDismissed, setShowDismissed] = useState(false);
  const activeAttention = groups.attention.filter((run) => !dismissedRunIds.has(run.id));
  const dismissedAttention = groups.attention.filter((run) => dismissedRunIds.has(run.id));
  const activeAttentionIds = new Set(activeAttention.map((run) => run.id));
  const visibleSelectedRunIds = [...selectedRunIds].filter((runId) =>
    activeAttentionIds.has(runId),
  );

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

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">{overview.instance.name}</p>
        <h1>
          {emptyInstance
            ? standalone
              ? overview.health.ready
                ? "Instance is ready."
                : "Instance data is loading."
            : !healthy
              ? "Daemon is unhealthy."
              : overview.health.ready
                ? "Daemon is ready."
                : "Daemon is starting."
            : attentionHeading(activeAttention.length)}
        </h1>
        <p>
          {emptyInstance
            ? "No gaggles are configured. Add gaggle definitions to begin observing workflows and runs."
            : standalone
              ? "Operational state read directly from this instance, ordered by what needs attention now."
              : "Live operational state from the daemon, ordered by what needs attention now."}
        </p>
      </header>

      {/* A section that failed to load must say so. Without this the page would
          render an empty run list identically to a genuinely idle instance,
          which is a worse failure than the blank page it replaced (#1709). */}
      {overview.sectionErrors && (
        <p className="inline-empty" role="alert">
          {overview.sectionErrors.runs
            ? "Run activity could not be read just now, so the run groups below may be incomplete or out of date. Everything else on this page is current."
            : "The gaggle and workflow inventory could not be read just now, so names and counts may be out of date. Everything else on this page is current."}
        </p>
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

      {groups.attention.length > 0 && (
        <section className="content-section attention-section">
          <div className="section-heading">
            <div>
              <p className="section-kicker section-kicker-danger">Attention</p>
              <h2>Needs attention</h2>
            </div>
            <div className="attention-actions">
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
                {activeAttention.map((run) => {
                  const reason = run.phase === "failed" ? failureReasons.get(run.id) : undefined;
                  const selected = selectedRunIds.has(run.id);
                  return (
                    <div className="attention-row" key={run.id}>
                      <input
                        aria-label={`Select run ${run.id} for bulk actions`}
                        checked={selected}
                        className="attention-select"
                        onChange={() => toggleSelected(run.id)}
                        type="checkbox"
                      />
                      <div className="attention-link-shell data-row-stretched">
                        <a
                          aria-label={`Open run ${run.id}`}
                          className="data-row-stretch-link"
                          href={routeHash({ page: "run", id: run.id })}
                        />
                        <span className="attention-icon">
                          <Icon name="alert" />
                        </span>
                        <span className="attention-copy">
                          <strong>{runLabel(run)}</strong>
                          <span>
                            {run.phase === "escalated"
                              ? "Run escalated and needs human review."
                              : reason
                                ? `${reason.code || "failed"} · ${reason.message}`
                                : "Run failed and needs investigation."}
                          </span>
                        </span>
                        <span className="attention-meta">
                          <span className="attention-workflow">
                            {workflowDisplayName(overview, run)}
                            <ScopePivot
                              label={workflowDisplayName(overview, run)}
                              scope={{ gaggle: run.gaggle, workflow: run.workflow }}
                            />
                          </span>
                          <time dateTime={run.finishedAt ?? run.startedAt}>
                            {formatTimestamp(run.finishedAt ?? run.startedAt)}
                          </time>
                        </span>
                        <Icon name="arrow" />
                      </div>
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

      <InstanceStrip overview={overview} standalone={standalone} />

      {emptyInstance ? (
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
            kicker="Live"
            overview={overview}
            runs={groups.active}
            title="Active runs"
          />
          <RunSection
            ariaLabel="Recent outcomes"
            kicker="History"
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

function InstanceStrip({
  overview,
  standalone,
}: {
  overview: OperationalOverview;
  standalone: boolean;
}) {
  const healthy = standalone || overview.health.healthy;
  const tickAge = overview.health.freshness.lastTickAgeMillis;
  const lastTickAt = overview.health.freshness.lastSchedulerTickAt;
  return (
    <section
      aria-label={standalone ? "Local instance status and counts" : "Daemon connection and instance counts"}
      className="instance-strip"
    >
      <div>
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
      <dl>
        <div>
          <dt>Workflows</dt>
          <dd>{overview.instance.counts.workflows}</dd>
        </div>
        <div>
          <dt>Active runs</dt>
          <dd>{overview.instance.counts.activeRuns}</dd>
        </div>
        <div>
          <dt>Gaggles</dt>
          <dd>{overview.instance.counts.gaggles}</dd>
        </div>
      </dl>
    </section>
  );
}

function RunSection({
  ariaLabel,
  kicker,
  overview,
  runs,
  title,
}: {
  ariaLabel: string;
  kicker: string;
  overview: OperationalOverview;
  runs: RunSummary[];
  title: string;
}) {
  const active = title === "Active runs";
  return (
    <section className="content-section">
      <div className="section-heading">
        <div>
          <p className="section-kicker">{kicker}</p>
          <h2>{title}</h2>
        </div>
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
                <span className="row-title">{runLabel(run)}</span>
                <span className="row-subtitle">
                  {active && run.operator
                    ? operatorSubtitle(run)
                    : `${run.trigger.ref ? `Trigger ${run.trigger.ref} · ` : ""}${run.id}`}
                </span>
                {active && operatorContext(run) ? (
                  <span className="row-subtitle">{operatorContext(run)}</span>
                ) : null}
              </span>
              {active ? (
                <>
                  <span className="row-workflow">
                    {workflowDisplayName(overview, run)}
                    <ScopePivot
                      label={workflowDisplayName(overview, run)}
                      scope={{ gaggle: run.gaggle, workflow: run.workflow }}
                    />
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
                    {workflowDisplayName(overview, run)}
                    <ScopePivot
                      label={workflowDisplayName(overview, run)}
                      scope={{ gaggle: run.gaggle, workflow: run.workflow }}
                    />
                  </span>
                </>
              )}
              <span className="mono">{formatDuration(run.durationMillis)}</span>
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
  return `${run.workflow} · ${run.id}`;
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
