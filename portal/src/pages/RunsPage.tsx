import { useState } from "react";
import type { DaemonClient, RunSummary } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { routeHash } from "../routing";
import { type RunsFilter, useRunsHistory } from "../runsHistory";
import { formatDuration, formatTimestamp } from "../runDetailData";
import { DataList, DataRow } from "../ui/DataList";
import { StatusBadge } from "../ui/StatusBadge";

const FILTERS: readonly RunsFilter[] = ["all", "active", "attention", "complete"];

export function RunsPage({
  client,
  standalone,
}: {
  client: DaemonClient;
  standalone: boolean;
}) {
  const [filter, setFilter] = useState<RunsFilter>("all");
  const query = useRunsHistory(client, filter);

  if (query.state.status === "loading") {
    return <DaemonLoadingState standalone={standalone} />;
  }
  if (query.state.status === "error") {
    return <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />;
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  const history = query.state.data;

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Journal</p>
        <h1>Runs</h1>
        <p>
          {standalone
            ? "Every execution recorded in this instance, filtered and paginated by the read service."
            : "Every execution across workflows and gaggles, filtered and paginated by the daemon."}
        </p>
      </header>

      <div aria-label="Filter runs" className="filter-bar" role="group">
        {FILTERS.map((option) => (
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
        {history.runs.length === 0 ? (
          <p className="inline-empty">No runs match this filter.</p>
        ) : (
          <>
            <DataList
              ariaLabel="Run history"
              columns={["Run", "Status", "Current stage", "Started", "Duration"]}
              gridClassName="all-runs-grid"
            >
              {history.runs.map((run) => (
                <RunHistoryRow key={run.id} run={run} />
              ))}
            </DataList>
            {history.hasMore && (
              <div className="load-more">
                <button
                  className="text-button"
                  disabled={history.loadingMore}
                  onClick={query.loadMore}
                  type="button"
                >
                  {history.loadingMore ? "Loading…" : "Load more runs"}
                </button>
              </div>
            )}
          </>
        )}
      </section>
    </>
  );
}

function RunHistoryRow({ run }: { run: RunSummary }) {
  return (
    <DataRow href={routeHash({ page: "run", id: run.id })} label={`Open run ${run.id}`}>
      <span className="row-primary">
        <span className="row-title mono">{run.id}</span>
        <span className="row-subtitle">
          {run.gaggle} / {run.workflow}
          {run.trigger.ref ? ` · ${run.trigger.ref}` : ""}
        </span>
      </span>
      <StatusBadge status={run.phase} />
      <span>{run.currentStage ?? (run.terminal ? "Terminal" : "Not started")}</span>
      <span>
        <time dateTime={run.startedAt}>{formatTimestamp(run.startedAt)}</time>
      </span>
      <span className="mono">{formatDuration(run.durationMillis)}</span>
    </DataRow>
  );
}
