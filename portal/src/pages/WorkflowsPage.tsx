import { workflows } from "../prototypeData";
import type { Navigate } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
import { StatusBadge } from "../ui/StatusBadge";

export function WorkflowsPage({ navigate }: { navigate: Navigate }) {
  return (
    <>
      <header className="page-heading page-heading-row">
        <div>
          <p className="page-kicker">Definitions</p>
          <h1>Workflows</h1>
          <p>Versioned processes currently provisioned in the local instance.</p>
        </div>
        <div className="scope-chip">
          <span className="scope-mark">G</span>
          goobers gaggle
        </div>
      </header>

      <section className="content-section">
        <DataList
          ariaLabel="Workflow definitions"
          columns={["Workflow", "Trigger", "Concurrency", "Last outcome"]}
          gridClassName="workflow-grid"
        >
          {workflows.map((workflow) => (
            <DataRow
              key={workflow.id}
              label={`Open workflow ${workflow.name}`}
              onClick={() => navigate({ page: "workflow", id: workflow.id })}
            >
              <span className="row-primary">
                <span className="row-title">{workflow.name}</span>
                <span className="row-subtitle">{workflow.description}</span>
              </span>
              <span>{workflow.trigger}</span>
              <span>
                {workflow.activeRuns} active / {workflow.maxConcurrency} max
              </span>
              <span className="outcome-cell">
                <StatusBadge status={workflow.lastOutcome} />
                <small>{workflow.lastRunAt}</small>
              </span>
            </DataRow>
          ))}
        </DataList>
      </section>
    </>
  );
}
