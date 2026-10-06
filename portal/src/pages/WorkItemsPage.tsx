import { PageHeading } from "../ui/Heading";
import { Action, ActionLink } from "../ui/Action";
import { FilterField, FilterOption, FilterOptions } from "../ui/Filters";
import { Timestamp } from "../ui/Timestamp";
import { TableHeader, TableShell } from "../ui/DataTable";
import { useEffect, useState } from "react";
import type {
  DaemonClient,
  WorkItemDetail,
  WorkItemKind,
  WorkItemOutcome,
  WorkItemPage,
  WorkItemSummary,
} from "../api/types";
import { DaemonApiError, MissingCapabilityError } from "../api/errors";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { PageToolbar, type ActivePageFilter } from "../components/PageToolbar";
import { ScopeControl } from "../components/ScopeControl";
import { insightScopeFromKey, insightScopeKey, insightScopeOption } from "../insightScope";
import type { Navigate, Route } from "../routing";
import { routeHash } from "../routing";
import { Icon } from "../ui/Icon";

type PageState<T> =
  | { status: "loading" }
  | { status: "error"; error: Error }
  | { status: "ready"; data: T };

type WorkItemFilters = {
  kind?: WorkItemKind;
  gaggle?: string;
  outcome?: WorkItemOutcome;
};

export function WorkItemsPage({
  client,
  navigate,
  route,
  standalone,
}: {
  client: DaemonClient;
  navigate: Navigate;
  route: Extract<Route, { page: "work-items" }>;
  standalone: boolean;
}) {
  if (route.provider && route.repository && route.kind && route.id) {
    return (
      <WorkItemDetailView
        client={client}
        externalId={route.id}
        kind={route.kind}
        navigate={navigate}
        provider={route.provider}
        repository={route.repository}
        standalone={standalone}
      />
    );
  }
  return (
    <WorkItemListView
      client={client}
      gaggle={route.gaggle}
      kind={route.kind}
      navigate={navigate}
      outcome={route.outcome}
      query={route.query}
      standalone={standalone}
    />
  );
}

