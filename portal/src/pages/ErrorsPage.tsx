import { useCallback, useEffect, useRef, useState } from "react";
import type { QueryState } from "../api/queryState";
import type {
  DaemonClient,
  TelemetryError,
  TelemetryErrorsOptions,
} from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { routeHash, type ErrorRouteFilters } from "../routing";
import { formatTimestamp } from "../runDetailData";
import { Icon } from "../ui/Icon";

const ERRORS_PAGE_SIZE = 50;

interface ErrorHistory {
  items: TelemetryError[];
  loadingMore: boolean;
  nextCursor?: string;
}

export function ErrorsPage({
  client,
  filters,
  standalone,
}: {
  client: DaemonClient;
  filters: ErrorRouteFilters;
  standalone: boolean;
}) {
  const query = useErrorHistory(client, filters);

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
  const code = filters.code === "" ? "uncoded" : (filters.code ?? "all codes");
  const errorClass =
    filters.errorClass === "" ? "unknown" : (filters.errorClass ?? "all coarse classes");

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Telemetry</p>
        <h1>Matching errors</h1>
        <p>
          Failure events matching <span className="mono">{code}</span> and{" "}
          <span className="mono">{errorClass}</span>
          {formatWindow(filters)}.
        </p>
      </header>

      <div aria-label="Failure reason drill-through scope" className="run-scope-strip">
        <strong>{formatScope(filters)}</strong>
        <a href={routeHash({ page: "insight" })}>Back to Insight</a>
      </div>

      {query.state.status === "stale" && query.state.error && (
        <div className="insight-inline-error" role="alert">
          <span>Matching errors could not be refreshed. Showing the last successful page.</span>
          <button className="text-button" onClick={query.retry} type="button">
            Retry
          </button>
        </div>
      )}

      <section className="content-section">
        {history.items.length === 0 ? (
          <p className="inline-empty">No errors match this signature, scope, and time window.</p>
        ) : (
          <>
            <div aria-label="Matching error history" className="telemetry-errors" role="region">
              <div aria-hidden="true" className="telemetry-error-header">
                <span>Error</span>
                <span>Coarse class</span>
                <span>Location</span>
                <span>Occurred</span>
                <span />
              </div>
              {history.items.map((item, index) => (
                <ErrorHistoryRow
                  item={item}
                  key={`${item.runId}:${item.occurredAt}:${item.code}:${index}`}
                />
              ))}
            </div>
            {history.nextCursor && (
              <div className="load-more">
                <button
                  className="text-button"
                  disabled={history.loadingMore}
                  onClick={query.loadMore}
                  type="button"
                >
                  {history.loadingMore ? "Loading…" : "Load more errors"}
                </button>
              </div>
            )}
          </>
        )}
      </section>
    </>
  );
}

function ErrorHistoryRow({ item }: { item: TelemetryError }) {
  const code = item.code || "uncoded";
  const location = item.runId
    ? [item.workflow, item.stage, item.attempt > 0 ? `attempt ${item.attempt}` : undefined]
        .filter(Boolean)
        .join(" / ")
    : "Instance scheduler";
  const content = (
    <>
      <span className="error-history-primary">
        <strong className="mono">{code}</strong>
        <small>{item.message || "No error message recorded."}</small>
      </span>
      <span className="error-class-label">{item.errorClass || "unknown"}</span>
      <span>{location}</span>
      <time dateTime={item.occurredAt}>{formatTimestamp(item.occurredAt)}</time>
      {item.runId ? <Icon name="chevron" size={15} /> : <span />}
    </>
  );

  return item.runId ? (
    <a
      aria-label={`Open run ${item.runId} for error ${code}`}
      className="telemetry-error-row"
      href={routeHash({ page: "run", id: item.runId })}
    >
      {content}
    </a>
  ) : (
    <div className="telemetry-error-row telemetry-error-row-instance">{content}</div>
  );
}

function useErrorHistory(client: DaemonClient, filters: ErrorRouteFilters) {
  const [state, setState] = useState<QueryState<ErrorHistory>>({ status: "loading" });
  const request = useRef<AbortController | undefined>(undefined);
  const items = useRef<TelemetryError[]>([]);
  const nextCursor = useRef<string | undefined>(undefined);
  const loadingMore = useRef(false);

  const publish = useCallback(() => {
    setState({
      status: "ready",
      data: {
        items: items.current,
        loadingMore: loadingMore.current,
        nextCursor: nextCursor.current,
      },
    });
  }, []);

  const refresh = useCallback(() => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    items.current = [];
    nextCursor.current = undefined;
    loadingMore.current = false;
    setState({ status: "loading" });

    void client.listTelemetryErrors(errorRequest(filters), { signal: controller.signal }).then(
      (page) => {
        if (controller.signal.aborted) {
          return;
        }
        items.current = page.items;
        nextCursor.current = page.nextCursor;
        publish();
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          setState({
            status: "error",
            error: error instanceof Error ? error : new Error("Unable to read matching errors."),
          });
        }
      },
    );
  }, [
    client,
    filters.code,
    filters.errorClass,
    filters.gaggle,
    filters.since,
    filters.stage,
    filters.until,
    filters.workflow,
    publish,
  ]);

  const loadMore = useCallback(() => {
    if (!nextCursor.current || loadingMore.current) {
      return;
    }
    const controller = new AbortController();
    request.current = controller;
    loadingMore.current = true;
    publish();
    void client
      .listTelemetryErrors(
        { ...errorRequest(filters), cursor: nextCursor.current },
        { signal: controller.signal },
      )
      .then(
        (page) => {
          if (controller.signal.aborted) {
            return;
          }
          items.current = [...items.current, ...page.items];
          nextCursor.current = page.nextCursor;
          loadingMore.current = false;
          publish();
        },
        (error: unknown) => {
          if (!controller.signal.aborted) {
            loadingMore.current = false;
            setState({
              status: "stale",
              data: {
                items: items.current,
                loadingMore: false,
                nextCursor: nextCursor.current,
              },
              error:
                error instanceof Error ? error : new Error("Unable to read more matching errors."),
            });
          }
        },
      );
  }, [
    client,
    filters.code,
    filters.errorClass,
    filters.gaggle,
    filters.since,
    filters.stage,
    filters.until,
    filters.workflow,
    publish,
  ]);

  useEffect(() => {
    refresh();
    return () => request.current?.abort();
  }, [refresh]);

  return { loadMore, retry: refresh, state };
}

function errorRequest(filters: ErrorRouteFilters): TelemetryErrorsOptions {
  return {
    gaggle: filters.gaggle,
    workflow: filters.workflow,
    stage: filters.stage,
    code: filters.code,
    errorClass: filters.errorClass,
    since: filters.since,
    until: filters.until,
    limit: ERRORS_PAGE_SIZE,
  };
}

function formatScope(filters: ErrorRouteFilters): string {
  if (filters.stage) {
    return `${filters.gaggle ?? "All gaggles"} / ${filters.workflow ?? "All workflows"} / ${filters.stage}`;
  }
  if (filters.workflow) {
    return `${filters.gaggle ?? "All gaggles"} / ${filters.workflow}`;
  }
  return filters.gaggle ? `Gaggle: ${filters.gaggle}` : "Instance";
}

function formatWindow(filters: ErrorRouteFilters): string {
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
