import { useState } from "react";
import { DataList, DataRow } from "../foundation/DataList";
import { StatusBadge } from "../foundation/StatusBadge";
import { runs, workflowForRun } from "../fixtures";
import type { Route } from "../routes";

type RunFilter = "all" | "active" | "attention" | "complete";

export function RunsPage({ navigate }: { navigate: (route: Route) => void }) {
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
          headers={["Run", "Status", "Current stage", "Started", "Duration", ""]}
          label="Run history"
          layout="all-runs-grid"
        >
          {filteredRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              layout="all-runs-grid"
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
