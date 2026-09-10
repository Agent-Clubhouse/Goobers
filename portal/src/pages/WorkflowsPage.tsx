import { useState } from "react";
import type { DaemonClient, RunSummary, WorkflowSummary, WorkflowTrigger } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { RecoveryCommand } from "../components/RecoveryAction";
import { ScopePivot } from "../components/ScopePivot";
import {
  latestWorkflowOutcome,
  type GaggleInventory,
  type OperationalSnapshot,
  useOperationalSnapshot,
} from "../operationalData";
import { routeHash } from "../routing";
import { CopyCommand } from "../ui/CopyCommand";
import { Icon } from "../ui/Icon";
import { DataList } from "../ui/DataList";
import { StatusBadge } from "../ui/StatusBadge";
import { manualRunCommand } from "../manualRunCommand";

export function WorkflowsPage({
  client,
  standalone,
}: {
  client: DaemonClient;
  standalone: boolean;
}) {
  const query = useOperationalSnapshot(client);

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
    <WorkflowInventory
      inventoryError={query.state.status === "stale" ? query.state.error : undefined}
      retry={query.retry}
      snapshot={query.state.data}
      standalone={standalone}
    />
  );
}

function WorkflowInventory({
  inventoryError,
  retry,
  snapshot,
  standalone,
}: {
  inventoryError?: Error;
  retry: () => void;
  snapshot: OperationalSnapshot;
  standalone: boolean;
}) {
  return (
    <>
      <header className="page-heading">
        <div>
          <h1>Workflows</h1>
          <p>Versioned processes and their provisioned workforce.</p>
        </div>
      </header>

      {inventoryError && snapshot.inventories.length === 0 ? (
        <div className="run-stale-state run-stale-state-error" role="alert">
          <span>
            <strong>Workflow inventory is unavailable</strong>
            <small>{inventoryError.message}</small>
          </span>
          <button className="text-button" onClick={retry} type="button">
            Retry
          </button>
        </div>
      ) : snapshot.loadingSections?.inventory && snapshot.inventories.length === 0 ? (
        <div className="inline-empty section-loading" role="status">
          <span aria-hidden="true" className="loading-mark" />
          <span>Loading</span>
        </div>
      ) : snapshot.inventories.length === 0 ? (
        <section className="empty-state">
          <img alt="" src="/goober-mascot.png" />
          <div>
            <h2>No gaggles configured</h2>
            <p>No configuration is available to the Portal yet. Initialize the instance to begin.</p>
            <RecoveryCommand command="goobers init --guided" />
          </div>
        </section>
      ) : (
        snapshot.inventories.map((inventory) => (
          <GaggleSection
            inventory={inventory}
            key={inventory.gaggle.name}
            runs={snapshot.runs}
            instanceRoot={snapshot.instance.instanceRoot}
          />
        ))
      )}
    </>
  );
}

