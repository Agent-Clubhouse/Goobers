import { useState } from "react";
import type {
  DaemonClient,
  ReadinessConditions,
  RunSummary,
  StageDefinition,
  WorkflowDetail,
} from "../api/types";
import type { ConfigurationWarningsProps } from "../components/ConfigurationWarnings";
import { ConfigurationWarnings } from "../components/ConfigurationWarnings";
import { WorkflowTopologyGraph } from "../components/WorkflowTopologyGraph";
import { formatDuration, formatTimestamp } from "../runDetailData";
import type { Navigate } from "../routing";
import { routeHash } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
import { GraphFrame } from "../ui/GraphFrame";
import { Icon } from "../ui/Icon";
import { Inspector } from "../ui/Inspector";
import { StatusBadge } from "../ui/StatusBadge";
import { useWorkflowDetail } from "../workflowDetailData";
import { formatTriggers } from "./WorkflowsPage";

export function WorkflowPage({
  client,
  configurationWarnings,
  gaggle,
  navigate,
  standalone,
  workflowName,
}: {
  client: DaemonClient;
  configurationWarnings: Omit<ConfigurationWarningsProps, "context">;
  gaggle: string;
  navigate: Navigate;
  standalone: boolean;
  workflowName: string;
}) {
  const query = useWorkflowDetail(client, gaggle, workflowName);

  if (query.state.status === "loading") {
    return (
      <section aria-live="polite" className="daemon-state" role="status">
        <span aria-hidden="true" className="loading-mark" />
        <div>
          <h1>Loading workflow</h1>
          <p>
            {standalone
              ? "Reading the current definition, canonical graph, and run history from local instance files."
              : "Reading the current definition, canonical graph, and run history from the daemon."}
          </p>
        </div>
      </section>
    );
  }
  if (query.state.status === "error") {
    return (
      <section className="daemon-state daemon-state-error" role="alert">
        <div>
          <h1>Workflow unavailable</h1>
          <p>{query.state.error.message}</p>
        </div>
        <button className="reconnect-button" onClick={query.retry} type="button">
          {standalone ? "Reload" : "Retry"}
        </button>
      </section>
    );
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  return (
    <>
      {query.state.status === "stale" && query.state.error && (
        <div className="workflow-stale-error" role="alert">
          <span>
            <strong>Workflow detail may be stale</strong>
            <small>{query.state.error.message}</small>
          </span>
          <button className="text-button" onClick={query.retry} type="button">
            Try again
          </button>
        </div>
      )}
      <WorkflowDetailWorkspace
        configurationWarnings={configurationWarnings}
        key={`${gaggle}/${workflowName}/${query.state.data.workflow.definition.digest}`}
        navigate={navigate}
        runs={query.state.data.runs}
        workflow={query.state.data.workflow}
      />
    </>
  );
}

function WorkflowDetailWorkspace({
  configurationWarnings,
  navigate,
  runs,
  workflow,
}: {
  configurationWarnings: Omit<ConfigurationWarningsProps, "context">;
  navigate: Navigate;
  runs: RunSummary[];
  workflow: WorkflowDetail;
}) {
  const initialStageId =
    workflow.stages.find((stage) => stage.name === workflow.graph.start)?.name ??
    workflow.stages[0]?.name ??
    "";
  const [selectedStageId, setSelectedStageId] = useState(initialStageId);
  const selectedStage =
    workflow.stages.find((stage) => stage.name === selectedStageId) ?? workflow.stages[0];

  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumbs">
        <button onClick={() => navigate({ page: "workflows" })} type="button">
          Workflows
        </button>
        <Icon name="chevron" size={14} />
        <span>{workflow.displayName}</span>
      </nav>
      <header className="detail-heading">
        <div>
          <span className="definition-label">Workflow definition</span>
          <h1>{workflow.displayName}</h1>
          <p>{workflow.purpose}</p>
        </div>
        <dl className="detail-meta workflow-detail-meta">
          <div>
            <dt>Trigger</dt>
            <dd>{formatTriggers(workflow)}</dd>
          </div>
          <div>
            <dt>Concurrency</dt>
            <dd>
              {workflow.concurrency.activeRuns} active /{" "}
              {workflow.concurrency.maxConcurrentRuns} max
            </dd>
          </div>
          <div>
            <dt>Gaggle</dt>
            <dd>{workflow.identity.gaggle}</dd>
          </div>
          <div>
            <dt>Definition</dt>
            <dd className="mono workflow-definition-pin">
              v{workflow.definition.version} · {workflow.definition.digest}
            </dd>
          </div>
        </dl>
      </header>

      <section aria-label="Workflow configuration summary" className="workflow-config-summary">
        <dl>
          <div>
            <dt>Owners</dt>
            <dd>
              {workflow.owners.length > 0
                ? workflow.owners.map((owner) => `${owner.gaggle}/${owner.name}`).join(", ")
                : "None declared"}
            </dd>
          </div>
          <div>
            <dt>Stages</dt>
            <dd>{workflow.stageCount}</dd>
          </div>
          <div>
            <dt>Readiness</dt>
            <dd>{formatReadiness(workflow.readiness)}</dd>
          </div>
        </dl>
      </section>

      <ConfigurationWarnings context="workflow" {...configurationWarnings} />

      <section className="graph-layout">
        <GraphFrame
          action={<span className="graph-legend">Select a stage to inspect its definition</span>}
        >
          <WorkflowTopologyGraph
            graph={workflow.graph}
            onSelectStage={setSelectedStageId}
            selectedStageId={selectedStageId}
          />
        </GraphFrame>
        {selectedStage && <StageDefinitionSummary stage={selectedStage} />}
      </section>

      <RecentRuns runs={runs} workflow={workflow} />
    </>
  );
}

