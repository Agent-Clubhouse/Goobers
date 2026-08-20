import { useState } from "react";
import type { DaemonClient, RunSummary } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { RecoveryCommand } from "../components/RecoveryAction";
import { ScopeStrip } from "../components/ScopeStrip";
import { routeHash, type RunRouteFilters } from "../routing";
import { scopeWindowLabel } from "../scope";
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
  // Hides routine no-work schedule ticks by default (#2188): a run whose only
  // stage reported no eligible work, on an instance ticking every ~60s, would
  // otherwise bury the runs an operator actually came here to find. The
  // toggle is the explicit escape hatch — it never deletes or hides the
  // underlying run, only this list's default view of it.
  const [showNoWork, setShowNoWork] = useState(false);
  const scope = { ...filters, showNoWork };
  const query = useRunsHistory(client, filter, scope);

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
            ? `Executions behind the selected Insight scope${scopeWindowLabel(filters)}.`
            : standalone
              ? "Every execution recorded in this instance, filtered and paginated by the read service."
              : "Every execution across workflows and gaggles, filtered and paginated by the daemon."}
        </p>
      </header>

      {filters && (
        <ScopeStrip
          ariaLabel="Insight drill-through scope"
          clearHref={routeHash({ page: "runs" })}
          filters={filters}
          suffix={formatPopulation(filters)}
        />
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
        <label className="filter-toggle">
          <input
            checked={showNoWork}
            onChange={(event) => setShowNoWork(event.target.checked)}
            type="checkbox"
          />
          Show no-work runs
        </label>
      </div>

      {query.state.status === "stale" && query.state.error && (
        <div className="run-stale-state run-stale-state-error" role="alert">
          <span>
            <strong>Run history may be stale</strong>
            <small>{query.state.error.message}</small>
          </span>
          <button className="text-button" onClick={query.retry} type="button">
            Try again
          </button>
        </div>
      )}

      <section className="content-section">
        {history.runs.length === 0 ? (
          history.hasAnyRuns ? (
            <div className="inline-empty inline-empty-recovery">
              <strong>Filters exclude existing runs</strong>
              <span>Clear the current filters to return to the complete run history.</span>
              <a
                className="text-button"
                href={routeHash({ page: "runs" })}
                onClick={() => {
                  setFilter("all");
                  setShowNoWork(true);
                }}
              >
                Clear all filters
              </a>
              <RecoveryCommand command="goobers status <instance>" />
            </div>
          ) : (
            <div className="inline-empty inline-empty-recovery">
              <strong>No runs recorded</strong>
              <span>Start a configured workflow to create the first run journal.</span>
              <RecoveryCommand command="goobers run <workflow> <instance>" />
            </div>
          )
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

function formatPopulation(filters: RunRouteFilters): string {
  switch (filters.population) {
    case "measured":
      return " · Duration-measured attempts";
    case "token-measured":
      return " · Token-measured attempts";
    case "premium-measured":
      return " · AI-credit-measured attempts";
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
      <StatusBadge stale={run.stale} status={run.phase} />
      <span>{run.currentStage ?? (run.terminal ? "Terminal" : "Not started")}</span>
      <span>
        <time dateTime={run.startedAt}>{formatTimestamp(run.startedAt)}</time>
      </span>
      <span className="mono">{formatDuration(run.durationMillis)}</span>
    </DataRow>
  );
}
