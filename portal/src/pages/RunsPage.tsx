import { useState } from "react";
import type { DaemonClient, RunSummary } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { routeHash, type RunRouteFilters } from "../routing";
import { type RunsFilter, useRunsHistory } from "../runsHistory";
import { formatDuration, formatTimestamp } from "../runDetailData";
import { DataList, DataRow } from "../ui/DataList";
import { StatusBadge } from "../ui/StatusBadge";

const FILTERS: readonly RunsFilter[] = ["all", "active", "attention", "complete"];

export function RunsPage({
  client,
  filters,
  standalone,
}: {
  client: DaemonClient;
  filters?: RunRouteFilters;
  standalone: boolean;
}) {
  const [filter, setFilter] = useState<RunsFilter>("all");
  const query = useRunsHistory(client, filter, filters);

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
          {filters
            ? `Executions behind the selected Insight scope${formatWindow(filters)}.`
            : standalone
              ? "Every execution recorded in this instance, filtered and paginated by the read service."
              : "Every execution across workflows and gaggles, filtered and paginated by the daemon."}
        </p>
      </header>

      {filters && (
        <div aria-label="Insight drill-through scope" className="run-scope-strip">
          <strong>{formatScope(filters)}</strong>
          <a href={routeHash({ page: "runs" })}>Clear Insight scope</a>
        </div>
      )}

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

function formatScope(filters: RunRouteFilters): string {
  let scope: string;
  if (filters.stage) {
    scope = `${filters.gaggle ?? "All gaggles"} / ${filters.workflow ?? "All workflows"} / ${filters.stage}`;
  } else if (filters.workflow) {
    scope = `${filters.gaggle ?? "All gaggles"} / ${filters.workflow}`;
  } else {
    scope = filters.gaggle ? `Gaggle: ${filters.gaggle}` : "Instance";
  }
  return `${scope}${formatPopulation(filters)}`;
}

function formatPopulation(filters: RunRouteFilters): string {
  switch (filters.population) {
    case "measured":
      return " · Duration-measured attempts";
    case "token-measured":
      return " · Token-measured attempts";
    case "cost-measured":
      return " · Cost-measured attempts";
    case "retry-waste":
      return " · Superseded attempts";
  }
  switch (filters.outcome) {
    case "terminal":
      return " · Terminal outcomes";
    case "success":
      return " · Successful outcomes";
    case "failure":
      return " · Failed outcomes";
    case "other":
      return " · Other outcomes";
    default:
      return filters.population === "attempts" ? " · All attempts" : "";
  }
}

function formatWindow(filters: RunRouteFilters): string {
  if (filters.since && filters.until) {
    return ` from ${formatTimestamp(filters.since)} to ${formatTimestamp(filters.until)}`;
  }
  if (filters.since) {
    return ` since ${formatTimestamp(filters.since)}`;
  }
  if (filters.until) {
    return ` through ${formatTimestamp(filters.until)}`;
  }
  return "";
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
