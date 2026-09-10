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
        provider={route.provider}
        repository={route.repository}
        standalone={standalone}
      />
    );
  }
  return (
    <WorkItemListView
      client={client}
      kind={route.kind}
      navigate={navigate}
      standalone={standalone}
    />
  );
}

function WorkItemListView({
  client,
  kind,
  navigate,
  standalone,
}: {
  client: DaemonClient;
  kind?: WorkItemKind;
  navigate: Navigate;
  standalone: boolean;
}) {
  const [state, setState] = useState<PageState<WorkItemPage>>({ status: "loading" });
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

  if (state.status === "loading") return <DaemonLoadingState standalone={standalone} />;
  if (state.status === "error") {
    return <DaemonErrorState error={state.error} retry={load} standalone={standalone} />;
  }

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
            onClick={() => navigate({ page: "work-items", kind: value })}
            type="button"
          >
            {label}
          </button>
        ))}
      </div>
      <section className="content-section">
        {state.data.items.length === 0 ? (
          <p className="inline-empty">No confirmed provider actions match this filter.</p>
        ) : (
          <div className="data-table work-items-table">
            <div aria-hidden="true" className="data-header work-item-grid">
              <span>Work item</span><span>Last action</span><span>Workflow</span><span>Actions</span><span />
            </div>
            {state.data.items.map((item) => (
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
                  <strong>{workItemLabel(item.repository, item.externalId)}</strong>
                  <small>{item.provider} · {item.kind === "pr" ? "pull request" : "issue"}</small>
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
  provider,
  repository,
  standalone,
}: {
  client: DaemonClient;
  externalId: string;
  kind: WorkItemKind;
  provider: string;
  repository: string;
  standalone: boolean;
}) {
  const [state, setState] = useState<PageState<WorkItemDetail>>({ status: "loading" });
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
  return (
    <>
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
          <a className="secondary-button" href={routeHash({ page: "work-items", kind })}>Back to Work Items</a>
          {item.url && (
            <a className="primary-button" href={item.url} rel="noreferrer" target="_blank">
              Open {kind === "pr" ? "pull request" : "issue"}
            </a>
          )}
        </div>
      </header>
      {item.relatedPullRequests.length > 0 && (
        <section className="content-section work-item-related">
          <h2>Related pull requests</h2>
          <p>Inferred from runs attributed to both this issue and the pull request.</p>
          <div className="work-item-related-links">
            {item.relatedPullRequests.map((related) => (
              <a
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
                {workItemLabel(related.repository, related.externalId)}
              </a>
            ))}
          </div>
        </section>
      )}
      <section className="content-section">
        <ol className="work-item-timeline">
          {item.actions.map((action) => (
            <li key={`${action.runId}:${action.sequence}`}>
              <span className="work-item-timeline-mark" />
              <div>
                <strong>{humanizeOperation(action.operation)}</strong>
                <p>
                  {[action.gaggle, action.workflow].filter(Boolean).join(" / ") || "Workflow unavailable"}
                  {action.runStatus ? ` · ${action.runStatus}` : ""}
                </p>
                <time dateTime={action.occurredAt}>{formatTimestamp(action.occurredAt)}</time>
                <a href={routeHash({ page: "run", id: action.runId })}>View run</a>
              </div>
            </li>
          ))}
        </ol>
        {item.truncated && <p className="data-overflow">Showing the 200 most recent actions.</p>}
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
