import { useCallback, useEffect, useRef, useState } from "react";
import type { QueryState } from "../api/queryState";
import type {
  DaemonClient,
  TelemetryError,
  TelemetryErrorsOptions,
} from "../api/types";
import { ScopedRequests, type ScopedRequest } from "../api/scopedRequest";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { ScopeStrip } from "../components/ScopeStrip";
import { dataCacheKey, type DataCacheDependency } from "../dataCache";
import { useLiveData } from "../liveData";
import { routeHash, type ErrorRouteFilters } from "../routing";
import { scopeWindowLabel } from "../scope";
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
          {scopeWindowLabel(filters)}.
        </p>
      </header>

      <ScopeStrip
        ariaLabel="Failure reason drill-through scope"
        clearHref={routeHash({ page: "insight" })}
        clearLabel="Back to Insight"
        filters={filters}
      />

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
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cacheKey = errorHistoryCacheKey(filters);
  const initialCached = useRef(cache.get<ErrorHistory>(cacheKey));
  const [state, setState] = useState<QueryState<ErrorHistory>>(() => {
    const cached = initialCached.current;
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });
  const request = useRef<ScopedRequest | undefined>(undefined);
  // Owns the controllers for background refreshes so scope teardown can abort
  // them and any completion that outlives its scope is rejected (#3656).
  const scopedRequests = useRef(new ScopedRequests());
  const items = useRef<TelemetryError[]>(initialCached.current?.items ?? []);
  const nextCursor = useRef<string | undefined>(initialCached.current?.nextCursor);
  const loadingMore = useRef(false);

  const publish = useCallback(
    (fresh: boolean, cacheRevision?: number) => {
      const data: ErrorHistory = {
        items: items.current,
        loadingMore: loadingMore.current,
        nextCursor: nextCursor.current,
      };
      setState(fresh ? { status: "ready", data } : { status: "stale", data });
      if (!loadingMore.current && cacheRevision !== undefined) {
        cache.set(
          cacheKey,
          data,
          errorHistoryDependencies(filters),
          cacheRevision,
        );
      }
    },
    [cache, cacheKey, filters.gaggle, filters.workflow],
  );

  // Reset pagination and load the first bounded page. Used on mount, filter
  // change, and retry — NOT on live invalidation. A live event calls
  // refreshWindow below instead, which merges. Resetting here on every event
  // is #1713's failure mode (runsHistory.ts): the operator loads several
  // pages of errors, any matching run/instance event fires, and the list
  // truncates back to page one with the scroll position lost.
  const reload = useCallback(() => {
    request.current?.abort();
    const dependencies = errorHistoryDependencies(filters);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const pending = scopedRequests.current.begin();
    request.current = pending;
    items.current = [];
    nextCursor.current = undefined;
    loadingMore.current = false;
    setState((current) =>
      current.status === "ready" || current.status === "stale"
        ? { status: "stale", data: { ...current.data, loadingMore: false } }
        : { status: "loading" },
    );

    return client.listTelemetryErrors(errorRequest(filters), { signal: pending.signal }).then(
      (page) => {
        pending.end();
        if (pending.obsolete) {
          return true;
        }
        items.current = page.items;
        nextCursor.current = page.nextCursor;
        publish(isFresh(), cacheRevision);
        return true;
      },
      (error: unknown) => {
        pending.end();
        if (!pending.obsolete) {
          const queryError =
            error instanceof Error ? error : new Error("Unable to read matching errors.");
          setState((current) =>
            current.status === "ready" || current.status === "stale"
              ? { status: "stale", data: current.data, error: queryError }
              : { status: "error", error: queryError },
          );
        }
        return false;
      },
    );
  }, [
    cache,
    cacheKey,
    client,
    filters.code,
    filters.errorClass,
    filters.gaggle,
    filters.since,
    filters.stage,
    filters.until,
    filters.workflow,
    isFresh,
    publish,
  ]);

  // Fetch the freshest first page and merge newly-seen errors ahead of the
  // loaded window instead of discarding it. A background refresh must not
  // undo "Load more errors" clicks (#2308, mirroring runsHistory.ts's
  // refreshWindow for #1713).
  const refreshWindow = useCallback(() => {
    if (loadingMore.current) {
      return Promise.resolve(true);
    }
    if (items.current.length === 0) {
      return reload();
    }
    const dependencies = errorHistoryDependencies(filters);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    // Registered with scopedRequests, unlike the controller this used to hold
    // privately: a refresh started under one signature, scope, or window must
    // not merge its page into a different one after the operator navigates
    // (#3656).
    const pending = scopedRequests.current.begin();

    return client.listTelemetryErrors(errorRequest(filters), { signal: pending.signal }).then(
      (page) => {
        pending.end();
        if (pending.obsolete) {
          return true;
        }
        items.current = mergeErrors(items.current, page.items);
        publish(isFresh(), cacheRevision);
        return true;
      },
      (error: unknown) => {
        pending.end();
        if (!pending.obsolete) {
          setState((current) =>
            current.status === "ready" || current.status === "stale"
              ? {
                  status: "stale",
                  data: current.data,
                  error:
                    error instanceof Error
                      ? error
                      : new Error("Unable to refresh matching errors."),
                }
              : {
                  status: "error",
                  error:
                    error instanceof Error
                      ? error
                      : new Error("Unable to refresh matching errors."),
                },
          );
        }
        return false;
      },
    );
  }, [
    cache,
    cacheKey,
    client,
    filters.code,
    filters.errorClass,
    filters.gaggle,
    filters.since,
    filters.stage,
    filters.until,
    filters.workflow,
    isFresh,
    publish,
    reload,
  ]);

  const loadMore = useCallback(() => {
    if (!nextCursor.current || loadingMore.current) {
      return;
    }
    const pending = scopedRequests.current.begin();
    const dependencies = errorHistoryDependencies(filters);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    request.current = pending;
    loadingMore.current = true;
    publish(isFresh());
    void client
      .listTelemetryErrors(
        { ...errorRequest(filters), cursor: nextCursor.current },
        { signal: pending.signal },
      )
      .then(
        (page) => {
          pending.end();
          if (pending.obsolete) {
            return;
          }
          items.current = [...items.current, ...page.items];
          nextCursor.current = page.nextCursor;
          loadingMore.current = false;
          publish(isFresh(), cacheRevision);
        },
        (error: unknown) => {
          pending.end();
          if (!pending.obsolete) {
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
    cache,
    cacheKey,
    client,
    filters.code,
    filters.errorClass,
    filters.gaggle,
    filters.since,
    filters.stage,
    filters.until,
    filters.workflow,
    isFresh,
    publish,
  ]);

  useEffect(() => {
    const unsubscribe = subscribe(
      ["instance", "run"],
      (_models, reason) => {
        const cached = reason === "initial" ? cache.get<ErrorHistory>(cacheKey) : undefined;
        if (cached) {
          items.current = cached.items;
          nextCursor.current = cached.nextCursor;
          loadingMore.current = false;
          const data = { ...cached, loadingMore: false };
          setState(
            isFresh() ? { status: "ready", data } : { status: "stale", data },
          );
          return true;
        }
        // reason === "initial" with no cache is a genuine first load; anything
        // else is a live invalidation, which must not discard the window.
        return reason === "initial" ? reload() : refreshWindow();
      },
      { gaggle: filters.gaggle, workflow: filters.workflow },
    );
    return () => {
      unsubscribe();
      request.current?.abort();
      // Unmount and a scope change are the same thing here: everything started
      // for the scope being torn down is abandoned, and anything that already
      // resolved is rejected on the way back in (#3656).
      scopedRequests.current.cancelScope();
      request.current = undefined;
    };
  }, [cache, cacheKey, filters.gaggle, filters.workflow, isFresh, reload, refreshWindow, subscribe]);

  useEffect(() => {
    setState((current) => {
      if (freshness !== "connected" && current.status === "ready") {
        return { status: "stale", data: current.data };
      }
      if (freshness === "connected" && current.status === "stale" && !current.error) {
        return { status: "ready", data: current.data };
      }
      return current;
    });
  }, [freshness]);

  const retry = useCallback(() => {
    cache.remove(cacheKey);
    void reload();
  }, [cache, cacheKey, reload]);
  return { loadMore, retry, state };
}

// Errors have no stable id (a runId can recur across occurrences), so
// identity is the composite key the row list already uses for its React
// key. Fresh items not already in the loaded window are prepended — the
// server returns each page newest-first, so a refreshed first page's novel
// entries stay newest-first ahead of the preserved window.
function mergeErrors(
  existing: TelemetryError[],
  incoming: TelemetryError[],
): TelemetryError[] {
  const errorKey = (item: TelemetryError) =>
    `${item.runId}:${item.occurredAt}:${item.code}:${item.attempt}`;
  const seen = new Set(existing.map(errorKey));
  const fresh = incoming.filter((item) => !seen.has(errorKey(item)));
  return [...fresh, ...existing];
}

function errorHistoryCacheKey(filters: ErrorRouteFilters): string {
  return dataCacheKey(
    "error-history",
    filters.gaggle ?? "",
    filters.workflow ?? "",
    filters.stage ?? "",
    optionalCachePart(filters.code),
    optionalCachePart(filters.errorClass),
    filters.since ?? "",
    filters.until ?? "",
  );
}

function optionalCachePart(value: string | undefined): string {
  return JSON.stringify([value !== undefined, value ?? ""]);
}

function errorHistoryDependencies(
  filters: ErrorRouteFilters,
): readonly DataCacheDependency[] {
  return [
    { model: "instance" },
    { model: "run", gaggle: filters.gaggle, workflow: filters.workflow },
  ];
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

