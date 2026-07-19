import type { DaemonClient, RunSummary, WorkflowSummary } from "../api/types";
import type { ConfigurationWarningsProps } from "../components/ConfigurationWarnings";
import { ConfigurationWarnings } from "../components/ConfigurationWarnings";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import {
  groupOperationalRuns,
  type OperationalSnapshot,
  useOperationalSnapshot,
} from "../operationalData";
import { routeHash } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
import { Icon } from "../ui/Icon";
import { StatusBadge } from "../ui/StatusBadge";

export function OverviewPage({
  client,
  configurationWarnings,
  standalone,
}: {
  client: DaemonClient;
  configurationWarnings: Omit<ConfigurationWarningsProps, "context">;
  standalone: boolean;
}) {
  const query = useOperationalSnapshot(client);

  if (query.state.status === "loading") {
    return <DaemonLoadingState standalone={standalone} />;
  }
  if (query.state.status === "error") {
    return <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />;
  }
  if (query.state.status !== "ready") {
    return null;
  }

  return (
    <Overview
      configurationWarnings={configurationWarnings}
      snapshot={query.state.data}
      standalone={standalone}
    />
  );
}

function Overview({
  configurationWarnings,
  snapshot,
  standalone,
}: {
  configurationWarnings: Omit<ConfigurationWarningsProps, "context">;
  snapshot: OperationalSnapshot;
  standalone: boolean;
}) {
  const groups = groupOperationalRuns(snapshot.runs);
  const emptyInstance = snapshot.inventories.length === 0;

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">{snapshot.instance.name}</p>
        <h1>
          {emptyInstance
            ? standalone
              ? snapshot.health.ready
                ? "Instance is ready."
                : "Instance data is loading."
              : snapshot.health.ready
                ? "Daemon is ready."
                : "Daemon is starting."
            : attentionHeading(groups.attention.length)}
        </h1>
        <p>
          {emptyInstance
            ? "No gaggles are configured. Add gaggle definitions to begin observing workflows and runs."
            : standalone
              ? "Operational state read directly from this instance, ordered by what needs attention now."
              : "Live operational state from the daemon, ordered by what needs attention now."}
        </p>
      </header>

      {groups.attention.length > 0 && (
        <section className="content-section attention-section">
          <div className="section-heading">
            <div>
              <p className="section-kicker section-kicker-danger">Attention</p>
              <h2>Needs attention</h2>
            </div>
            <span className="section-count">
              {groups.attention.length} {groups.attention.length === 1 ? "run" : "runs"}
            </span>
          </div>
          <div className="attention-list">
            {groups.attention.map((run) => (
              <a
                aria-label={`Open run ${run.id}`}
                className="attention-row"
                href={routeHash({ page: "run", id: run.id })}
                key={run.id}
              >
                <span className="attention-icon">
                  <Icon name="alert" />
                </span>
                <span className="attention-copy">
                  <strong>{runLabel(run)}</strong>
                  <span>
                    {run.phase === "escalated"
                      ? "Run escalated and needs human review."
                      : "Run failed and needs investigation."}
                  </span>
                </span>
                <span className="attention-meta">
                  <span>{workflowLabel(snapshot, run)}</span>
                  <time dateTime={run.finishedAt ?? run.startedAt}>
                    {formatTimestamp(run.finishedAt ?? run.startedAt)}
                  </time>
                </span>
                <Icon name="arrow" />
              </a>
            ))}
          </div>
        </section>
      )}

      <InstanceStrip snapshot={snapshot} standalone={standalone} />

      {emptyInstance ? (
        <section className="empty-state">
          <img alt="" src="/goober-mascot.png" />
          <div>
            <h2>No gaggles configured</h2>
            <p>
              {snapshot.health.ready
                ? standalone
                  ? "The instance is ready for provisioned gaggle, goober, and workflow definitions."
                  : "The daemon is ready and waiting for provisioned gaggle, goober, and workflow definitions."
                : standalone
                  ? "The local read service has not reported ready yet, and no gaggle definitions are loaded."
                  : "The daemon has not reported ready yet, and no gaggle definitions are loaded."}
            </p>
          </div>
        </section>
      ) : (
        <>
          <RunSection
            ariaLabel="Active runs"
            kicker="Live"
            runs={groups.active}
            snapshot={snapshot}
            title="Active runs"
          />
          <RunSection
            ariaLabel="Recent outcomes"
            kicker="History"
            runs={groups.recent}
            snapshot={snapshot}
            title="Recent outcomes"
          />
        </>
      )}

      <ConfigurationWarnings context="instance" {...configurationWarnings} />
    </>
  );
}

function InstanceStrip({
  snapshot,
  standalone,
}: {
  snapshot: OperationalSnapshot;
  standalone: boolean;
}) {
  return (
    <section
      aria-label={standalone ? "Local instance status and counts" : "Daemon connection and instance counts"}
      className="instance-strip"
    >
      <div>
        <span aria-hidden="true" className={snapshot.health.ready ? "live-mark" : "live-mark pending"} />
        <strong>
          {standalone
            ? snapshot.health.ready
              ? "Local instance loaded"
              : "Local instance not ready"
            : snapshot.health.ready
              ? "Daemon connected"
              : "Daemon not ready"}
        </strong>
        <span>
          observed{" "}
          <time dateTime={snapshot.health.freshness.observedAt}>
            {formatTimestamp(snapshot.health.freshness.observedAt)}
          </time>
        </span>
      </div>
      <dl>
        <div>
          <dt>Workflows</dt>
          <dd>{snapshot.instance.counts.workflows}</dd>
        </div>
        <div>
          <dt>Active runs</dt>
          <dd>{snapshot.instance.counts.activeRuns}</dd>
        </div>
        <div>
          <dt>Gaggles</dt>
          <dd>{snapshot.instance.counts.gaggles}</dd>
        </div>
      </dl>
    </section>
  );
}

function RunSection({
  ariaLabel,
  kicker,
  runs,
  snapshot,
  title,
}: {
  ariaLabel: string;
  kicker: string;
  runs: RunSummary[];
  snapshot: OperationalSnapshot;
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
              key={run.id}
              label={`Open run ${run.id}`}
            >
              <span className="row-primary">
                <span className="row-title">{runLabel(run)}</span>
                <span className="row-subtitle">
                  {run.trigger.ref ? `Trigger ${run.trigger.ref} · ` : ""}
                  {run.id}
                </span>
              </span>
              {active ? (
                <>
                  <span>{workflowLabel(snapshot, run)}</span>
                  <span className="stage-progress">
                    <span aria-hidden="true" className="stage-progress-mark" />
                    {run.currentStage ?? "Awaiting stage"}
                  </span>
                </>
              ) : (
                <>
                  <StatusBadge status={run.phase} />
                  <span>{workflowLabel(snapshot, run)}</span>
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

function workflowLabel(snapshot: OperationalSnapshot, run: RunSummary): string {
  let workflow: WorkflowSummary | undefined;
  for (const inventory of snapshot.inventories) {
    workflow = inventory.workflows.find(
      (candidate) =>
        candidate.identity.gaggle === run.gaggle && candidate.identity.name === run.workflow,
    );
    if (workflow) {
      break;
    }
  }
  return workflow?.displayName ?? `${run.gaggle} / ${run.workflow}`;
}

function runLabel(run: RunSummary): string {
  return `${run.workflow} · ${run.id}`;
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