function StageDefinitionSummary({ stage }: { stage: StageDefinition }) {
  const actor =
    stage.kind === "gate"
      ? stage.evaluator
        ? `${stage.evaluator} evaluator`
        : "Evaluator not declared"
      : stage.owner
        ? `${stage.owner.gaggle}/${stage.owner.name}`
        : stage.kind === "deterministic"
          ? "Deterministic runtime"
          : "Owner not declared";

  return (
    <Inspector className="definition-panel" label={`${stage.name} definition`}>
      <div className="inspector-heading">
        <span className={`primitive-icon primitive-${stage.kind}`}>
          <Icon
            name={stage.kind === "gate" ? "gate" : stage.kind === "agentic" ? "code" : "workflow"}
            size={17}
          />
        </span>
        <div>
          <span>{stageKindLabel(stage)}</span>
          <h3>{stage.name}</h3>
        </div>
      </div>
      <p className="inspector-description">{stage.goal || "No stage goal declared."}</p>
      <dl className="property-list">
        <div>
          <dt>{stage.kind === "gate" ? "Evaluator" : "Owner"}</dt>
          <dd>{actor}</dd>
        </div>
        <div>
          <dt>Capabilities</dt>
          <dd>
            {stage.capabilities.length > 0 ? stage.capabilities.join(", ") : "None declared"}
          </dd>
        </div>
      </dl>
    </Inspector>
  );
}

function RecentRuns({ runs, workflow }: { runs: RunSummary[]; workflow: WorkflowDetail }) {
  return (
    <section className="content-section">
      <div className="section-heading">
        <div>
          <p className="section-kicker">History</p>
          <h2>Recent runs</h2>
        </div>
        <span className="section-count">{runs.length}</span>
      </div>
      {runs.length === 0 ? (
        <p className="inline-empty">No runs are recorded for this workflow.</p>
      ) : (
        <DataList
          ariaLabel={`${workflow.displayName} recent runs`}
          columns={["Run", "Outcome", "Stage", "Duration"]}
          gridClassName="workflow-runs-grid"
        >
          {runs.map((run) => (
            <DataRow
              href={routeHash({ page: "run", id: run.id })}
              key={run.id}
              label={`Open run ${run.id}`}
            >
              <span className="row-primary">
                <span className="row-title mono">{run.id}</span>
                <span className="row-subtitle">
                  {run.trigger.kind}
                  {run.trigger.ref ? ` · ${run.trigger.ref}` : ""} ·{" "}
                  <time dateTime={run.startedAt}>{formatTimestamp(run.startedAt)}</time>
                </span>
              </span>
              <StatusBadge status={run.phase} />
              <span>{run.currentStage ?? (run.terminal ? "Terminal" : "Not started")}</span>
              <span className="mono">{formatDuration(run.durationMillis)}</span>
            </DataRow>
          ))}
        </DataList>
      )}
    </section>
  );
}

function formatReadiness(readiness: ReadinessConditions): string {
  const limits = [
    ["runs", readiness.maxConcurrentRuns],
    ["runs/hour", readiness.maxRunsPerHour],
    ["runs/day", readiness.maxRunsPerDay],
    ["chain depth", readiness.maxChainDepth],
    ["open PRs", readiness.maxOpenPRs],
  ] as const;
  const configured = limits.flatMap(([label, value]) =>
    value === undefined ? [] : [`${value} ${label}`],
  );
  return configured.length > 0 ? configured.join(", ") : "No additional limits";
}

function stageKindLabel(stage: StageDefinition): string {
  switch (stage.kind) {
    case "agentic":
      return "Agentic task";
    case "deterministic":
      return "Deterministic task";
    case "gate":
      return "Gate";
  }
}