function WorkItemListView({
  client,
  gaggle,
  kind,
  navigate,
  outcome,
  query,
  standalone,
}: {
  client: DaemonClient;
  gaggle?: string;
  kind?: WorkItemKind;
  navigate: Navigate;
  outcome?: WorkItemOutcome;
  query?: string;
  standalone: boolean;
}) {
  const [state, setState] = useState<
    PageState<{
      page: WorkItemPage;
      filterItems: WorkItemSummary[];
    }>
  >({ status: "loading" });
  const [searchQuery, setSearchQuery] = useState(query ?? "");
  const [draft, setDraft] = useState<WorkItemFilters>({ kind, gaggle, outcome });
  const load = () => {
    const controller = new AbortController();
    setState({ status: "loading" });
    const pullRequests = client.listWorkItems(
      { kind: "pr", limit: 200 },
      { signal: controller.signal },
    );
    const issues = client.listWorkItems(
      { kind: "issue", limit: 200 },
      { signal: controller.signal },
    );
    const page =
      gaggle
        ? client.listWorkItems({ kind, gaggle, limit: 200 }, { signal: controller.signal })
        : kind === "pr"
        ? pullRequests
        : kind === "issue"
          ? issues
          : client.listWorkItems({ limit: 200 }, { signal: controller.signal });
    Promise.all([page, pullRequests, issues]).then(
      ([data, pullRequestData, issueData]) =>
        setState({
          status: "ready",
          data: {
            page: data,
            filterItems: [...pullRequestData.items, ...issueData.items],
          },
        }),
      (error: Error) => {
        if (!controller.signal.aborted) setState({ status: "error", error });
      },
    );
    return () => controller.abort();
  };

  useEffect(load, [client, gaggle, kind]);
  useEffect(() => setSearchQuery(query ?? ""), [query]);
  useEffect(() => setDraft({ kind, gaggle, outcome }), [kind, gaggle, outcome]);

  if (state.status === "loading") return <DaemonLoadingState standalone={standalone} />;
  if (state.status === "error") {
    if (isMissingWorkItemsCapability(state.error)) {
      return (
        <section className="daemon-state daemon-state-error" role="alert">
          <div>
            <h1>Work Items unavailable</h1>
            <p>This daemon does not support work item history. Upgrade Goobers to use this page.</p>
          </div>
          <Action variant="primary" className="reconnect-button" onClick={load} type="button">
            Retry
          </Action>
        </section>
      );
    }
    return <DaemonErrorState error={state.error} retry={load} standalone={standalone} />;
  }

  const gaggleOptions = (selectedKind?: WorkItemKind) =>
    [
      ...new Set(
        state.data.filterItems
          .filter((item) => !selectedKind || item.kind === selectedKind)
          .map((item) => item.gaggle)
          .filter((value): value is string => Boolean(value)),
      ),
    ].sort((left, right) => left.localeCompare(right));
  const normalizedQuery = searchQuery.trim().toLocaleLowerCase();
  const items = state.data.page.items.filter((item) => {
    if (gaggle && item.gaggle !== gaggle) return false;
    if (outcome && item.outcome !== outcome) return false;
    if (!normalizedQuery) return true;
    return [workItemLabel(item.repository, item.externalId), item.repository, item.externalId].some(
      (value) => value?.toLocaleLowerCase().includes(normalizedQuery),
    );
  });
  const updateFilters = (updates: WorkItemFilters & { query?: string }) => {
    navigate({
      page: "work-items",
      kind,
      gaggle,
      outcome,
      query: searchQuery || undefined,
      ...updates,
    });
  };
  const updateSearch = (value: string) => {
    setSearchQuery(value);
    const hash = routeHash({
      page: "work-items",
      kind,
      gaggle,
      outcome,
      query: value || undefined,
    });
    window.history.replaceState(window.history.state, "", hash);
  };
  const filterError = workItemFilterError(kind, gaggle, gaggleOptions(kind));
  const activeFilters: ActivePageFilter[] = [
    ...(kind
      ? [
          {
            key: "kind",
            label: kind === "pr" ? "Pull requests" : "Issues",
            onRemove: () => updateFilters({ kind: undefined }),
          },
        ]
      : []),
    ...(gaggle
      ? [
          {
            key: "gaggle",
            label: `Gaggle: ${gaggle}`,
            onRemove: () => updateFilters({ gaggle: undefined }),
          },
        ]
      : []),
    ...(outcome
      ? [
          {
            key: "outcome",
            label: `Status: ${workItemOutcomeLabel(outcome)}`,
            onRemove: () => updateFilters({ outcome: undefined }),
          },
        ]
      : []),
  ];
  const renderFilters = (mobile: boolean) => {
    const values = mobile ? draft : { kind, gaggle, outcome };
    const change = (updates: WorkItemFilters) => {
      if (mobile) {
        setDraft((current) => ({ ...current, ...updates }));
      } else {
        updateFilters(updates);
      }
    };
    return (
      <FilterOptions label="Work item filters" className="filter-bar">
        {mobile && (
          <ScopeControl
            ariaLabel="Draft scope"
            onChange={(key) => {
              const selected = insightScopeFromKey(key);
              change({ gaggle: selected.kind === "gaggle" ? selected.gaggle : undefined });
            }}
            scopes={[
              insightScopeOption({ kind: "instance" }),
              ...gaggleOptions(values.kind).map((name) =>
                insightScopeOption({ kind: "gaggle", gaggle: name }),
              ),
            ]}
            searchPlaceholder="Find gaggle"
            value={insightScopeKey(
              values.gaggle ? { kind: "gaggle", gaggle: values.gaggle } : { kind: "instance" },
            )}
          />
        )}
        {(
          [
            ["all", undefined],
            ["pull requests", "pr"],
            ["issues", "issue"],
          ] as const
        ).map(([label, value]) => (
          <FilterOption
            selected={values.kind === value}
            key={label}
            onClick={() => change({ kind: value })}
            type="button"
          >
            {label}
          </FilterOption>
        ))}
        <div className="work-item-filter-controls">
          <FilterField kind="select" className="filter-select work-item-filter-field">
            <select
              aria-label={mobile ? "Draft work item status filter" : "Filter work items by status"}
              onChange={(event) =>
                change({
                  outcome: workItemOutcomeFilter(event.target.value),
                })
              }
              value={values.outcome ?? ""}
            >
              <option value="">All statuses</option>
              <option value="done">Done</option>
              <option value="in-progress">In progress</option>
              <option value="bad-terminal">Error</option>
            </select>
          </FilterField>
        </div>
      </FilterOptions>
    );
  };

  return (
    <>
      <PageToolbar
        className="work-items-toolbar"
        inlineSearch
        activeFilters={activeFilters}
        count={items.length}
        description="Pull requests, issues, and work items changed through a recorded provider operation."
        filterError={filterError}
        filters={renderFilters}
        onApplyFilters={() => {
          updateFilters(draft);
        }}
        onOpenFilters={() => setDraft({ kind, gaggle, outcome })}
        onResetFilters={() =>
          navigate({
            page: "work-items",
            query: searchQuery || undefined,
          })
        }
        onValidateFilters={() =>
          workItemFilterError(draft.kind, draft.gaggle, gaggleOptions(draft.kind), false)
        }
        search={
          <FilterField kind="search" label="Filter" className="filter-search page-toolbar-search">
            <input
              aria-label="Search work items"
              onChange={(event) => updateSearch(event.target.value)}
              placeholder="Repository or number"
              type="search"
              value={searchQuery}
            />
          </FilterField>
        }
        scope={
          <ScopeControl
            onChange={(key) => {
              const selected = insightScopeFromKey(key);
              updateFilters({ gaggle: selected.kind === "gaggle" ? selected.gaggle : undefined });
            }}
            scopes={[
              insightScopeOption({ kind: "instance" }),
              ...[...new Set([...gaggleOptions(kind), ...(gaggle ? [gaggle] : [])])].map((name) =>
                insightScopeOption({ kind: "gaggle", gaggle: name }),
              ),
            ]}
            searchPlaceholder="Find gaggle"
            value={insightScopeKey(gaggle ? { kind: "gaggle", gaggle } : { kind: "instance" })}
          />
        }
        title="Work Items"
      />
      <section className="content-section work-items-content">
        {items.length === 0 ? (
          <p className="inline-empty">
            No confirmed provider actions match this filter. This page lists only provider changes
            Goobers recorded; work created or changed outside a recorded provider operation may
            still exist in the provider.
          </p>
        ) : (
          <TableShell ariaLabel="Work items" className="data-table work-items-table">
            <TableHeader
              columns={["Work item", "Last action", "Gaggle", "Actions"]}
              className="work-item-grid"
              trailingColumn
            />
            {items.map((item) => {
              const label = workItemLabel(item.repository, item.externalId);
              return (
                <button
                  aria-label={
                    item.repository
                      ? `Open ${workItemShortKind(item)} #${item.externalId} in ${item.repository}`
                      : `${workItemShortKind(item)} #${item.externalId} has no recorded repository`
                  }
                  className={`data-row work-item-grid work-item-row-${item.outcome}`}
                  data-status={item.outcome}
                  disabled={!item.repository}
                  title={
                    item.repository
                      ? undefined
                      : "No repository was recorded for this item, so it has no detail page."
                  }
                  key={workItemRowKey(item)}
                  onClick={() =>
                    item.repository &&
                    navigate({
                      page: "work-items",
                      provider: item.provider,
                      repository: item.repository,
                      kind: item.kind,
                      id: item.externalId,
                    })
                  }
                  type="button"
                >
                  <span className="work-item-identity">
                    <strong className="data-table-primary" title={label}>
                      {label}
                    </strong>
                    <small className="data-table-meta">
                      {item.provider} · {workItemKindLabel(item.provider, item.kind)}
                    </small>
                    <span className="sr-only">{workItemOutcomeLabel(item.outcome, item.kind)}</span>
                  </span>
                  <span className="work-item-last-action">
                    <strong>{humanizeOperation(item.lastOperation)}</strong>
                    <small>
                      <Timestamp value={item.lastActionAt} />
                    </small>
                    <small>{item.workflow || "Unknown workflow"}</small>
                  </span>
                  <span className="work-item-gaggle">
                    <strong>{item.gaggle || "No gaggle recorded"}</strong>
                  </span>
                  <strong className="work-item-action-count">{item.actionCount}</strong>
                  <span className="work-item-mobile-context">
                    <span>
                      Last action: {humanizeOperation(item.lastOperation)} · {item.actionCount}{" "}
                      {item.actionCount === 1 ? "action" : "actions"}
                    </span>
                    <Timestamp value={item.lastActionAt} />
                    <span>
                      {item.gaggle || "Unknown gaggle"} / {item.workflow || "Unknown workflow"}
                    </span>
                  </span>
                  {item.repository && (
                    <span className="row-arrow">
                      <Icon name="chevron" size={15} />
                    </span>
                  )}
                </button>
              );
            })}
            {state.data.page.hasMore && (
              <p className="data-overflow">Showing the 200 most recently actioned work items.</p>
            )}
          </TableShell>
        )}
      </section>
    </>
  );
}

