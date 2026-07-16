import { useState } from "react";
import { DataList, DataRow } from "../components/DataList";
import { StatusBadge } from "../components/StatusBadge";
import type { Navigate } from "../foundation/navigation";
import { runs, workflowForRun, workflows } from "../prototypeData";

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
          ariaLabel="Provisioned workflows"
          headerClassName="workflow-grid"
          headings={["Workflow", "Trigger", "Concurrency", "Last outcome", ""]}
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

export function RunsPage({ navigate }: { navigate: Navigate }) {
  const [filter, setFilter] = useState<"all" | "active" | "attention" | "complete">("all");
  const filteredRuns = runs.filter((run) => {
    if (filter === "active") {
      return run.status === "running";
    }
    if (filter === "attention") {
      return run.status === "escalated" || run.status === "failed";
    }
    if (filter === "complete") {
      return run.status === "completed";
    }
    return true;
  });

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Journal</p>
        <h1>Runs</h1>
        <p>Every execution, ordered by the most recent journal activity.</p>
      </header>

      <div className="filter-bar" aria-label="Filter runs">
        {(["all", "active", "attention", "complete"] as const).map((option) => (
          <button
            aria-pressed={filter === option}
            className={filter === option ? "filter-button filter-button-active" : "filter-button"}
            key={option}
            onClick={() => setFilter(option)}
            type="button"
          >
            {option === "all" ? "All runs" : option}
          </button>
        ))}
      </div>

      <section className="content-section">
        <DataList
          ariaLabel="Run history"
          headerClassName="all-runs-grid"
          headings={["Run", "Status", "Current stage", "Started", "Duration", ""]}
        >
          {filteredRuns.map((run) => (
            <DataRow key={run.id} label={`Open run ${run.title}`} onClick={() => navigate({ page: "run", id: run.id })}>
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">
                  {run.issue} · {workflowForRun(run).name}
                </span>
              </span>
              <StatusBadge status={run.status} />
              <span>{run.currentStage}</span>
              <span>{run.startedAt}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </DataList>
      </section>
    </>
  );
}
