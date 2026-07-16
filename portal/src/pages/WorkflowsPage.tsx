import { DataList, DataRow } from "../foundation/DataList";
import { StatusBadge } from "../foundation/StatusBadge";
import { workflows } from "../fixtures";
import type { Route } from "../routes";

export function WorkflowsPage({ navigate }: { navigate: (route: Route) => void }) {
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
          headers={["Workflow", "Trigger", "Concurrency", "Last outcome", ""]}
          label="Provisioned workflows"
          layout="workflow-grid"
        >
          {workflows.map((workflow) => (
            <DataRow
              key={workflow.id}
              label={`Open workflow ${workflow.name}`}
              layout="workflow-grid"
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
