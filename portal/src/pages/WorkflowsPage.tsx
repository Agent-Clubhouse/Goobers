import type { DaemonClient, Goober, RunSummary, WorkflowSummary } from "../api/types";
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
import { Icon } from "../ui/Icon";
import { DataList, DataRow } from "../ui/DataList";
import { StatusBadge } from "../ui/StatusBadge";

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
            <RecoveryCommand command="goobers init --guided <instance>" />
          </div>
        </section>
      ) : (
        snapshot.inventories.map((inventory) => (
          <GaggleSection
            inventory={inventory}
            key={inventory.gaggle.name}
            runs={snapshot.runs}
          />
        ))
      )}
    </>
  );
}

function GaggleSection({
  inventory,
  runs,
}: {
  inventory: GaggleInventory;
  runs: RunSummary[];
}) {
  const { gaggle } = inventory;
  const headingId = `gaggle-${gaggle.name}`;

  return (
    <section aria-labelledby={headingId} className="gaggle-section">
      <div className="gaggle-heading">
        <div>
          <p className="section-kicker">Gaggle</p>
          <div className="gaggle-heading-line">
            <h2 id={headingId}>
              <a
                className="gaggle-detail-link"
                href={routeHash({ page: "gaggle", id: gaggle.name })}
              >
                {gaggle.displayName}
                <Icon name="arrow" size={16} />
              </a>
            </h2>
            <ScopePivot label={gaggle.displayName} scope={{ gaggle: gaggle.name }} />
          </div>
          <p>
            {gaggle.name} · {gaggle.project.owner}/{gaggle.project.name}
          </p>
        </div>
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

      <div className="content-section gaggle-content">
        <div className="section-heading">
          <h3>Workflow inventory</h3>
          <span className="section-count">{inventory.workflows.length}</span>
        </div>
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
                        scope={{ gaggle: workflow.identity.gaggle, workflow: workflow.identity.name }}
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
                  </span>
                </DataRow>
              );
            })}
          </DataList>
        )}
      </div>

      <div className="content-section gaggle-content">
        <div className="section-heading">
          <h3>Provisioned goobers</h3>
          <span className="section-count">{inventory.goobers.length}</span>
        </div>
        {inventory.goobers.length === 0 ? (
          <p className="inline-empty">No goobers are provisioned for this gaggle.</p>
        ) : (
          <div aria-label={`${gaggle.displayName} provisioned goobers`} className="goober-roster">
            {inventory.goobers.map((goober) => (
              <GooberCard goober={goober} key={goober.name} />
            ))}
          </div>
        )}
      </div>
    </section>
  );
}

function GooberCard({ goober }: { goober: Goober }) {
  return (
    <article className="goober-card">
      <header>
        <div>
          <h4>{goober.displayName}</h4>
          <p>{goober.role}</p>
        </div>
        <span className="definition-status">{goober.status}</span>
      </header>
      <dl>
        <DefinitionList label="Skills" values={goober.skills} />
        <DefinitionList label="Capabilities" values={goober.capabilities} />
        <DefinitionList
          label="Workflow ownership"
          values={goober.workflows.map(
            (workflow) => `${workflow.gaggle} / ${workflow.name}`,
          )}
        />
        <DefinitionList
          label="Stage ownership"
          values={goober.stages.map(
            (stage) =>
              `${stage.workflow.gaggle} / ${stage.workflow.name} / ${stage.stage} (${stage.kind})`,
          )}
        />
      </dl>
    </article>
  );
}

function DefinitionList({ label, values }: { label: string; values: string[] }) {
  return (
    <div>
      <dt>{label}</dt>
      <dd>{values.length > 0 ? values.join(", ") : "None declared"}</dd>
    </div>
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
