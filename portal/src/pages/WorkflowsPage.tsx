import { useState } from "react";
import type { DaemonClient, RunSummary, WorkflowSummary } from "../api/types";
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
import { DataList, DataRow } from "../ui/DataList";
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

  return <WorkflowInventory snapshot={query.state.data} standalone={standalone} />;
}

function WorkflowInventory({
  snapshot,
  standalone,
}: {
  snapshot: OperationalSnapshot;
  standalone: boolean;
}) {
  return (
    <>
      <header className="page-heading page-heading-row">
        <div>
          <p className="page-kicker">Definitions</p>
          <h1>Workflows</h1>
          <p>
            {standalone
              ? "Versioned processes and their provisioned workforce, read from this instance."
              : "Versioned processes and their provisioned workforce, read from the daemon."}
          </p>
        </div>
        <div className="scope-chip">
          <span className="scope-mark">G</span>
          {snapshot.inventories.length}{" "}
          {snapshot.inventories.length === 1 ? "gaggle" : "gaggles"}
        </div>
      </header>

      {snapshot.inventories.length === 0 ? (
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
  const [expanded, setExpanded] = useState(inventory.workflows.length <= 3);

  return (
    <section aria-labelledby={headingId} className="gaggle-section">
      <div className="gaggle-heading gaggle-inventory-heading">
        <button
          aria-controls={contentId}
          aria-expanded={expanded}
          className="gaggle-inventory-toggle"
          onClick={() => setExpanded((current) => !current)}
          type="button"
        >
          <div>
            <p className="section-kicker">Gaggle</p>
            <div className="gaggle-heading-line">
              <h2 id={headingId}>{gaggle.displayName}</h2>
              <span aria-hidden="true" className="gaggle-inventory-chevron">
                <Icon name="chevron" size={16} />
              </span>
            </div>
            <p>
              {gaggle.name} · {gaggle.project.owner}/{gaggle.project.name}
            </p>
          </div>
        </button>
        <dl>
          <div>
            <dt>Status</dt>
            <dd>{gaggle.status}</dd>
          </div>
          <div>
            <dt>Workflows</dt>
            <dd>{gaggle.workflowCount}</dd>
          </div>
          <div>
            <dt>Goobers</dt>
            <dd>{gaggle.gooberCount}</dd>
          </div>
          <div>
            <dt>Active runs</dt>
            <dd>{gaggle.activeRunCount}</dd>
          </div>
        </dl>
      </div>

      <div className="gaggle-inventory-actions">
        <a className="gaggle-detail-link" href={routeHash({ page: "gaggle", id: gaggle.name })}>
          Open gaggle details
          <Icon name="arrow" size={16} />
        </a>
        <ScopePivot label={gaggle.displayName} scope={{ gaggle: gaggle.name }} />
      </div>

      {expanded && (
        <div id={contentId}>
          <div className="content-section gaggle-content">
            <div className="section-heading">
              <h3>Workflow inventory</h3>
              <span className="section-count">{inventory.workflows.length}</span>
            </div>
            <p className="inline-empty">
              Copying a manual-run command prepares it for your terminal; it does not start a
              workflow.
            </p>
            {inventory.workflows.length === 0 ? (
              <div className="inline-empty inline-empty-recovery">
                <strong>No workflows are configured for this gaggle.</strong>
                <span>Add a workflow definition, then validate the instance.</span>
                <RecoveryCommand command="goobers validate <instance>" />
              </div>
            ) : (
              <DataList
                ariaLabel={`${gaggle.displayName} workflow definitions`}
                columns={["Workflow", "Trigger", "Concurrency", "Last outcome"]}
                gridClassName="workflow-grid"
              >
                {inventory.workflows.map((workflow) => {
                  const outcome = latestWorkflowOutcome(
                    runs,
                    workflow.identity.gaggle,
                    workflow.identity.name,
                  );
                  return (
                    <DataRow
                      href={routeHash({
                        page: "workflow",
                        gaggle: workflow.identity.gaggle,
                        id: workflow.identity.name,
                      })}
                      interactiveChildren
                      key={`${workflow.identity.gaggle}/${workflow.identity.name}`}
                      label={`Open workflow ${workflow.displayName} for gaggle ${gaggle.displayName}`}
                    >
                      <span className="row-primary">
                        <span className="row-title row-title-with-pivot">
                          <span className="row-title-text">{workflow.displayName}</span>
                          <ScopePivot
                            label={`${gaggle.displayName} / ${workflow.displayName}`}
                            scope={{
                              gaggle: workflow.identity.gaggle,
                              workflow: workflow.identity.name,
                            }}
                          />
                        </span>
                        <span className="row-subtitle">{workflow.purpose}</span>
                      </span>
                      <span>{formatTriggers(workflow)}</span>
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
                        <CopyCommand
                          command={manualRunCommand(
                            workflow.identity.gaggle,
                            workflow.identity.name,
                            instanceRoot,
                          )}
                          failureLabel="Could not copy the manual run command. Select and copy the command from the workflow details."
                          idleLabel="Copy manual run command"
                          successLabel="Manual run command copied to the clipboard."
                        />
                      </span>
                    </DataRow>
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
  return workflow.triggers
    .map((trigger) => {
      switch (trigger.type) {
        case "backlog-item":
          return "Backlog item";
        case "manual":
          return "Manual";
        case "schedule":
          return trigger.schedule ? `Schedule · ${trigger.schedule}` : "Schedule";
        case "signal":
          return trigger.signal ? `Signal · ${trigger.signal}` : "Signal";
        case "webhook":
          return trigger.events?.length ? `Webhook · ${trigger.events.join("/")}` : "Webhook";
      }
    })
    .join(", ");
}

function formatTimestamp(value: string): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}
