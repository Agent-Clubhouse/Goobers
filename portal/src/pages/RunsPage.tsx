import { useEffect, useState } from "react";
import { RunTiming } from "../components/RunTiming";
import type { DaemonClient, RunSummary } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { RecoveryCommand } from "../components/RecoveryAction";
import { PageToolbar, type ActivePageFilter } from "../components/PageToolbar";
import { manualRunCommand, statusCommand } from "../manualRunCommand";
import { useOperationalSnapshot } from "../operationalData";
import {
  routeHash,
  type Navigate,
  type RunRouteFilters,
} from "../routing";
import { scopeWindowLabel } from "../scope";
import { type RunsFilter, useRunsHistory } from "../runsHistory";
import { formatTimestamp } from "../runDetailData";
import { DataList, DataRow } from "../ui/DataList";
import { StatusBadge } from "../ui/StatusBadge";

const FILTERS: readonly RunsFilter[] = ["active", "attention", "complete", "all"];
const NARROW_RUNS_PAGE_SIZE = 20;

export function RunsPage({
  client,
  filters,
  navigate,
  standalone,
}: {
  client: DaemonClient;
  filters?: RunRouteFilters;
  navigate: Navigate;
  standalone: boolean;
}) {
  const analyticalScope = Boolean(
    filters?.since ||
    filters?.until ||
    filters?.outcome ||
    filters?.population ||
    filters?.stage,
  );
  const filter = filters?.status ?? (analyticalScope ? "all" : "active");
  const inventoryQuery = useOperationalSnapshot(client);
  // Hides routine no-work schedule ticks by default (#2188): a run whose only
  // stage reported no eligible work, on an instance ticking every ~60s, would
  // otherwise bury the runs an operator actually came here to find. The
  // toggle is the explicit escape hatch — it never deletes or hides the
  // underlying run, only this list's default view of it.
  const showNoWork = filters?.showNoWork ?? false;
  const [draft, setDraft] = useState<RunRouteFilters>({ ...filters });
  useEffect(() => setDraft({ ...filters }), [filters]);
  const scope = { ...filters, showNoWork };
  const pageSize =
    typeof window !== "undefined" && window.innerWidth <= 480
      ? NARROW_RUNS_PAGE_SIZE
      : undefined;
  const query = useRunsHistory(client, filter, scope, pageSize);
  const snapshot =
    inventoryQuery.state.status === "ready" || inventoryQuery.state.status === "stale"
      ? inventoryQuery.state.data
      : undefined;
  const inventories = snapshot?.inventories ?? [];
  const instanceRoot = snapshot?.instance.instanceRoot;
  const gaggleOptions = inventories.map(({ gaggle }) => ({
    name: gaggle.name,
    label: gaggle.displayName || gaggle.name,
  }));
  const workflowOptions = inventories
    .filter(({ gaggle }) => !filters?.gaggle || gaggle.name === filters.gaggle)
    .flatMap(({ gaggle, workflows }) =>
      workflows.map((workflow) => ({
        gaggle: gaggle.name,
        name: workflow.identity.name,
        label: workflow.displayName || workflow.identity.name,
      })),
    );

  const updateFilters = (updates: Partial<RunRouteFilters>) => {
    const next = { ...filters, ...updates };
    navigate({
      page: "runs",
      filters: Object.values(next).some(Boolean) ? next : undefined,
    });
  };
  const resetFilters = () => navigate({ page: "runs" });

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
  const filterError = runsFilterError(filters, gaggleOptions, workflowOptions);
  const activeFilters: ActivePageFilter[] = [
    ...(filter !== "active" ? [{
      key: "status",
      label: `Status: ${filter}`,
      onRemove: () => updateFilters({ status: undefined }),
    }] : []),
    ...(filters?.gaggle ? [{
      key: "gaggle",
      label: `Gaggle: ${filters.gaggle}`,
      onRemove: () => updateFilters({ gaggle: undefined, workflow: undefined }),
    }] : []),
    ...(filters?.workflow ? [{
      key: "workflow",
      label: `Workflow: ${filters.workflow}`,
      onRemove: () => updateFilters({ workflow: undefined }),
    }] : []),
    ...(showNoWork ? [{
      key: "no-work",
      label: "Includes no-work runs",
      onRemove: () => updateFilters({ showNoWork: undefined }),
    }] : []),
  ];
  const renderFilters = (mobile: boolean) => {
    const values = mobile ? draft : { ...filters, status: filter, showNoWork };
    const change = (updates: Partial<RunRouteFilters>) => {
      if (mobile) {
        setDraft((current) => ({ ...current, ...updates }));
      } else {
        updateFilters(updates);
      }
    };
    const selectedStatus = values.status ?? "active";
    return (
      <div aria-label="Filter runs" className="filter-bar" role="group">
        {FILTERS.map((option) => (
          <button
            aria-pressed={selectedStatus === option}
            className={selectedStatus === option ? "filter-button filter-button-active" : "filter-button"}
            key={option}
            onClick={() => change({ status: option === "active" ? undefined : option })}
            type="button"
          >
            {option === "all" ? "All runs" : option}
          </button>
        ))}
        <div className="run-filter-fields">
          <label className="filter-select run-filter-field">
            <span>Gaggle</span>
            <select
              aria-label={mobile ? "Draft gaggle filter" : "Filter by gaggle"}
              onChange={(event) => change({
                gaggle: event.target.value || undefined,
                workflow: undefined,
              })}
              value={values.gaggle ?? ""}
            >
              <option value="">All gaggles</option>
              {gaggleOptions.map((gaggle) => (
                <option key={gaggle.name} value={gaggle.name}>{gaggle.label}</option>
              ))}
            </select>
          </label>
          <label className="filter-select run-filter-field">
            <span>Workflow</span>
            <select
              aria-label={mobile ? "Draft workflow filter" : "Filter by workflow"}
              onChange={(event) => {
                const [gaggle, workflow] = event.target.value
                  ? JSON.parse(event.target.value) as [string, string]
                  : ["", ""];
                change({
                  gaggle: workflow ? gaggle : values.gaggle,
                  workflow: workflow || undefined,
                });
              }}
              value={values.workflow
                ? JSON.stringify([values.gaggle ?? "", values.workflow])
                : ""}
            >
              <option value="">All workflows</option>
              {workflowOptions
                .filter((workflow) => !values.gaggle || workflow.gaggle === values.gaggle)
                .map((workflow) => (
                  <option
                    key={`${workflow.gaggle}/${workflow.name}`}
                    value={JSON.stringify([workflow.gaggle, workflow.name])}
                  >
                    {values.gaggle ? workflow.label : `${workflow.gaggle} / ${workflow.label}`}
                  </option>
                ))}
            </select>
          </label>
          <label className="filter-toggle run-filter-field">
            <input
              aria-label={mobile ? "Draft show no-work runs" : "Show no-work runs"}
              checked={values.showNoWork ?? false}
              onChange={(event) => change({ showNoWork: event.target.checked || undefined })}
              type="checkbox"
            />
            Show no-work runs
          </label>
        </div>
      </div>
    );
  };

  return (
    <>
      <PageToolbar
        activeFilters={activeFilters}
        count={history.runs.length}
        description={filters
          ? `Executions behind the selected Insight scope${scopeWindowLabel(filters)}.`
          : standalone
            ? "Every execution recorded in this instance, filtered and paginated by the read service."
            : "Every execution across workflows and gaggles, filtered and paginated by the daemon."}
        filterError={filterError}
        filters={renderFilters}
        onApplyFilters={() => {
          const error = runsFilterError(draft, gaggleOptions, workflowOptions, false);
          if (error) return error;
          navigate({
            page: "runs",
            filters: Object.values(draft).some(Boolean) ? draft : undefined,
          });
        }}
        onOpenFilters={() => setDraft({ ...filters })}
        onResetFilters={resetFilters}
        title="Runs"
      />

      {query.state.status === "stale" && query.state.error && (
        <div className="run-stale-state run-stale-state-error" role="alert">
          <span>
            <strong>Run history may be stale</strong>
            <small>{query.state.error.message}</small>
          </span>
          <button className="text-button" onClick={query.retry} type="button">
            Retry
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
                href={routeHash({ page: "runs", filters: { status: "all" } })}
                onClick={() => {
                  updateFilters({ showNoWork: true });
                }}
              >
                Clear all filters
              </a>
              {instanceRoot && <RecoveryCommand command={statusCommand(instanceRoot)} />}
            </div>
          ) : (
            <div className="inline-empty inline-empty-recovery">
              <strong>No runs recorded</strong>
              <span>Start a configured workflow to create the first run journal.</span>
              {instanceRoot && (
                <RecoveryCommand
                  command={manualRunCommand("<gaggle>", "<workflow>", instanceRoot)}
                />
              )}
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

function runsFilterError(
  filters: RunRouteFilters | undefined,
  gaggles: { name: string }[],
  workflows: { gaggle: string; name: string }[],
  inspectRoute = true,
): string | undefined {
  const search = new URLSearchParams(window.location.hash.split("?")[1] ?? "");
  const rawStatus = search.get("status");
  if (inspectRoute && rawStatus && !FILTERS.includes(rawStatus as RunsFilter)) {
    return `Invalid status filter "${rawStatus}". Choose active, attention, complete, or all.`;
  }
  if (filters?.gaggle && !gaggles.some((option) => option.name === filters.gaggle)) {
    return `Invalid gaggle filter "${filters.gaggle}". Choose a configured gaggle.`;
  }
  if (
    filters?.workflow &&
    !workflows.some((option) =>
      option.name === filters.workflow && (!filters.gaggle || option.gaggle === filters.gaggle))
  ) {
    return `Invalid workflow filter "${filters.workflow}". Choose a configured workflow.`;
  }
  return undefined;
}

function RunHistoryRow({ run }: { run: RunSummary }) {
  const workItem = run.operator?.issue;
  const identity = workItem
    ? `#${workItem.number}${workItem.title ? ` · ${workItem.title}` : ""}`
    : run.id;
  const context = `${run.gaggle} / ${run.workflow}${run.trigger.ref ? ` · ${run.trigger.ref}` : ""}${
    workItem ? ` · ${run.id}` : ""
  }`;

  return (
    <DataRow
      focusRestoreKey={`run-${run.id}`}
      href={routeHash({ page: "run", id: run.id })}
      label={`Open run ${run.id}`}
    >
      <span className="row-primary">
        <span className="row-title" title={identity}>
          {workItem ? (
            <>
              <span>#{workItem.number}</span>
              {workItem.title ? ` · ${workItem.title}` : ""}
            </>
          ) : (
            <span className="mono">{run.id}</span>
          )}
        </span>
        <span className="row-subtitle" title={context}>
          {run.gaggle} / {run.workflow}
          {run.trigger.ref ? ` · ${run.trigger.ref}` : ""}
          {workItem ? <span className="mono"> · {run.id}</span> : null}
        </span>
      </span>
      <StatusBadge stale={run.stale} status={run.phase} />
      <span
        aria-label={`Current stage: ${
          run.currentStage ?? (run.terminal ? "Terminal" : "Not started")
        }`}
        className="run-current-stage"
      >
        <span className="sr-only">Current stage: </span>
        {run.currentStage ?? (run.terminal ? "Terminal" : "Not started")}
      </span>
      <span>
        <time dateTime={run.startedAt}>{formatTimestamp(run.startedAt)}</time>
      </span>
      <RunTiming run={run} />
    </DataRow>
  );
}
