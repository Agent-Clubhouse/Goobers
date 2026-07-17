import { useState } from "react";
import { runs, workflowForRun } from "../prototypeData";
import type { Navigate } from "../routing";
import { DataList, DataRow } from "../ui/DataList";
import { StatusBadge } from "../ui/StatusBadge";

type RunFilter = "all" | "active" | "attention" | "complete";

export function RunsPage({ navigate }: { navigate: Navigate }) {
  const [filter, setFilter] = useState<RunFilter>("all");
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

      <div aria-label="Filter runs" className="filter-bar" role="group">
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
          columns={["Run", "Status", "Current stage", "Started", "Duration"]}
          gridClassName="all-runs-grid"
        >
          {filteredRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              onClick={() => navigate({ page: "run", id: run.id })}
            >
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
