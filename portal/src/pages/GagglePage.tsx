import type { DaemonClient, RunSummary, WorkflowSummary } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import {
  type GaggleInventory,
  useOperationalSnapshot,
  workflowKey,
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
      latestOutcomes={query.state.data.latestOutcomes}
      navigate={navigate}
    />
  );
}

function GaggleTopology({
  inventory,
  latestOutcomes,
  navigate,
}: {
  inventory: GaggleInventory;
  latestOutcomes: Map<string, RunSummary>;
  navigate: Navigate;
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
            {inventory.workflows.length === 1 ? "workflow" : "workflows"}
          </span>
        }
        className="gaggle-topology-panel"
        eyebrow="Definitions"
        title="Workflow topology"
      >
        {inventory.workflows.length === 0 ? (
          <p className="inline-empty">No workflows are provisioned for this gaggle.</p>
        ) : (
          <ul
            aria-label={`${gaggle.displayName} workflows`}
            className="gaggle-workflow-topology"
          >
            {inventory.workflows.map((workflow) => (
              <WorkflowNode
                gaggleDisplayName={gaggle.displayName}
                key={workflow.identity.name}
                latestOutcome={latestOutcomes.get(
                  workflowKey(workflow.identity.gaggle, workflow.identity.name),
                )}
                workflow={workflow}
              />
            ))}
          </ul>
        )}
      </GraphFrame>
    </>
  );
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