function workItemFilterError(
  kind: WorkItemKind | undefined,
  gaggle: string | undefined,
  gaggles: string[],
  inspectRoute = true,
): string | undefined {
  const search = new URLSearchParams(window.location.hash.split("?")[1] ?? "");
  const rawKind = search.get("kind");
  if (inspectRoute && search.has("kind") && rawKind !== "pr" && rawKind !== "issue") {
    return `Invalid work item type "${rawKind}". Choose pull requests or issues.`;
  }
  const rawOutcome = search.get("outcome");
  if (
    inspectRoute &&
    search.has("outcome") &&
    rawOutcome !== "done" &&
    rawOutcome !== "in-progress" &&
    rawOutcome !== "bad-terminal"
  ) {
    return `Invalid work item status "${rawOutcome}". Choose done, in progress, or bad terminal.`;
  }
  if (kind && kind !== "pr" && kind !== "issue") {
    return `Invalid work item type "${kind}". Choose pull requests or issues.`;
  }
  if (gaggle && !gaggles.includes(gaggle)) {
    return `Invalid gaggle filter "${gaggle}". Choose a recorded gaggle.`;
  }
  return undefined;
}

function WorkItemDetailView({
  client,
  externalId,
  kind,
  navigate,
  provider,
  repository,
  standalone,
}: {
  client: DaemonClient;
  externalId: string;
  kind: WorkItemKind;
  navigate: Navigate;
  provider: string;
  repository: string;
  standalone: boolean;
}) {
  const [state, setState] = useState<PageState<WorkItemDetail>>({ status: "loading" });
  const [actionType, setActionType] = useState("all");
  const load = () => {
    const controller = new AbortController();
    setState({ status: "loading" });
    client.getWorkItem(provider, repository, kind, externalId, { signal: controller.signal }).then(
      (data) => setState({ status: "ready", data }),
      (error: Error) => {
        if (!controller.signal.aborted) setState({ status: "error", error });
      },
    );
    return () => controller.abort();
  };
  useEffect(load, [client, provider, repository, kind, externalId]);

  if (state.status === "loading") return <DaemonLoadingState standalone={standalone} />;
  if (state.status === "error") {
    return <DaemonErrorState error={state.error} retry={load} standalone={standalone} />;
  }
  const item = state.data;
  const actionTypes = [...new Set(item.actions.map((action) => action.operation))].sort(
    (left, right) => humanizeOperation(left).localeCompare(humanizeOperation(right)),
  );
  const visibleActions =
    actionType === "all"
      ? item.actions
      : item.actions.filter((action) => action.operation === actionType);
  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumbs">
        <button onClick={() => navigate({ page: "work-items", kind })} type="button">
          Work Items
        </button>
        <Icon name="chevron" size={14} />
        <span>{workItemLabel(repository, externalId)}</span>
      </nav>
      <PageHeading
        title={workItemLabel(repository, externalId)}
        className=""
        beforeTitle={
          <>
            <p className="page-kicker">
              {provider} {workItemKindLabel(provider, kind)} activity
            </p>
          </>
        }
      >
        <div className="work-item-summary">
          <span>
            <small>Confirmed actions</small>
            <strong>{item.actions.length}</strong>
          </span>
          <span>
            <small>Attributed cost to date</small>
            <strong>{formatWorkItemCost(item.cost)}</strong>
            {item.cost?.lowerBound && <em>Lower bound; some usage is unmeasured</em>}
          </span>
        </div>
        <div className="page-heading-actions">
          {item.url && (
            <a
              className="scope-pivot-link work-item-heading-link"
              href={item.url}
              rel="noreferrer"
              target="_blank"
            >
              <Icon name="arrow" size={14} />
              Open {workItemKindLabel(provider, kind)}
            </a>
          )}
          {item.relatedPullRequests.map((related) => (
            <a
              aria-label={`Open related PR ${workItemLabel(related.repository, related.externalId)}`}
              className="scope-pivot-link work-item-heading-link"
              href={
                related.url ??
                routeHash({
                  page: "work-items",
                  provider: related.provider,
                  repository: related.repository,
                  kind: "pr",
                  id: related.externalId,
                })
              }
              key={workItemRowKey(related)}
              rel={related.url ? "noreferrer" : undefined}
              target={related.url ? "_blank" : undefined}
            >
              <Icon name="arrow" size={14} />
              Related PR {workItemLabel(related.repository, related.externalId)}
            </a>
          ))}
        </div>
      </PageHeading>
      <section className="content-section">
        <FilterOptions
          label="Work item action filters"
          className="filter-bar work-item-action-filters"
        >
          <FilterField kind="select" label="Action type" className="filter-select">
            <select
              aria-label="Filter actions by type"
              onChange={(event) => setActionType(event.target.value)}
              value={actionType}
            >
              <option value="all">All actions</option>
              {actionTypes.map((operation) => (
                <option key={operation || "provider-action"} value={operation}>
                  {humanizeOperation(operation)}
                </option>
              ))}
            </select>
          </FilterField>
        </FilterOptions>
        <div
          aria-label={`Action history for ${workItemLabel(repository, externalId)}`}
          className="data-table-shell work-item-actions-table"
          role="table"
        >
          <div className="data-table-header work-item-action-grid" role="row">
            <span role="columnheader">Action</span>
            <span role="columnheader">Gaggle / workflow</span>
            <span role="columnheader">Status</span>
            <span role="columnheader">Time</span>
            <span role="columnheader">Run</span>
          </div>
          {visibleActions.map((action) => (
            <div
              className="work-item-action-grid work-item-action-row"
              key={`${action.runId}:${action.sequence}`}
              role="row"
            >
              <span className="work-item-action-cell" role="cell">
                <strong className="data-table-primary">
                  {humanizeOperation(action.operation)}
                </strong>
                <small className="data-table-meta">Sequence {action.sequence}</small>
              </span>
              <span className="work-item-action-cell" role="cell">
                <strong>{action.gaggle || "Unknown gaggle"}</strong>
                <small className="data-table-meta">
                  {action.workflow || "Workflow unavailable"}
                </small>
              </span>
              <span role="cell">
                {action.runStatus ? humanizeOperation(action.runStatus) : "Unknown"}
              </span>
              <Timestamp value={action.occurredAt} role="cell" />
              <span role="cell">
                <ActionLink
                  variant="text"
                  size="compact"
                  className="text-button run-link-action"
                  href={routeHash({ page: "run", id: action.runId })}
                >
                  View run
                </ActionLink>
              </span>
            </div>
          ))}
          {visibleActions.length === 0 && (
            <p className="data-overflow" role="status">
              No actions match this type.
            </p>
          )}
          {item.truncated && <p className="data-overflow">Showing the 200 most recent actions.</p>}
        </div>
      </section>
    </>
  );
}