function GaggleSection({
  inventory,
  instanceRoot,
  runs,
}: {
  inventory: GaggleInventory;
  instanceRoot: string;
  runs: RunSummary[];
}) {
  const { gaggle } = inventory;
  const headingId = `gaggle-${gaggle.name}`;
  const contentId = `${headingId}-inventory`;
  const [expanded, setExpanded] = useState(false);

  return (
    <section aria-labelledby={headingId} className="goober-group workflow-gaggle-group">
      <div className="workflow-gaggle-summary">
        <button
          aria-controls={contentId}
          aria-expanded={expanded}
          className="goober-group-summary"
          onClick={() => setExpanded((current) => !current)}
          type="button"
        >
          <span>
            <strong id={headingId}>{gaggle.displayName}</strong>
            <code>
              {gaggle.name} · {gaggle.project.owner}/{gaggle.project.name}
            </code>
          </span>
          <span className="goober-group-summary-meta">
            <span className="section-count">
              {inventory.workflows.length}{" "}
              {inventory.workflows.length === 1 ? "workflow" : "workflows"}
            </span>
            <span aria-hidden="true" className="goober-group-chevron">
              <Icon name="chevron" size={16} />
            </span>
          </span>
        </button>
        <div
          aria-label={`${gaggle.displayName} destinations`}
          className="workflow-gaggle-actions"
          role="group"
        >
          <a className="gaggle-detail-link" href={routeHash({ page: "gaggle", id: gaggle.name })}>
            <Icon name="workflow" size={13} />
            Details
          </a>
          <ScopePivot label={gaggle.displayName} scope={{ gaggle: gaggle.name }} />
        </div>
      </div>

      {expanded && (
        <div className="workflow-gaggle-content" id={contentId}>
          <div className="content-section gaggle-content">
            {inventory.workflows.length === 0 ? (
              <div className="inline-empty inline-empty-recovery">
                <strong>No workflows are configured for this gaggle.</strong>
                <span>Add a workflow definition, then validate the instance.</span>
                <RecoveryCommand command="goobers validate <instance>" />
              </div>
            ) : (
              <DataList
                ariaLabel={`${gaggle.displayName} workflow definitions`}
                columns={["Workflow", "Trigger(s)", "Concurrency", "Last outcome", "Actions"]}
                gridClassName="workflow-grid"
                showTrailingColumn={false}
              >
                {inventory.workflows.map((workflow) => {
                  const outcome = latestWorkflowOutcome(
                    runs,
                    workflow.identity.gaggle,
                    workflow.identity.name,
                  );
                  return (
                    <div
                      className="data-row workflow-row"
                      key={`${workflow.identity.gaggle}/${workflow.identity.name}`}
                    >
                      <span className="row-primary">
                        <span className="row-title">{workflow.displayName}</span>
                        <span className="row-subtitle">{workflow.purpose}</span>
                        <CopyCommand
                          compact
                          command={manualRunCommand(
                            workflow.identity.gaggle,
                            workflow.identity.name,
                            instanceRoot,
                          )}
                          failureLabel="Could not copy the manual run command. Select and copy the command from the workflow details."
                          idleLabel="Copy run command"
                          successLabel="Manual run command copied to the clipboard."
                        />
                      </span>
                      <WorkflowTriggers workflow={workflow} />
                      <span>
                        {workflow.concurrency.activeRuns} active
                        {workflow.concurrency.desiredRuns !== undefined
                          ? ` / ${workflow.concurrency.desiredRuns} desired`
                          : ""}{" "}
                        / {workflow.concurrency.maxConcurrentRuns} max
                        {workflow.concurrency.admissionBlocked && (
                          <small>Blocked: {workflow.concurrency.blockingCondition}</small>
                        )}
                      </span>
                      <span className="outcome-cell">
                        {outcome ? (
                          <>
                            <StatusBadge status={outcome.phase} />
                            <small>
                              <time dateTime={outcome.finishedAt ?? outcome.startedAt}>
                                {formatTimestamp(outcome.finishedAt ?? outcome.startedAt)}
                              </time>
                            </small>
                          </>
                        ) : (
                          <small>No recorded runs</small>
                        )}
                      </span>
                      <span
                        aria-label={`${workflow.displayName} destinations`}
                        className="workflow-row-actions"
                        role="group"
                      >
                        <a
                          className="scope-pivot-link"
                          href={routeHash({
                            page: "workflow",
                            gaggle: workflow.identity.gaggle,
                            id: workflow.identity.name,
                          })}
                        >
                          <Icon name="workflow" size={13} />
                          Details
                        </a>
                        <ScopePivot
                          label={`${gaggle.displayName} / ${workflow.displayName}`}
                          scope={{
                            gaggle: workflow.identity.gaggle,
                            workflow: workflow.identity.name,
                          }}
                        />
                      </span>
                    </div>
                  );
                })}
              </DataList>
            )}
          </div>

          <div className="content-section gaggle-content">
            <div className="section-heading">
              <h3>Goober summary</h3>
              <span className="section-count">{inventory.goobers.length}</span>
            </div>
            {inventory.goobers.length === 0 ? (
              <p className="inline-empty">No goobers are provisioned for this gaggle.</p>
            ) : (
              <div className="gaggle-goober-summary">
                <p>
                  {inventory.goobers.length} configured{" "}
                  {inventory.goobers.length === 1 ? "persona" : "personas"} ·{" "}
                  {inventory.goobers.map((goober) => goober.displayName).join(", ")}
                </p>
                <a href={routeHash({ page: "goobers", gaggle: gaggle.name })}>
                  View {gaggle.displayName} Goobers
                </a>
              </div>
            )}
          </div>
        </div>
      )}
    </section>
  );
}

