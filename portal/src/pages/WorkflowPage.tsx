import { useState } from "react";
import { runs, type Workflow, type WorkflowStage } from "../prototypeData";
import type { Navigate } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
import { GraphFrame, TopologyGraph } from "../ui/GraphFrame";
import { Icon } from "../ui/Icon";
import { Inspector, InspectorHeading } from "../ui/Inspector";
import { StatusBadge } from "../ui/StatusBadge";

function StageDefinition({ stage }: { stage: WorkflowStage }) {
  return (
    <Inspector className="definition-panel" label={`${stage.name} definition`}>
      <InspectorHeading stage={stage} />
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
  navigate: Navigate;
}) {
  const [selectedStageId, setSelectedStageId] = useState(workflow.stages[0]?.id ?? "");
  const selectedStage =
    workflow.stages.find((stage) => stage.id === selectedStageId) ?? workflow.stages[0];
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
        <GraphFrame action={<span className="graph-legend">Select a stage to inspect it</span>}>
          <TopologyGraph
            onSelectStage={setSelectedStageId}
            selectedStageId={selectedStageId}
            workflow={workflow}
          />
        </GraphFrame>
        {selectedStage && <StageDefinition stage={selectedStage} />}
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent runs</h2>
          </div>
        </div>
        <DataList ariaLabel={`${workflow.name} recent runs`}>
          {workflowRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
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
