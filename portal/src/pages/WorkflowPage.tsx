import { useState } from "react";
import { DataList, DataRow } from "../foundation/DataList";
import { GraphFrame } from "../foundation/GraphFrame";
import { Icon } from "../foundation/Icon";
import { Inspector } from "../foundation/Inspector";
import { StatusBadge } from "../foundation/StatusBadge";
import { runs, type Workflow, type WorkflowStage } from "../fixtures";
import type { Route } from "../routes";

function StageDefinition({ stage }: { stage: WorkflowStage }) {
  return (
    <Inspector className="definition-panel" kind={stage.kind} title={stage.name}>
      <p className="inspector-description">{stage.description}</p>
      <dl className="property-list">
        <div>
          <dt>{stage.kind === "gate" ? "Evaluator" : "Goober"}</dt>
          <dd>{stage.evaluator ?? stage.goober}</dd>
        </div>
        <div>
          <dt>Policy</dt>
          <dd>{stage.retry}</dd>
        </div>
      </dl>
      <div className="code-heading">
        <span>Definition</span>
        <span>YAML</span>
      </div>
      <pre className="code-block">{stage.yaml}</pre>
    </Inspector>
  );
}

export function WorkflowPage({
  workflow,
  navigate,
}: {
  workflow: Workflow;
  navigate: (route: Route) => void;
}) {
  const [selectedStageId, setSelectedStageId] = useState(workflow.stages[0]?.id ?? "");
  const selectedStage = workflow.stages.find((stage) => stage.id === selectedStageId) ?? workflow.stages[0];
  const workflowRuns = runs.filter((run) => run.workflowId === workflow.id);

  return (
    <>
      <nav className="breadcrumbs" aria-label="Breadcrumb">
        <button onClick={() => navigate({ page: "workflows" })} type="button">
          Workflows
        </button>
        <Icon name="chevron" size={14} />
        <span>{workflow.name}</span>
      </nav>
      <header className="detail-heading">
        <div>
          <span className="definition-label">Workflow definition</span>
          <h1>{workflow.name}</h1>
          <p>{workflow.description}</p>
        </div>
        <dl className="detail-meta">
          <div>
            <dt>Trigger</dt>
            <dd>{workflow.trigger}</dd>
          </div>
          <div>
            <dt>Concurrency</dt>
            <dd>{workflow.activeRuns} / {workflow.maxConcurrency}</dd>
          </div>
          <div>
            <dt>Gaggle</dt>
            <dd>{workflow.gaggle}</dd>
          </div>
        </dl>
      </header>

      <section className="graph-layout">
        <div className="graph-panel">
          <div className="panel-heading-row">
            <div>
              <p className="section-kicker">Structure</p>
              <h2>Execution graph</h2>
            </div>
            <span className="graph-legend">Select a stage to inspect it</span>
          </div>
          <GraphFrame
            onSelectStage={setSelectedStageId}
            selectedStageId={selectedStageId}
            workflow={workflow}
          />
        </div>
        {selectedStage && <StageDefinition stage={selectedStage} />}
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent runs</h2>
          </div>
        </div>
        <DataList label="Recent workflow runs" layout="outcome-grid">
          {workflowRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              layout="outcome-grid"
              onClick={() => navigate({ page: "run", id: run.id })}
            >
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">{run.issue} · {run.startedAt}</span>
              </span>
              <StatusBadge status={run.status} />
              <span>{run.currentStage}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </DataList>
      </section>
    </>
  );
}
