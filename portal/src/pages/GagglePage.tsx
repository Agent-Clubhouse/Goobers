import { PageHeading, SectionHeading } from "../ui/Heading";
import { DataTable } from "../ui/DataTable";
import { MetadataGrid } from "../ui/MetadataGrid";
import { ControlGroup } from "../ui/Filters";
import { Timestamp } from "../ui/Timestamp";
import { Action, ActionLink } from "../ui/Action";
import { RunTiming } from "../components/RunTiming";
import type {
  DaemonClient,
  Goober,
  RepositoryConnection,
  RunSummary,
  WorkflowSummary,
} from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { GaggleWorkflowExplorer } from "../components/GaggleWorkflowExplorer";
import { DisclosureSection } from "../components/DisclosureSection";
import { ScopePivot } from "../components/ScopePivot";
import { providerName } from "../runDetailData";
import {
  incompleteRunPhasesMessage,
  useGaggleActivity,
  useOperationalSnapshot,
  type GaggleActivity,
  type GaggleInventory,
} from "../operationalData";
import type { Navigate } from "../routing";
import { routeHash } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
import { Icon } from "../ui/Icon";
import { StatusBadge } from "../ui/StatusBadge";

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
  const activityQuery = useGaggleActivity(client, gaggleName);

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

  const inventory = query.state.data.inventories.find(({ gaggle }) => gaggle.name === gaggleName);
  if (!inventory) {
    return (
      <section className="daemon-state daemon-state-error" role="alert">
        <div>
          <h1>Gaggle unavailable</h1>
          <p>No gaggle named "{gaggleName}" is configured in this instance.</p>
        </div>
        <Action
          variant="primary"
          className="reconnect-button"
          onClick={() => navigate({ page: "workflows" })}
          type="button"
        >
          View workflows
        </Action>
      </section>
    );
  }

  const activity =
    activityQuery.state.status === "ready" || activityQuery.state.status === "stale"
      ? activityQuery.state.data
      : undefined;

  return (
    <GaggleTopology
      activity={activity}
      client={client}
      inventory={inventory}
      navigate={navigate}
      runs={query.state.data.runs}
    />
  );
}

