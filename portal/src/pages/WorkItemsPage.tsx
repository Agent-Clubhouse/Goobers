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
  const [outcome, setOutcome] = useState<WorkItemOutcome | "all">("all");
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
    if (isMissingWorkItemsCapability(state.error)) {
      return (
        <section className="daemon-state daemon-state-error" role="alert">
          <div>
            <h1>Work Items unavailable</h1>
            <p>
              This daemon does not support work item history. Upgrade Goobers to use this page.
            </p>
          </div>
          <button className="reconnect-button" onClick={load} type="button">
            Retry
          </button>
        </section>
      );
    }
    return <DaemonErrorState error={state.error} retry={load} standalone={standalone} />;
  }

  const gaggleOptions = [...new Set(
    state.data.items.map((item) => item.gaggle).filter((value): value is string => Boolean(value)),
  )].sort((left, right) => left.localeCompare(right));
  const normalizedQuery = searchQuery.trim().toLocaleLowerCase();
  const items = state.data.items.filter((item) => {
    if (gaggle && item.gaggle !== gaggle) return false;
    if (outcome !== "all" && item.outcome !== outcome) return false;
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
        <h1>Work Items</h1>
        <p>
          Pull requests, issues, and work items that Goobers changed through a recorded
          provider operation.
        </p>
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
            <span>Status</span>
            <select
              aria-label="Filter work items by status"
              onChange={(event) => setOutcome(event.target.value as WorkItemOutcome | "all")}
              value={outcome}
            >
              <option value="all">All statuses</option>
              <option value="done">Done</option>
              <option value="in-progress">In progress</option>
              <option value="bad-terminal">Bad terminal</option>
            </select>
          </label>
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
          <p className="inline-empty">
            No confirmed provider actions match this filter. This page lists only
            provider changes Goobers recorded; work created or changed outside a
            recorded provider operation may still exist in the provider.
          </p>
        ) : (
          <div className="data-table data-table-shell work-items-table">
            <div aria-hidden="true" className="data-header data-table-header work-item-grid">
              <span>Work item</span><span>Outcome</span><span>Workflow</span><span>Actions</span><span />
            </div>
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
                  disabled={!item.repository}
                  title={item.repository
                    ? undefined
                    : "No repository was recorded for this item, so it has no detail page."}
                  key={workItemRowKey(item)}
                  onClick={() => item.repository && navigate({
                    page: "work-items",
                    provider: item.provider,
                    repository: item.repository,
                    kind: item.kind,
                    id: item.externalId,
                  })}
                  type="button"
                >
                  <span className="work-item-identity">
                    <strong className="data-table-primary" title={label}>{label}</strong>
                    <small className="data-table-meta">
                      {item.provider} · {workItemKindLabel(item.provider, item.kind)}
                      {!item.repository && " · repository unknown"}
                    </small>
                  </span>
                  <span className="work-item-last-action">
                    <strong className={`status-badge work-item-outcome-${item.outcome}`}>
                      {workItemOutcomeLabel(item.outcome)}
                    </strong>
                    <small>
                      Last action: {humanizeOperation(item.lastOperation)} ·{" "}
                      {formatTimestamp(item.lastActionAt)}
                    </small>
                  </span>
                  <span className="work-item-workflow">
                    <strong>{item.workflow || "Unknown"}</strong>
                    <small>{item.gaggle || "No gaggle recorded"}</small>
                  </span>
                  <strong className="work-item-action-count">{item.actionCount}</strong>
                  <span className="work-item-mobile-context">
                    <span
                      className={`status-badge work-item-status work-item-outcome-${item.outcome}`}
                      data-status={item.outcome}
                    >
                      {workItemOutcomeLabel(item.outcome)}
                    </span>
                    <span>
                      Last action: {humanizeOperation(item.lastOperation)} · {item.actionCount}{" "}
                      {item.actionCount === 1 ? "action" : "actions"}
                    </span>
                    <span>
                      {item.gaggle || "Unknown gaggle"} / {item.workflow || "Unknown workflow"}
                    </span>
                  </span>
                  <span className="row-arrow"><Icon name="chevron" size={15} /></span>
                </button>
              );
            })}
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
        <p className="page-kicker">{provider} {workItemKindLabel(provider, kind)} activity</p>
        <h1>{workItemLabel(repository, externalId)}</h1>
        <div className="work-item-summary">
          <span>
            <small>Confirmed actions</small>
            <strong>{item.actions.length}</strong>
          </span>
          <span>
            <small>Attributed AIC to date</small>
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
              href={related.url ?? routeHash({
                page: "work-items",
                provider: related.provider,
                repository: related.repository,
                kind: "pr",
                id: related.externalId,
              })}
              key={workItemRowKey(related)}
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

/** A row key that keeps every identity the read model partitions by, so equal
 *  numeric ids in different providers, repositories, or projects (or with
 *  different recorded URLs when the repository is unknown) never collide. */
function workItemRowKey(
  item: Pick<WorkItemSummary, "provider" | "repository" | "kind" | "externalId" | "url">,
): string {
  return JSON.stringify([item.provider, item.repository ?? "", item.kind, item.externalId, item.url ?? ""]);
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

function workItemOutcomeLabel(outcome: WorkItemOutcome): string {
  switch (outcome) {
    case "done":
      return "Done";
    case "bad-terminal":
      return "Bad terminal";
    case "in-progress":
      return "In progress";
  }
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
  return operation
    .replaceAll("-", " ")
    .replace(/\b\w/g, (letter) => letter.toUpperCase());
}

function isMissingWorkItemsCapability(error: Error): boolean {
  return (
    (error instanceof MissingCapabilityError && error.capability === "work-items") ||
    (error instanceof DaemonApiError &&
      (error.status === 404 || error.code === "not_found"))
  );
}
