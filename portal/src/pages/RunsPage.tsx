import { FilterField, FilterOption, FilterOptions } from "../ui/Filters";
import { Action, ActionLink } from "../ui/Action";
import { Timestamp } from "../ui/Timestamp";
import { useEffect, useState } from "react";
import { RunTiming } from "../components/RunTiming";
import type { DaemonClient, RunSummary } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { RecoveryCommand } from "../components/RecoveryAction";
import { PageToolbar, type ActivePageFilter } from "../components/PageToolbar";
import { ScopeControl } from "../components/ScopeControl";
import {
  insightScopeApiParameters,
  insightScopeFromKey,
  insightScopeFromRoute,
  insightScopeKey,
  insightScopeOption,
} from "../insightScope";
import { manualRunCommand, statusCommand } from "../manualRunCommand";
import { runWorkTitle, useOperationalSnapshot } from "../operationalData";
import { routeHash, type Navigate, type RunRouteFilters } from "../routing";
import { scopeWindowLabel } from "../scope";
import { type RunsFilter, useRunsHistory } from "../runsHistory";
import { DataList, DataRow } from "../ui/DataList";
import { StatusBadge } from "../ui/StatusBadge";

const FILTERS: readonly RunsFilter[] = ["all", "active", "attention", "complete"];
const OUTCOMES = ["finished", "terminal", "success", "failure", "other"] as const;
const POPULATIONS = [
  "attempts",
  "measured",
  "token-measured",
  "premium-measured",
  "cost-measured",
  "retry-waste",
] as const;
const WINDOWS = ["24h", "7d", "30d", "all"] as const;
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
  const filterError = runsRouteFilterError();
  if (filterError) {
    return <InvalidRunsRoutePage filterError={filterError} filters={filters} navigate={navigate} />;
  }
  return (
    <RunsPageContent
      client={client}
      filters={filters}
      navigate={navigate}
      standalone={standalone}
    />
  );
}

function InvalidRunsRoutePage({
  filterError,
  filters,
  navigate,
}: {
  filterError: string;
  filters?: RunRouteFilters;
  navigate: Navigate;
}) {
  const [draft, setDraft] = useState<RunRouteFilters>({ ...filters });
  const clearInvalidFilters = () => navigate({ page: "runs" });
  const renderFilters = (mobile: boolean) => {
    const selectedStatus = mobile ? (draft.status ?? "all") : "all";
    return (
      <FilterOptions label="Filter runs" className="filter-bar">
        {FILTERS.map((option) => (
          <FilterOption
            selected={selectedStatus === option}
            key={option}
            onClick={() => {
              const status = option === "all" ? undefined : option;
              if (mobile) {
                setDraft({ ...filters, status });
                return;
              }
              navigate({ page: "runs", filters: status ? { status } : undefined });
            }}
            type="button"
          >
            {option === "all" ? "All runs" : option}
          </FilterOption>
        ))}
      </FilterOptions>
    );
  };

  return (
    <PageToolbar
      activeFilters={[]}
      count={0}
      description="Every execution across workflows and gaggles, filtered and paginated by the daemon."
      filterError={filterError}
      filters={renderFilters}
      onApplyFilters={() => {
        navigate({
          page: "runs",
          filters: Object.values(draft).some(Boolean) ? draft : undefined,
        });
      }}
      onOpenFilters={() => setDraft({ ...filters })}
      onResetFilters={clearInvalidFilters}
      title="Runs"
    />
  );
}