/** A row key that keeps every identity the read model partitions by, so equal
 *  numeric ids in different providers, repositories, or projects (or with
 *  different recorded URLs when the repository is unknown) never collide. */
function workItemRowKey(
  item: Pick<WorkItemSummary, "provider" | "repository" | "kind" | "externalId" | "url">,
): string {
  return JSON.stringify([
    item.provider,
    item.repository ?? "",
    item.kind,
    item.externalId,
    item.url ?? "",
  ]);
}

/** Azure Boards tracks native work items, not GitHub-style issues. */
function workItemKindLabel(provider: string, kind: WorkItemKind): string {
  if (kind === "pr") return "pull request";
  return provider.toLowerCase() === "ado" ? "work item" : "issue";
}

function workItemShortKind(item: Pick<WorkItemSummary, "provider" | "kind">): string {
  return item.kind === "pr" ? "PR" : workItemKindLabel(item.provider, item.kind);
}

function workItemLabel(repository: string | undefined, externalId: string): string {
  return repository ? `${repository}#${externalId}` : `#${externalId}`;
}

function workItemOutcomeLabel(outcome: WorkItemOutcome, kind?: WorkItemKind): string {
  switch (outcome) {
    case "done":
      return kind === "pr" ? "Merged" : kind === "issue" ? "Closed" : "Done";
    case "bad-terminal":
      return "Error";
    case "in-progress":
      return "In progress";
  }
}

function workItemOutcomeFilter(value: string): WorkItemOutcome | undefined {
  return value === "done" || value === "in-progress" || value === "bad-terminal"
    ? value
    : undefined;
}

function formatWorkItemCost(cost: WorkItemDetail["cost"]): string {
  if (!cost) return "Not attributed";
  if (cost.nanoAIU !== undefined) {
    return `${new Intl.NumberFormat("en-US", { maximumFractionDigits: 4 }).format(cost.nanoAIU / 1_000_000_000)} AIC`;
  }
  return "Not measured";
}

function humanizeOperation(operation: string): string {
  if (!operation) return "Provider action";
  return operation.replaceAll("-", " ").replace(/\b\w/g, (letter) => letter.toUpperCase());
}

function isMissingWorkItemsCapability(error: Error): boolean {
  return (
    (error instanceof MissingCapabilityError && error.capability === "work-items") ||
    (error instanceof DaemonApiError && (error.status === 404 || error.code === "not_found"))
  );
}
