import type {
  DaemonClient,
  Goober,
  RepositoryConnection,
  RunSummary,
  WorkflowSummary,
} from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { ScopePivot } from "../components/ScopePivot";
import {
  latestWorkflowOutcome,
  useGaggleActivity,
  useGaggleList,
  useOperationalSnapshot,
  type GaggleActivity,
  type GaggleInventory,
  type GaggleSummary,
} from "../operationalData";
import type { Navigate } from "../routing";
import { routeHash } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
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
  const gaggleListQuery = useGaggleList(client);
  const activityQuery = useGaggleActivity(client, gaggleName);

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

  const gaggleList =
    gaggleListQuery.state.status === "ready" || gaggleListQuery.state.status === "stale"
      ? gaggleListQuery.state.data
      : undefined;
  const activity =
    activityQuery.state.status === "ready" || activityQuery.state.status === "stale"
      ? activityQuery.state.data
      : undefined;

  return (
    <GaggleTopology
      activity={activity}
      gaggleList={gaggleList}
      inventory={inventory}
      navigate={navigate}
      runs={query.state.data.runs}
    />
  );
}

function GaggleTopology({
  activity,
  gaggleList,
  inventory,
  navigate,
  runs,
}: {
  activity: GaggleActivity | undefined;
  gaggleList: GaggleSummary[] | undefined;
  inventory: GaggleInventory;
  navigate: Navigate;
  runs: RunSummary[];
}) {
  const { gaggle } = inventory;
  const otherGaggles = (gaggleList ?? []).filter((candidate) => candidate.name !== gaggle.name);

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
          <div className="detail-heading-line">
            <h1>{gaggle.displayName}</h1>
            <ScopePivot label={gaggle.displayName} scope={{ gaggle: gaggle.name }} />
          </div>
          <p>
            {gaggle.name} · {gaggle.project.owner}/{gaggle.project.name}
          </p>
        </div>
        {otherGaggles.length > 0 && (
          <label className="gaggle-switcher">
            <span>Switch gaggle</span>
            <select
              aria-label="Switch gaggle"
              onChange={(event) => navigate({ page: "gaggle", id: event.target.value })}
              value={gaggle.name}
            >
              <option value={gaggle.name}>{gaggle.displayName}</option>
              {otherGaggles.map((candidate) => (
                <option key={candidate.name} value={candidate.name}>
                  {candidate.displayName}
                </option>
              ))}
            </select>
          </label>
        )}
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

      <GaggleActivitySections
        activity={activity}
        gaggleDisplayName={gaggle.displayName}
        workflows={inventory.workflows}
      />

      <GoobersPanel gaggleDisplayName={gaggle.displayName} goobers={inventory.goobers} />

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

/**
 * "What is this gaggle doing right now" (#2531): active runs plus a bounded
 * recent-outcome list, scoped to this gaggle instead of the per-workflow
 * last-outcome badges the topology already shows.
 */
function GaggleActivitySections({
  activity,
  gaggleDisplayName,
  workflows,
}: {
  activity: GaggleActivity | undefined;
  gaggleDisplayName: string;
  workflows: WorkflowSummary[];
}) {
  const workflowNames = new Map(
    workflows.map((workflow) => [workflow.identity.name, workflow.displayName]),
  );
  const label = (run: RunSummary) => workflowNames.get(run.workflow) ?? run.workflow;

  return (
    <>
      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">Live</p>
            <h2>Active runs</h2>
          </div>
          {activity && <span className="section-count">{activity.active.length}</span>}
        </div>
        {!activity ? (
          <p className="inline-empty">Loading active runs…</p>
        ) : activity.active.length === 0 ? (
          <p className="inline-empty">No runs are active for {gaggleDisplayName}.</p>
        ) : (
          <DataList
            ariaLabel={`${gaggleDisplayName} active runs`}
            columns={["Run", "Workflow", "Current stage", "Elapsed"]}
            gridClassName="run-grid"
          >
            {activity.active.map((run) => (
              <DataRow href={routeHash({ page: "run", id: run.id })} key={run.id} label={`Open run ${run.id}`}>
                <span className="row-primary">
                  <span className="row-title">
                    {run.workflow} · {run.id}
                  </span>
                </span>
                <span>{label(run)}</span>
                <span className="stage-progress">
                  <span aria-hidden="true" className="stage-progress-mark" />
                  {run.currentStage ?? "Awaiting stage"}
                </span>
                <span className="mono">{formatDuration(run.durationMillis)}</span>
              </DataRow>
            ))}
          </DataList>
        )}
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent outcomes</h2>
          </div>
          {activity && <span className="section-count">{activity.recent.length}</span>}
        </div>
        {!activity ? (
          <p className="inline-empty">Loading recent outcomes…</p>
        ) : activity.recent.length === 0 ? (
          <p className="inline-empty">No recent outcomes for {gaggleDisplayName}.</p>
        ) : (
          <DataList
            ariaLabel={`${gaggleDisplayName} recent outcomes`}
            columns={["Run", "Outcome", "Workflow", "Duration"]}
            gridClassName="outcome-grid"
          >
            {activity.recent.map((run) => (
              <DataRow href={routeHash({ page: "run", id: run.id })} key={run.id} label={`Open run ${run.id}`}>
                <span className="row-primary">
                  <span className="row-title">
                    {run.workflow} · {run.id}
                  </span>
                </span>
                <StatusBadge status={run.phase} />
                <span>{label(run)}</span>
                <span className="mono">{formatDuration(run.durationMillis)}</span>
              </DataRow>
            ))}
          </DataList>
        )}
      </section>
    </>
  );
}

/**
 * Configured goobers for this gaggle (#2531 — "what's configured" alongside
 * workflows). Non-goals: #1687's ready/needs-human backlog counts are not
 * computed here; this only renders the goober definitions already carried on
 * the inventory.
 */
function GoobersPanel({
  gaggleDisplayName,
  goobers,
}: {
  gaggleDisplayName: string;
  goobers: Goober[];
}) {
  return (
    <section className="content-section">
      <div className="section-heading">
        <div>
          <p className="section-kicker">Definitions</p>
          <h2>Goobers</h2>
        </div>
        <span className="section-count">{goobers.length}</span>
      </div>
      {goobers.length === 0 ? (
        <p className="inline-empty">No goobers are provisioned for this gaggle.</p>
      ) : (
        <ul aria-label={`${gaggleDisplayName} goobers`} className="gaggle-goober-list">
          {goobers.map((goober) => (
            <li className="gaggle-goober-node" key={goober.name}>
              <strong>{goober.displayName}</strong>
              <p>{goober.role}</p>
              <span className="gaggle-goober-meta">
                {goober.stages.length} {goober.stages.length === 1 ? "stage" : "stages"} owned
              </span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
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
      <div className="gaggle-workflow-card data-row-stretched">
        <a
          aria-label={`Open workflow ${workflow.displayName} for gaggle ${gaggleDisplayName}`}
          className="data-row-stretch-link"
          href={routeHash({
            page: "workflow",
            gaggle: workflow.identity.gaggle,
            id: workflow.identity.name,
          })}
        />
        <span className="gaggle-workflow-kind">
          <Icon name="workflow" size={13} />
          Workflow
        </span>
        <span className="gaggle-workflow-title">
          <strong>{workflow.displayName}</strong>
          <ScopePivot
            label={`${gaggleDisplayName} / ${workflow.displayName}`}
            scope={{ gaggle: workflow.identity.gaggle, workflow: workflow.identity.name }}
          />
        </span>
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
      </div>
    </li>
  );
}
