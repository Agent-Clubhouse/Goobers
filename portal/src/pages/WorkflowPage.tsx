import { useState } from "react";
import { DataCell, DataList, DataRow } from "../components/DataList";
import { GraphFrame } from "../components/GraphFrame";
import { StageDefinition } from "../components/Inspectors";
import { StatusBadge } from "../components/StatusBadge";
import { Icon } from "../foundation/Icon";
import type { Navigate } from "../foundation/navigation";
import { runs, type Workflow } from "../prototypeData";

export function WorkflowPage({ workflow, navigate }: { workflow: Workflow; navigate: Navigate }) {
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
        <GraphFrame
          onSelectStage={setSelectedStageId}
          selectedStageId={selectedStageId}
          workflow={workflow}
        />
        {selectedStage && <StageDefinition stage={selectedStage} />}
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent runs</h2>
          </div>
        </div>
        <DataList
          ariaLabel={`${workflow.name} recent runs`}
          headerClassName="workflow-runs-grid"
          headings={["Run", "Status", "Current stage", "Duration", ""]}
        >
          {workflowRuns.map((run) => (
            <DataRow key={run.id} label={`Open run ${run.title}`} onClick={() => navigate({ page: "run", id: run.id })}>
              <DataCell className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">{run.issue} · {run.startedAt}</span>
              </DataCell>
              <DataCell className="status-cell">
                <StatusBadge status={run.status} />
              </DataCell>
              <DataCell>{run.currentStage}</DataCell>
              <DataCell className="mono">{run.duration}</DataCell>
            </DataRow>
          ))}
        </DataList>
      </section>
    </>
  );
}