function RunsPageContent({
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
  const filter = filters?.status ?? "all";
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
    typeof window !== "undefined" && window.innerWidth <= 480 ? NARROW_RUNS_PAGE_SIZE : undefined;
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
  const workflowOptions = inventories.flatMap(({ gaggle, workflows }) =>
    workflows.map((workflow) => ({
      gaggle: gaggle.name,
      name: workflow.identity.name,
      label: workflow.displayName || workflow.identity.name,
    })),
  );
  const scopeOptions = [
    insightScopeOption({ kind: "instance" }),
    ...gaggleOptions.map(({ name }) => insightScopeOption({ kind: "gaggle", gaggle: name })),
    ...workflowOptions.map(({ gaggle, name }) =>
      insightScopeOption({ kind: "workflow", gaggle, workflow: name }),
    ),
  ];

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
    return (
      <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />
    );
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  const history = query.state.data;
  const filterError = runsFilterError(filters, gaggleOptions, workflowOptions);
  const activeFilters: ActivePageFilter[] = [
    ...(filters?.status
      ? [
          {
            key: "status",
            label: `Status: ${filters.status}`,
            onRemove: () => updateFilters({ status: undefined }),
          },
        ]
      : []),
    ...(filters?.gaggle
      ? [
          {
            key: "gaggle",
            label: `Gaggle: ${filters.gaggle}`,
            onRemove: () => updateFilters({ gaggle: undefined, workflow: undefined }),
          },
        ]
      : []),
    ...(filters?.workflow
      ? [
          {
            key: "workflow",
            label: `Workflow: ${filters.workflow}`,
            onRemove: () => updateFilters({ workflow: undefined }),
          },
        ]
      : []),
    ...(filters?.stage
      ? [
          {
            key: "stage",
            label: `Stage: ${filters.stage}`,
            onRemove: () => updateFilters({ stage: undefined }),
          },
        ]
      : []),
    ...(filters?.outcome
      ? [
          {
            key: "outcome",
            label: `Outcome: ${filterValueLabel(filters.outcome)}`,
            onRemove: () => updateFilters({ outcome: undefined }),
          },
        ]
      : []),
    ...(filters?.population
      ? [
          {
            key: "population",
            label: `Population: ${filterValueLabel(filters.population)}`,
            onRemove: () => updateFilters({ population: undefined }),
          },
        ]
      : []),
    ...(filters?.since
      ? [
          {
            key: "since",
            label: `Since: ${filters.since}`,
            onRemove: () => updateFilters({ since: undefined }),
          },
        ]
      : []),
    ...(filters?.until
      ? [
          {
            key: "until",
            label: `Until: ${filters.until}`,
            onRemove: () => updateFilters({ until: undefined }),
          },
        ]
      : []),
    ...(filters?.window
      ? [
          {
            key: "window",
            label: `Time window: ${windowLabel(filters.window)}`,
            onRemove: () => updateFilters({ window: undefined }),
          },
        ]
      : []),
    ...(showNoWork
      ? [
          {
            key: "no-work",
            label: "Includes no-work runs",
            onRemove: () => updateFilters({ showNoWork: undefined }),
          },
        ]
      : []),
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
    const selectedStatus = values.status ?? "all";
    return (
      <FilterOptions label="Filter runs" className="filter-bar">
        {mobile && (
          <ScopeControl
            ariaLabel="Draft scope"
            onChange={(key) => {
              const selected = insightScopeApiParameters(insightScopeFromKey(key));
              change({
                gaggle: selected.gaggle,
                workflow: selected.workflow,
                stage: selected.stage,
              });
            }}
            scopes={[...scopeOptions, insightScopeOption(insightScopeFromRoute(values))]}
            searchPlaceholder="Find gaggle or workflow"
            value={insightScopeKey(insightScopeFromRoute(values))}
          />
        )}
        {FILTERS.map((option) => (
          <FilterOption
            selected={selectedStatus === option}
            key={option}
            onClick={() => change({ status: option === "all" ? undefined : option })}
            type="button"
          >
            {option === "all" ? "All runs" : option}
          </FilterOption>
        ))}
        <div className="run-filter-fields">
          <FilterField kind="toggle" className="filter-toggle run-filter-field">
            <input
              aria-label={mobile ? "Draft show no-work runs" : "Show no-work runs"}
              checked={values.showNoWork ?? false}
              onChange={(event) => change({ showNoWork: event.target.checked || undefined })}
              type="checkbox"
            />
            Show no-work runs
          </FilterField>
          {mobile && (
            <>
              <FilterField kind="select" label="Stage" className="filter-select run-filter-field">
                <input
                  aria-label="Draft stage filter"
                  onChange={(event) => change({ stage: event.target.value || undefined })}
                  placeholder="Any stage"
                  type="text"
                  value={values.stage ?? ""}
                />
              </FilterField>
              <FilterField kind="select" label="Outcome" className="filter-select run-filter-field">
                <select
                  aria-label="Draft outcome filter"
                  onChange={(event) =>
                    change({
                      outcome: event.target.value
                        ? (event.target.value as RunRouteFilters["outcome"])
                        : undefined,
                    })
                  }
                  value={values.outcome ?? ""}
                >
                  <option value="">Any outcome</option>
                  {OUTCOMES.map((outcome) => (
                    <option key={outcome} value={outcome}>
                      {filterValueLabel(outcome)}
                    </option>
                  ))}
                </select>
              </FilterField>
              <FilterField
                kind="select"
                label="Population"
                className="filter-select run-filter-field"
              >
                <select
                  aria-label="Draft population filter"
                  onChange={(event) =>
                    change({
                      population: event.target.value
                        ? (event.target.value as RunRouteFilters["population"])
                        : undefined,
                    })
                  }
                  value={values.population ?? ""}
                >
                  <option value="">Any population</option>
                  {POPULATIONS.map((population) => (
                    <option key={population} value={population}>
                      {filterValueLabel(population)}
                    </option>
                  ))}
                </select>
              </FilterField>
              <FilterField kind="select" label="Since" className="filter-select run-filter-field">
                <input
                  aria-label="Draft since filter"
                  onChange={(event) => change({ since: event.target.value || undefined })}
                  placeholder="RFC3339 timestamp"
                  type="text"
                  value={values.since ?? ""}
                />
              </FilterField>
              <FilterField kind="select" label="Until" className="filter-select run-filter-field">
                <input
                  aria-label="Draft until filter"
                  onChange={(event) => change({ until: event.target.value || undefined })}
                  placeholder="RFC3339 timestamp"
                  type="text"
                  value={values.until ?? ""}
                />
              </FilterField>
              <FilterField
                kind="select"
                label="Time window"
                className="filter-select run-filter-field"
              >
                <select
                  aria-label="Draft time window filter"
                  onChange={(event) =>
                    change({
                      window: event.target.value
                        ? (event.target.value as RunRouteFilters["window"])
                        : undefined,
                    })
                  }
                  value={values.window ?? ""}
                >
                  <option value="">Any time</option>
                  {WINDOWS.map((window) => (
                    <option key={window} value={window}>
                      {windowLabel(window)}
                    </option>
                  ))}
                </select>
              </FilterField>
            </>
          )}
        </div>
      </FilterOptions>
    );
  };

  return (
    <>
      <PageToolbar
        activeFilters={activeFilters}
        count={history.runs.length}
        description={
          filters
            ? `Executions behind the selected Insight scope${scopeWindowLabel(filters)}.`
            : standalone
              ? "Every execution recorded in this instance, filtered and paginated by the read service."
              : "Every execution across workflows and gaggles, filtered and paginated by the daemon."
        }
        filterError={filterError}
        filters={renderFilters}
        onApplyFilters={() => {
          navigate({
            page: "runs",
            filters: Object.values(draft).some(Boolean) ? draft : undefined,
          });
        }}
        onOpenFilters={() => setDraft({ ...filters })}
        onResetFilters={resetFilters}
        onValidateFilters={() => runsFilterError(draft, gaggleOptions, workflowOptions)}
        scope={
          <ScopeControl
            onChange={(key) => {
              const selected = insightScopeApiParameters(insightScopeFromKey(key));
              updateFilters({
                gaggle: selected.gaggle,
                workflow: selected.workflow,
                stage: selected.stage,
              });
            }}
            scopes={[...scopeOptions, insightScopeOption(insightScopeFromRoute(filters))]}
            searchPlaceholder="Find gaggle or workflow"
            value={insightScopeKey(insightScopeFromRoute(filters))}
          />
        }
        title="Runs"
      />

      {query.state.status === "stale" && query.state.error && (
        <div className="run-stale-state run-stale-state-error" role="alert">
          <span>
            <strong>Run history may be stale</strong>
            <small>{query.state.error.message}</small>
          </span>
          <Action variant="text" className="text-button" onClick={query.retry} type="button">
            Retry
          </Action>
        </div>
      )}

      <section className="content-section">
        {history.runs.length === 0 ? (
          history.hasAnyRuns ? (
            <div className="inline-empty inline-empty-recovery">
              <strong>Filters exclude existing runs</strong>
              <span>Clear the current filters to return to the complete run history.</span>
              <ActionLink
                variant="text"
                className="text-button"
                href={routeHash({ page: "runs", filters: { status: "all" } })}
                onClick={() => {
                  updateFilters({ showNoWork: true });
                }}
              >
                Clear all filters
              </ActionLink>
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
                <Action
                  variant="text"
                  className="text-button"
                  disabled={history.loadingMore}
                  onClick={query.loadMore}
                  type="button"
                >
                  {history.loadingMore ? "Loading…" : "Load more runs"}
                </Action>
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
): string | undefined {
  if (filters?.gaggle && !gaggles.some((option) => option.name === filters.gaggle)) {
    return `Invalid gaggle filter "${filters.gaggle}". Choose a configured gaggle.`;
  }
  if (
    filters?.workflow &&
    !workflows.some(
      (option) =>
        option.name === filters.workflow && (!filters.gaggle || option.gaggle === filters.gaggle),
    )
  ) {
    return `Invalid workflow filter "${filters.workflow}". Choose a configured workflow.`;
  }
  for (const name of ["since", "until"] as const) {
    const value = filters?.[name];
    if (value && !validTimestamp(value)) {
      return `Invalid ${name} filter "${value}". Enter an RFC3339 timestamp.`;
    }
  }
  return undefined;
}

function filterValueLabel(value: string): string {
  return value
    .split("-")
    .map((part) => `${part.charAt(0).toUpperCase()}${part.slice(1)}`)
    .join(" ");
}

function windowLabel(window: RunRouteFilters["window"]): string {
  switch (window) {
    case "24h":
      return "Last 24 hours";
    case "7d":
      return "Last 7 days";
    case "30d":
      return "Last 30 days";
    case "all":
      return "All time";
    default:
      return "";
  }
}

function runsRouteFilterError(): string | undefined {
  const search = new URLSearchParams(window.location.hash.split("?")[1] ?? "");
  const rawStatus = search.get("status");
  if (search.has("status") && !FILTERS.includes(rawStatus as RunsFilter)) {
    return `Invalid status filter "${rawStatus}". Choose active, attention, complete, or all.`;
  }
  const rawOutcome = search.get("outcome");
  if (search.has("outcome") && !OUTCOMES.includes(rawOutcome as (typeof OUTCOMES)[number])) {
    return `Invalid outcome filter "${rawOutcome}".`;
  }
  const rawPopulation = search.get("population");
  if (
    search.has("population") &&
    !POPULATIONS.includes(rawPopulation as (typeof POPULATIONS)[number])
  ) {
    return `Invalid population filter "${rawPopulation}".`;
  }
  const rawWindow = search.get("window");
  if (search.has("window") && !WINDOWS.includes(rawWindow as (typeof WINDOWS)[number])) {
    return `Invalid time window filter "${rawWindow}". Choose 24h, 7d, 30d, or all.`;
  }
  for (const name of ["since", "until"] as const) {
    const value = search.get(name);
    if (search.has(name) && !validTimestamp(value)) {
      return `Invalid ${name} filter "${value}". Enter an RFC3339 timestamp.`;
    }
  }
  const rawShowNoWork = search.get("showNoWork");
  if (search.has("showNoWork") && rawShowNoWork !== "1") {
    return `Invalid show no-work filter "${rawShowNoWork}".`;
  }
  return undefined;
}

function validTimestamp(value: string | null): boolean {
  const match = value?.match(
    /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(?:Z|[+-](\d{2}):(\d{2}))$/,
  );
  if (!match) return false;
  const [, year, month, day, hour, minute, second, zoneHour, zoneMinute] = match;
  const numericYear = Number(year);
  const numericMonth = Number(month);
  const daysInMonth = [
    31,
    numericYear % 4 === 0 && (numericYear % 100 !== 0 || numericYear % 400 === 0) ? 29 : 28,
    31,
    30,
    31,
    30,
    31,
    31,
    30,
    31,
    30,
    31,
  ];
  return (
    numericMonth >= 1 &&
    numericMonth <= 12 &&
    Number(day) >= 1 &&
    Number(day) <= (daysInMonth[numericMonth - 1] ?? 0) &&
    Number(hour) <= 23 &&
    Number(minute) <= 59 &&
    Number(second) <= 59 &&
    (zoneHour === undefined || Number(zoneHour) <= 23) &&
    (zoneMinute === undefined || Number(zoneMinute) <= 59)
  );
}

function RunHistoryRow({ run }: { run: RunSummary }) {
  const workItem = run.operator?.issue;
  const displayTitle = run.operator?.displayTitle;
  const titled = Boolean(displayTitle || workItem);
  const identity = runWorkTitle(run, " · ") ?? run.id;
  const context = `${run.gaggle} / ${run.workflow}${run.trigger.ref ? ` · ${run.trigger.ref}` : ""}${
    titled ? ` · ${run.id}` : ""
  }`;

  return (
    <DataRow
      focusRestoreKey={`run-${run.id}`}
      href={routeHash({ page: "run", id: run.id })}
      label={`Open run ${run.id}`}
    >
      <span className="row-primary">
        <span className="row-title" title={identity}>
          {displayTitle ? (
            <span>{displayTitle}</span>
          ) : workItem ? (
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
          {titled ? <span className="mono"> · {run.id}</span> : null}
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
        <Timestamp value={run.startedAt} />
      </span>
      <RunTiming run={run} />
    </DataRow>
  );
}