export function formatTriggers(workflow: WorkflowSummary): string {
  if (workflow.triggers.length === 0) {
    return "Manual";
  }
  return workflow.triggers.map(formatTriggerLabel).join("\n");
}

function WorkflowTriggers({ workflow }: { workflow: WorkflowSummary }) {
  const triggers = workflow.triggers.length > 0 ? workflow.triggers : [{ type: "manual" } as const];
  return (
    <span className="workflow-trigger-list">
      {triggers.map((trigger, index) => (
        <span key={`${trigger.type}-${index}`}>{formatTriggerLabel(trigger)}</span>
      ))}
    </span>
  );
}

function formatTriggerLabel(trigger: WorkflowTrigger): string {
  switch (trigger.type) {
    case "backlog-item":
      return "Backlog item";
    case "manual":
      return "Manual";
    case "schedule":
      return trigger.schedule ? describeCron(trigger.schedule) : "Scheduled";
    case "signal":
      return trigger.signal ? `Signal · ${trigger.signal}` : "Signal";
    case "webhook":
      return trigger.events?.length ? `Webhook · ${trigger.events.join(", ")}` : "Webhook";
  }
}

function describeCron(schedule: string): string {
  const [minute, hour, dayOfMonth, month, dayOfWeek] = schedule.trim().split(/\s+/);
  if (!minute || !hour || !dayOfMonth || !month || !dayOfWeek) {
    return "Scheduled";
  }
  if (/^\d+$/.test(minute) && /^\*\/\d+$/.test(hour) && dayOfMonth === "*" && month === "*" && dayOfWeek === "*") {
    return `At ${Number(minute)} minutes past every ${Number(hour.slice(2))} hours`;
  }
  if (/^\d+$/.test(minute) && /^\d+(,\d+)+$/.test(hour) && dayOfMonth === "*" && month === "*" && dayOfWeek === "*") {
    const times = hour
      .split(",")
      .map((value) => `${value.padStart(2, "0")}:${minute.padStart(2, "0")}`);
    return `Daily at ${formatList(times)} scheduler time`;
  }
  if (/^\d+-\d+\/\d+$/.test(minute) && hour === "*" && dayOfMonth === "*" && month === "*" && dayOfWeek === "*") {
    const [range, interval] = minute.split("/");
    const [start, end] = range.split("-");
    return `Every ${Number(interval)} minutes from minute ${Number(start)} through ${Number(end)}`;
  }
  if (/^\*\/\d+$/.test(minute) && hour === "*" && dayOfMonth === "*" && month === "*" && dayOfWeek === "*") {
    return `Every ${Number(minute.slice(2))} minutes`;
  }
  if (/^\d+$/.test(minute) && hour === "*" && dayOfMonth === "*" && month === "*" && dayOfWeek === "*") {
    return `Hourly at minute ${Number(minute)}`;
  }
  if (/^\d+$/.test(minute) && /^\d+$/.test(hour) && dayOfMonth === "*" && month === "*" && dayOfWeek === "*") {
    return `Daily at ${hour.padStart(2, "0")}:${minute.padStart(2, "0")} scheduler time`;
  }
  return "Scheduled";
}

function formatList(values: string[]): string {
  if (values.length <= 1) return values[0] ?? "";
  if (values.length === 2) return `${values[0]} and ${values[1]}`;
  return `${values.slice(0, -1).join(", ")}, and ${values.at(-1)}`;
}

function formatTimestamp(value: string): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}