function GaggleTopology({
  activity,
  client,
  inventory,
  navigate,
  runs,
}: {
  activity: GaggleActivity | undefined;
  client: DaemonClient;
  inventory: GaggleInventory;
  navigate: Navigate;
  runs: RunSummary[];
}) {
  const { gaggle } = inventory;

  return (
    <div className="gaggle-detail-page">
      <nav aria-label="Breadcrumb" className="breadcrumbs">
        <button onClick={() => navigate({ page: "workflows" })} type="button">
          Workflows
        </button>
        <Icon name="chevron" size={14} />
        <span>{gaggle.displayName}</span>
      </nav>
      <PageHeading
        title={gaggle.displayName}
        className="gaggle-detail-heading"
        description={
          <>
            {gaggle.name} · {gaggle.project.owner}/{gaggle.project.name}
          </>
        }
        fullWidth
      />
      <ControlGroup className="gaggle-detail-controls" label="Gaggle scope">
        <ScopePivot label={gaggle.displayName} scope={{ gaggle: gaggle.name }} />
        <MetadataGrid className="detail-meta" layout="inline">
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
        </MetadataGrid>
      </ControlGroup>

      {gaggle.template && (
        <section className="daemon-state" aria-label="Template updates">
          <div>
            <h2>
              {gaggle.template.state === "update-available"
                ? "Template update available"
                : gaggle.template.state === "conflicts"
                  ? "Template update needs conflict resolution"
                  : `Template: ${gaggle.template.state}`}
            </h2>
            <p>Installed revision: {gaggle.template.installed || "not checked"}</p>
            {gaggle.template.candidate && <p>Source revision: {gaggle.template.candidate}</p>}
            <p>
              Last successful check:{" "}
              {gaggle.template.lastSuccess.startsWith("0001-") ? (
                "never"
              ) : (
                <Timestamp value={gaggle.template.lastSuccess} />
              )}
            </p>
            {gaggle.template.error && <p role="alert">{gaggle.template.error}</p>}
            {gaggle.template.pendingBackprop && (
              <p>Runtime edits need backprop into your config repository before deployment.</p>
            )}
            {(gaggle.template.changes?.length ?? 0) > 0 && (
              <p>Changed files: {gaggle.template.changes?.join(", ")}</p>
            )}
            {(gaggle.template.conflicts?.length ?? 0) > 0 && (
              <p>Conflicts: {gaggle.template.conflicts?.join("; ")}</p>
            )}
            <p>
              Updates are never applied automatically. Stop the instance, then review with{" "}
              <code>goobers config templates update --gaggle {gaggle.name}</code>.
            </p>
          </div>
        </section>
      )}

      <GaggleActivitySections
        activity={activity}
        gaggleDisplayName={gaggle.displayName}
        workflows={inventory.workflows}
      />

      <GoobersPanel
        gaggleDisplayName={gaggle.displayName}
        gaggleName={gaggle.name}
        goobers={inventory.goobers}
      />

      <GaggleWorkflowExplorer
        client={client}
        gaggleDisplayName={gaggle.displayName}
        runs={runs}
        workflows={inventory.workflows}
      />

      <div className="gaggle-connections-section">
        <DisclosureSection
          count={inventory.connections.length}
          defaultOpen
          title="Repository connections"
        >
          <ConnectionTopology
            connections={inventory.connections}
            gaggleDisplayName={gaggle.displayName}
            hasWorkflows={inventory.workflows.length > 0}
          />
        </DisclosureSection>
      </div>
    </div>
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
      <h3>Repositories available to this gaggle</h3>
      <p className="gaggle-connection-description">
        {hasWorkflows
          ? "These repositories are connected through the gaggle's configured workflows."
          : "These repositories are configured for the gaggle even though it has no workflows."}
      </p>
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
                <p>{providerName(connection.repository.provider)}</p>
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
  const label = (run: RunSummary) => workflowNames.get(run.workflow);
  const identity = (run: RunSummary) => `${run.gaggle} / ${run.workflow}`;

  return (
    <>
      {/* Some phase queries failed while others succeeded: the sections below
          are missing those runs and must not read as an idle gaggle (#3658). */}
      {activity?.incomplete && (
        <p className="inline-empty" role="alert">
          {incompleteRunPhasesMessage(activity.incomplete)}
        </p>
      )}
      <DisclosureSection count={activity?.active.length} defaultOpen title="Active runs">
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
              <DataRow
                href={routeHash({ page: "run", id: run.id })}
                key={run.id}
                label={`Open run ${run.id}`}
              >
                <span className="row-primary">
                  <span className="row-title">
                    {identity(run)} · {run.id}
                  </span>
                </span>
                <span>{label(run) ?? run.workflow}</span>
                <span className="stage-progress">
                  <span aria-hidden="true" className="stage-progress-mark" />
                  {run.currentStage ?? "Awaiting stage"}
                </span>
                <RunTiming run={run} />
              </DataRow>
            ))}
          </DataList>
        )}
      </DisclosureSection>

      <DisclosureSection count={activity?.recent.length} title="Recent outcomes">
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
              <DataRow
                href={routeHash({ page: "run", id: run.id })}
                key={run.id}
                label={`Open run ${run.id}`}
              >
                <span className="row-primary">
                  <span className="row-title">
                    {identity(run)} · {run.id}
                  </span>
                </span>
                <StatusBadge status={run.phase} />
                <span>{label(run) ?? run.workflow}</span>
                <RunTiming run={run} />
              </DataRow>
            ))}
          </DataList>
        )}
      </DisclosureSection>
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
  gaggleName,
  goobers,
}: {
  gaggleDisplayName: string;
  gaggleName: string;
  goobers: Goober[];
}) {
  return (
    <section className="content-section">
      <SectionHeading
        title="Goobers"
        className=""
        actions={
          <>
            <ActionLink
              variant="text"
              size="compact"
              className="text-button run-link-action"
              href={routeHash({ page: "goobers", gaggle: gaggleName })}
            >
              View full Goober details
            </ActionLink>
          </>
        }
      />
      {goobers.length === 0 ? (
        <p className="inline-empty">No goobers are provisioned for this gaggle.</p>
      ) : (
        <DataTable
          ariaLabel={`${gaggleDisplayName} goobers`}
          className="gaggle-goober-table"
          columns={["Goober", "Role", "Stages owned"]}
        >
          {goobers.map((goober) => (
            <tr key={goober.name}>
              <td className="data-table-primary">{goober.displayName}</td>
              <td>{goober.role}</td>
              <td className="data-table-number">{goober.stages.length}</td>
            </tr>
          ))}
        </DataTable>
      )}
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
