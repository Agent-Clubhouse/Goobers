import { useEffect, useState } from "react";
import type {
  DaemonClient,
  WorkItemDetail,
  WorkItemKind,
  WorkItemPage,
} from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import type { Navigate, Route } from "../routing";
import { routeHash } from "../routing";
import { formatTimestamp } from "../runDetailData";
import { Icon } from "../ui/Icon";

type PageState<T> =
  | { status: "loading" }
  | { status: "error"; error: Error }
  | { status: "ready"; data: T };

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
  query,
  standalone,
}: {
  client: DaemonClient;
  gaggle?: string;
  kind?: WorkItemKind;
  navigate: Navigate;
  query?: string;
  standalone: boolean;
}) {
  const [state, setState] = useState<PageState<WorkItemPage>>({ status: "loading" });
  const [searchQuery, setSearchQuery] = useState(query ?? "");
  const load = () => {
    const controller = new AbortController();
    setState({ status: "loading" });
    client.listWorkItems({ kind, limit: 200 }, { signal: controller.signal }).then(
      (data) => setState({ status: "ready", data }),
      (error: Error) => {
        if (!controller.signal.aborted) setState({ status: "error", error });
      },
    );
    return () => controller.abort();
  };

  useEffect(load, [client, kind]);
  useEffect(() => setSearchQuery(query ?? ""), [query]);

  if (state.status === "loading") return <DaemonLoadingState standalone={standalone} />;
  if (state.status === "error") {
    return <DaemonErrorState error={state.error} retry={load} standalone={standalone} />;
  }

  const gaggleOptions = [...new Set(
    state.data.items.map((item) => item.gaggle).filter((value): value is string => Boolean(value)),
  )].sort((left, right) => left.localeCompare(right));
  const normalizedQuery = searchQuery.trim().toLocaleLowerCase();
  const items = state.data.items.filter((item) => {
    if (gaggle && item.gaggle !== gaggle) return false;
    if (!normalizedQuery) return true;
    return [
      workItemLabel(item.repository, item.externalId),
      item.repository,
      item.externalId,
    ].some((value) => value?.toLocaleLowerCase().includes(normalizedQuery));
  });
  const updateFilters = (updates: { kind?: WorkItemKind; gaggle?: string; query?: string }) => {
    navigate({
      page: "work-items",
      kind,
      gaggle,
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
      query: value || undefined,
    });
    window.history.replaceState(window.history.state, "", hash);
  };

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">External activity</p>
        <h1>Work Items</h1>
        <p>Pull requests and issues that Goobers changed through a provider operation.</p>
      </header>
      <div aria-label="Work item type" className="filter-bar">
        {([
          ["all", undefined],
          ["pull requests", "pr"],
          ["issues", "issue"],
        ] as const).map(([label, value]) => (
          <button
            className={kind === value ? "filter-button filter-button-active" : "filter-button"}
            key={label}
            onClick={() => updateFilters({ kind: value })}
            type="button"
          >
            {label}
          </button>
        ))}
        <div className="work-item-filter-fields">
          <label className="filter-select work-item-filter-field">
            <span>Gaggle</span>
            <select
              aria-label="Filter work items by gaggle"
              onChange={(event) => updateFilters({ gaggle: event.target.value || undefined })}
              value={gaggle ?? ""}
            >
              <option value="">All gaggles</option>
              {gaggleOptions.map((option) => (
                <option key={option} value={option}>{option}</option>
              ))}
            </select>
          </label>
          <label className="filter-search work-item-filter-field">
            <span>Find work item</span>
            <input
              aria-label="Search work items"
              onChange={(event) => updateSearch(event.target.value)}
              placeholder="Repository or number"
              type="search"
              value={searchQuery}
            />
          </label>
        </div>
      </div>
      <section className="content-section">
        {items.length === 0 ? (
          <p className="inline-empty">No confirmed provider actions match this filter.</p>
        ) : (
          <div className="data-table data-table-shell work-items-table">
            <div aria-hidden="true" className="data-header data-table-header work-item-grid">
              <span>Work item</span><span>Last action</span><span>Workflow</span><span>Actions</span><span />
            </div>
            {items.map((item) => (
              <button
                aria-label={`Open ${item.kind === "pr" ? "PR" : "issue"} #${item.externalId} in ${item.repository}`}
                className="data-row work-item-grid"
                key={`${item.provider}/${item.kind}/${item.externalId}`}
                onClick={() => navigate({
                  page: "work-items",
                  provider: item.provider,
                  repository: item.repository,
                  kind: item.kind,
                  id: item.externalId,
                })}
                type="button"
              >
                <span className="work-item-identity">
                  <strong className="data-table-primary">{workItemLabel(item.repository, item.externalId)}</strong>
                  <small className="data-table-meta">{item.provider} · {item.kind === "pr" ? "pull request" : "issue"}</small>
                </span>
                <span>
                  <strong>{humanizeOperation(item.lastOperation)}</strong>
                  <small>{formatTimestamp(item.lastActionAt)}</small>
                </span>
                <span>
                  <strong>{item.workflow || "Unknown"}</strong>
                  <small>{item.gaggle || "No gaggle recorded"}</small>
                </span>
                <strong>{item.actionCount}</strong>
                <Icon name="chevron" size={15} />
              </button>
            ))}
            {state.data.hasMore && (
              <p className="data-overflow">Showing the 200 most recently actioned work items.</p>
            )}
          </div>
        )}
      </section>
    </>
  );
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
  const actionTypes = [...new Set(item.actions.map((action) => action.operation))]
    .sort((left, right) => humanizeOperation(left).localeCompare(humanizeOperation(right)));
  const visibleActions = actionType === "all"
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
      <header className="page-heading">
        <p className="page-kicker">{provider} {kind === "pr" ? "pull request" : "issue"} activity</p>
        <h1>{workItemLabel(repository, externalId)}</h1>
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
              Open {kind === "pr" ? "pull request" : "issue"}
            </a>
          )}
          {item.relatedPullRequests.map((related) => (
            <a
              aria-label={`Open related PR ${workItemLabel(related.repository, related.externalId)}`}
              className="scope-pivot-link work-item-heading-link"
              href={related.url ?? routeHash({
                page: "work-items",
                provider: related.provider,
                repository: related.repository,
                kind: "pr",
                id: related.externalId,
              })}
              key={`${related.repository}/${related.externalId}`}
              rel={related.url ? "noreferrer" : undefined}
              target={related.url ? "_blank" : undefined}
            >
              <Icon name="arrow" size={14} />
              Related PR {workItemLabel(related.repository, related.externalId)}
            </a>
          ))}
        </div>
      </header>
      <section className="content-section">
        <div aria-label="Work item action filters" className="filter-bar work-item-action-filters" role="group">
          <label className="filter-select">
            <span>Action type</span>
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
          </label>
        </div>
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
            <div className="work-item-action-grid work-item-action-row" key={`${action.runId}:${action.sequence}`} role="row">
              <span className="work-item-action-cell" role="cell">
                <strong className="data-table-primary">{humanizeOperation(action.operation)}</strong>
                <small className="data-table-meta">Sequence {action.sequence}</small>
              </span>
              <span className="work-item-action-cell" role="cell">
                <strong>{action.gaggle || "Unknown gaggle"}</strong>
                <small className="data-table-meta">{action.workflow || "Workflow unavailable"}</small>
              </span>
              <span role="cell">{action.runStatus ? humanizeOperation(action.runStatus) : "Unknown"}</span>
              <time dateTime={action.occurredAt} role="cell">{formatTimestamp(action.occurredAt)}</time>
              <span role="cell">
                <a href={routeHash({ page: "run", id: action.runId })}>View run</a>
              </span>
            </div>
          ))}
          {visibleActions.length === 0 && (
            <p className="data-overflow" role="status">No actions match this type.</p>
          )}
          {item.truncated && <p className="data-overflow">Showing the 200 most recent actions.</p>}
        </div>
      </section>
    </>
  );
}

function workItemLabel(repository: string | undefined, externalId: string): string {
  return repository ? `${repository}#${externalId}` : `#${externalId}`;
}

function formatWorkItemCost(cost: WorkItemDetail["cost"]): string {
  if (!cost) return "Not attributed";
  if (cost.costUSD !== undefined) {
    return new Intl.NumberFormat("en-US", {
      style: "currency",
      currency: "USD",
      minimumFractionDigits: 2,
      maximumFractionDigits: 4,
    }).format(cost.costUSD);
  }
  if (cost.nanoAIU !== undefined) {
    return `${new Intl.NumberFormat("en-US").format(cost.nanoAIU)} nano-AIU`;
  }
  return "Not measured";
}

function humanizeOperation(operation: string): string {
  if (!operation) return "Provider action";
  return operation
    .replaceAll("-", " ")
    .replace(/\b\w/g, (letter) => letter.toUpperCase());
}
