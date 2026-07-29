import type {
  DaemonClient,
  RepositoryConnection,
  RunSummary,
  WorkflowSummary,
} from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import {
  latestWorkflowOutcome,
  type GaggleInventory,
  useOperationalSnapshot,
} from "../operationalData";
import type { Navigate } from "../routing";
import { routeHash } from "../routing";
import { GraphFrame } from "../ui/GraphFrame";
import { Icon } from "../ui/Icon";
import { StatusBadge } from "../ui/StatusBadge";
import { formatTriggers } from "./WorkflowsPage";

export function GagglePage({
  client,
  gaggleName,
  navigate,
  standalone,
}: {
  client: DaemonClient;
  gaggleName: string;
  navigate: Navigate;
  standalone: boolean;
}) {
  const query = useOperationalSnapshot(client, { gaggle: gaggleName });

  if (query.state.status === "loading") {
    return <DaemonLoadingState standalone={standalone} />;
  }
  if (query.state.status === "error") {
    return <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />;
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  const inventory = query.state.data.inventories.find(
    ({ gaggle }) => gaggle.name === gaggleName,
  );
  if (!inventory) {
    return (
      <section className="daemon-state daemon-state-error" role="alert">
        <div>
          <h1>Gaggle unavailable</h1>
          <p>No gaggle named "{gaggleName}" is configured in this instance.</p>
        </div>
        <button
          className="reconnect-button"
          onClick={() => navigate({ page: "workflows" })}
          type="button"
        >
          View workflows
        </button>
      </section>
    );
  }

  return (
    <GaggleTopology
      inventory={inventory}
      navigate={navigate}
      runs={query.state.data.runs}
    />
  );
}

function GaggleTopology({
  inventory,
  navigate,
  runs,
}: {
  inventory: GaggleInventory;
  navigate: Navigate;
  runs: RunSummary[];
}) {
  const { gaggle } = inventory;

  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumbs">
        <button onClick={() => navigate({ page: "workflows" })} type="button">
          Workflows
        </button>
        <Icon name="chevron" size={14} />
        <span>{gaggle.displayName}</span>
      </nav>
      <header className="detail-heading">
        <div>
          <span className="definition-label">Gaggle</span>
          <h1>{gaggle.displayName}</h1>
          <p>
            {gaggle.name} · {gaggle.project.owner}/{gaggle.project.name}
          </p>
        </div>
        <dl className="detail-meta">
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
      </header>

      <GraphFrame
        action={
          <span className="graph-legend">
            {inventory.workflows.length}{" "}
            {inventory.workflows.length === 1 ? "workflow" : "workflows"} ·{" "}
            {inventory.connections.length}{" "}
            {inventory.connections.length === 1 ? "repository" : "repositories"}
          </span>
        }
        className="gaggle-topology-panel"
        eyebrow="Definitions"
        title="Workflow topology"
      >
        <div className="gaggle-workflow-topology">
          <div className="gaggle-workflow-column">
            {inventory.workflows.length === 0 ? (
              <p className="inline-empty">No workflows are provisioned for this gaggle.</p>
            ) : (
              <ul aria-label={`${gaggle.displayName} workflows`} className="gaggle-workflow-list">
                {inventory.workflows.map((workflow) => (
                  <WorkflowNode
                    gaggleDisplayName={gaggle.displayName}
                    key={workflow.identity.name}
                    latestOutcome={latestWorkflowOutcome(
                      runs,
                      workflow.identity.gaggle,
                      workflow.identity.name,
                    )}
                    workflow={workflow}
                  />
                ))}
              </ul>
            )}
          </div>
          <ConnectionTopology
            connections={inventory.connections}
            gaggleDisplayName={gaggle.displayName}
            hasWorkflows={inventory.workflows.length > 0}
          />
        </div>
      </GraphFrame>
    </>
  );
}

function ConnectionTopology({
  connections,
  gaggleDisplayName,
  hasWorkflows,
}: {
  connections: RepositoryConnection[];
  gaggleDisplayName: string;
  hasWorkflows: boolean;
}) {
  return (
    <section
      aria-label={`${gaggleDisplayName} repository connections`}
      className={`gaggle-connection-topology${hasWorkflows ? "" : " without-workflows"}`}
    >
      <h3>External connections</h3>
      <ul>
        {connections.map((connection) => {
          const identity = repositoryIdentity(connection);
          const access = formatAccessMode(connection);
          return (
            <li key={`${identity}/${connection.accessMode}`}>
              {hasWorkflows ? (
                <span aria-hidden="true" className="gaggle-connection-edge">
                  <span>{access}</span>
                </span>
              ) : null}
              <article className={`gaggle-repository-node ${connection.accessMode}`}>
                <span className="gaggle-workflow-kind">
                  <Icon name="code" size={13} />
                  {connection.accessMode === "read-write"
                    ? "Target repository"
                    : "Reference repository"}
                </span>
                <strong>{identity}</strong>
                <p>{connection.repository.provider === "ado" ? "Azure DevOps" : "GitHub"}</p>
                <span className="gaggle-repository-access">{access} access</span>
                {hasWorkflows ? (
                  <span className="sr-only">
                    Connected from the configured workflows with {access.toLowerCase()} access.
                  </span>
                ) : null}
              </article>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function repositoryIdentity(connection: RepositoryConnection): string {
  const { owner, project, name } = connection.repository;
  return [owner, project, name].filter(Boolean).join("/");
}

function formatAccessMode(connection: RepositoryConnection): string {
  return connection.accessMode === "read-write" ? "Read / write" : "Read only";
}

function WorkflowNode({
  gaggleDisplayName,
  latestOutcome,
  workflow,
}: {
  gaggleDisplayName: string;
  latestOutcome?: RunSummary;
  workflow: WorkflowSummary;
}) {
  return (
    <li className="gaggle-workflow-node">
      <a
        aria-label={`Open workflow ${workflow.displayName} for gaggle ${gaggleDisplayName}`}
        href={routeHash({
          page: "workflow",
          gaggle: workflow.identity.gaggle,
          id: workflow.identity.name,
        })}
      >
        <span className="gaggle-workflow-kind">
          <Icon name="workflow" size={13} />
          Workflow
        </span>
        <strong>{workflow.displayName}</strong>
        <p>{workflow.purpose}</p>
        <dl>
          <div>
            <dt>Trigger</dt>
            <dd>{formatTriggers(workflow)}</dd>
          </div>
          <div>
            <dt>Stages</dt>
            <dd>{workflow.stageCount}</dd>
          </div>
          <div>
            <dt>Concurrency</dt>
            <dd>
              {workflow.concurrency.activeRuns} active /{" "}
              {workflow.concurrency.maxConcurrentRuns} max
            </dd>
          </div>
          <div>
            <dt>Last outcome</dt>
            <dd>
              {latestOutcome ? (
                <StatusBadge status={latestOutcome.phase} />
              ) : (
                <span className="gaggle-workflow-no-runs">No recorded runs</span>
              )}
            </dd>
          </div>
        </dl>
      </a>
    </li>
  );
}
