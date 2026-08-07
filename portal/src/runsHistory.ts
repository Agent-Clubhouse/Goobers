import { useCallback, useEffect, useRef, useState } from "react";
import type { QueryState } from "./api/queryState";
import type {
  DaemonClient,
  ModelInvalidation,
  RunListOptions,
  RunPhase,
  RunSummary,
} from "./api/types";
import { dataCacheKey, type DataCacheDependency } from "./dataCache";
import { useLiveData } from "./liveData";
import { QueryFamily } from "./api/queryFamily";

export type RunsFilter = "all" | "active" | "attention" | "complete";

export const RUNS_PAGE_SIZE = 50;

// Each filter chip maps to the server-side phase filters that back it. "all" is
// one unfiltered stream; "attention" is the union of failed and escalated, so
// it fans out to two independently-cursored streams that are merge-sorted
// client-side (the read API filters a single phase per request). Filtering and
// pagination happen on the daemon — the full journal is never fetched
// client-side (DASH-14).
const FILTER_PHASES: Record<RunsFilter, (RunPhase | undefined)[]> = {
  all: [undefined],
  active: ["running"],
  attention: ["failed", "escalated"],
  complete: ["completed"],
};

export interface RunsHistory {
  runs: RunSummary[];
  hasMore: boolean;
  hasAnyRuns: boolean;
  loadingMore: boolean;
}

export interface RunsHistoryQuery {
  loadMore: () => void;
  retry: () => void;
  state: QueryState<RunsHistory>;
}

export type RunHistoryScope = Pick<
  RunListOptions,
  "gaggle" | "workflow" | "stage" | "outcome" | "population" | "since" | "until" | "showNoWork"
>;

interface RunsStream {
  phase: RunPhase | undefined;
  cursor: string | undefined; // undefined = not yet requested
  exhausted: boolean;
}

interface CachedRunsHistory {
  data: RunsHistory;
  streams: RunsStream[];
}

export function useRunsHistory(
  client: DaemonClient,
  filter: RunsFilter,
  scope: RunHistoryScope = {},
): RunsHistoryQuery {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cacheKey = runsHistoryCacheKey(filter, scope);
  const initialCached = useRef(cache.get<CachedRunsHistory>(cacheKey));
  const [state, setState] = useState<QueryState<RunsHistory>>(() => {
    const cached = initialCached.current;
    return cached ? { status: "ready", data: cached.data } : { status: "loading" };
  });
  const request = useRef<AbortController | undefined>(undefined);
  // The coalescing family (#1930). Held in a ref and recreated only when the
  // subscription scope changes: its whole job is to remember what is in flight,
  // and a family that was rebuilt on every render would remember nothing —
  // which is how the pile-up got in.
  //
  // The loader is read from a ref at call time so ordinary renders do not
  // rebuild the family merely because `refreshWindow` changes identity.
  const refreshLoader = useRef<() => Promise<unknown>>(() => Promise.resolve());
  const family = useRef<QueryFamily | undefined>(undefined);
  const streams = useRef<RunsStream[]>(
    initialCached.current?.streams.map((stream) => ({ ...stream })) ?? [],
  );
  const runs = useRef<RunSummary[]>(initialCached.current?.data.runs ?? []);
  const hasAnyRuns = useRef(initialCached.current?.data.hasAnyRuns ?? false);
  const invalidatedRunIds = useRef(new Set<string>());
  const loadingMore = useRef(false);

  const publish = useCallback((fresh: boolean, cacheRevision?: number) => {
    const data: RunsHistory = {
      runs: runs.current,
      hasMore: streams.current.some((stream) => !stream.exhausted),
      hasAnyRuns: hasAnyRuns.current,
      loadingMore: loadingMore.current,
    };
    setState(fresh ? { status: "ready", data } : { status: "stale", data });
    if (!loadingMore.current && cacheRevision !== undefined) {
      cache.set(
        cacheKey,
        {
          data,
          streams: streams.current.map((stream) => ({ ...stream })),
        },
        runsHistoryDependencies(scope),
        cacheRevision,
      );
    }
  }, [cache, cacheKey, scope.gaggle, scope.workflow]);

  // Reset pagination and load the first bounded page.
  //
  // Used on mount, filter change, and retry — NOT on live invalidation. A live
  // event calls refreshWindow below, which merges instead. Resetting here on
  // every event is #1713: the user pages in five times, any run in scope emits
  // an event, and the list truncates to 50 rows with the scroll position lost.
  // On the unfiltered #/runs route scope.gaggle and scope.workflow are both
  // undefined, so EVERY run event in the instance triggered it — and under the
  // polling fallback that is every 5s, making the page unusable on a busy
  // instance precisely when someone is trying to watch it.
  const reload = useCallback(() => {
    request.current?.abort();
    const dependencies = runsHistoryDependencies(scope);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const controller = new AbortController();
    request.current = controller;
    streams.current = FILTER_PHASES[filter].map((phase) => ({
      phase,
      cursor: undefined,
      exhausted: false,
    }));
    runs.current = [];
    loadingMore.current = false;
    setState((current) =>
      current.status === "ready" || current.status === "stale"
        ? { status: "stale", data: { ...current.data, loadingMore: false } }
        : { status: "loading" },
    );

    return advanceStreams(client, streams.current, scope, controller.signal)
      .then(async (fetched) => {
        if (controller.signal.aborted) {
          return true;
        }
        runs.current = mergeRuns([], fetched);
        hasAnyRuns.current =
          fetched.length > 0 || (await instanceHasRuns(client, controller.signal));
        publish(isFresh(), cacheRevision);
        return true;
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          setState((current) => runsError(current, error));
        }
        return false;
      });
  }, [cache, cacheKey, client, filter, isFresh, publish, scope.gaggle, scope.outcome, scope.population, scope.since, scope.stage, scope.until, scope.workflow, scope.showNoWork]);

  // Merge the newest page into the loaded window, preserving pagination.
  //
  // This is what a live event triggers instead of reload(). It fetches a fresh
  // FIRST page and merges by run id, so:
  //
  //   - runs the user paged in stay loaded, and the scroll position holds;
  //   - new runs appear at the head;
  //   - a run already in the head page is updated by the list response;
  //   - invalidated rows outside the head page are fetched directly by id.
  const refreshWindow = useCallback(() => {
    // Deliberately does NOT abort an in-flight loadMore. A live event arriving
    // while the user is paging must not cancel their page — that was another
    // way the old reset lost work.
    if (loadingMore.current) {
      return Promise.resolve(true);
    }
    // Nothing loaded yet means there is no window to preserve — an invalidation
    // can outrun the initial load, and merging into an empty window would
    // publish hasMore=false off an empty stream set, hiding "Load more"
    // entirely.
    if (streams.current.length === 0) {
      return reload();
    }
    // NOT `request.current?.abort()`. That is what this used to do, and it is
    // the pile-up #1930 exists to end: an event arrived, the in-flight refresh
    // was cancelled to start a newer one, another event arrived, and so on — so
    // at a high enough event rate NO refresh ever completed and the list went
    // permanently stale exactly when it was changing fastest.
    //
    // A request already in flight will return data at least as new as the event
    // that would have cancelled it, because the response reflects server state
    // at response time. Cancelling it discards work that was about to answer
    // the question. Coalescing is handled by the QueryFamily wrapping this
    // loader, which holds at most one in-flight and one queued pass.
    const dependencies = runsHistoryDependencies(scope);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const controller = new AbortController();

    // A throwaway stream set positioned at the top. streams.current is left
    // untouched, so "Load more" continues from where the user actually is.
    const head: RunsStream[] = FILTER_PHASES[filter].map((phase) => ({
      phase,
      cursor: undefined,
      exhausted: false,
    }));
    const affectedRunIds = [...invalidatedRunIds.current];
    invalidatedRunIds.current.clear();

    return advanceStreams(client, head, scope, controller.signal)
      .then(async (fetched) => {
        if (controller.signal.aborted) {
          return true;
        }
        const fetchedIds = new Set(fetched.map((run) => run.id));
        const loadedIds = new Set(runs.current.map((run) => run.id));
        const targeted = await Promise.all(
          affectedRunIds
            .filter((runId) => loadedIds.has(runId) && !fetchedIds.has(runId))
            .map((runId) => client.getRun(runId, { signal: controller.signal })),
        );
        if (controller.signal.aborted) {
          return true;
        }
        runs.current = mergeRuns(runs.current, [...fetched, ...targeted]);
        if (!hasAnyRuns.current && runs.current.length === 0) {
          hasAnyRuns.current = await instanceHasRuns(client, controller.signal);
        } else if (runs.current.length > 0) {
          hasAnyRuns.current = true;
        }
        publish(isFresh(), cacheRevision);
        return true;
      })
      .catch((error: unknown) => {
        if (!controller.signal.aborted) {
          for (const runId of affectedRunIds) {
            invalidatedRunIds.current.add(runId);
          }
          setState((current) => runsError(current, error));
        }
        return false;
      });
  }, [cache, cacheKey, client, filter, isFresh, publish, reload, scope.gaggle, scope.outcome, scope.population, scope.since, scope.stage, scope.until, scope.workflow, scope.showNoWork]);

  const loadMore = useCallback(() => {
    if (loadingMore.current || !streams.current.some((stream) => !stream.exhausted)) {
      return;
    }
    const controller = new AbortController();
    const dependencies = runsHistoryDependencies(scope);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    request.current = controller;
    loadingMore.current = true;
    publish(isFresh(), cacheRevision);

    void advanceStreams(client, streams.current, scope, controller.signal).then(
      (fetched) => {
        if (controller.signal.aborted) {
          return;
        }
        loadingMore.current = false;
        runs.current = mergeRuns(runs.current, fetched);
        publish(isFresh(), cacheRevision);
        if (invalidatedRunIds.current.size > 0) {
          family.current?.request("event");
        }
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          loadingMore.current = false;
          setState((current) => runsError(current, error));
          if (invalidatedRunIds.current.size > 0) {
            family.current?.request("event");
          }
        }
      },
    );
  }, [client, isFresh, publish, scope.gaggle, scope.outcome, scope.population, scope.since, scope.stage, scope.until, scope.workflow, scope.showNoWork]);

  refreshLoader.current = refreshWindow;

  useEffect(() => {
    const owned = new QueryFamily(async () => {
      await refreshLoader.current();
    });
    family.current = owned;
    const unsubscribe = subscribe(
      ["run"],
      (_models, reason, invalidations) => {
        const cached =
          reason === "initial" ? cache.get<CachedRunsHistory>(cacheKey) : undefined;
        if (cached) {
          request.current = undefined;
          streams.current = cached.streams.map((stream) => ({ ...stream }));
          runs.current = cached.data.runs;
          hasAnyRuns.current = cached.data.hasAnyRuns;
          loadingMore.current = false;
          setState(
            isFresh()
              ? { status: "ready", data: cached.data }
              : { status: "stale", data: cached.data },
          );
          return true;
        }
        // reason === "initial" with no cache is a genuine first load; anything
        // else is a live invalidation, which must not discard the window.
        if (reason === "initial") {
          return reload();
        }
        collectInvalidatedRunIds(invalidations, runs.current, invalidatedRunIds.current);
        // Through the family, not straight to refreshWindow: an event arriving
        // while a refresh is already running must queue one follow-up pass, not
        // start a second request and not cancel the first.
        family.current?.request("event");
        return true;
      },
      { gaggle: scope.gaggle, workflow: scope.workflow },
    );
    return () => {
      unsubscribe();
      // Unmount IS a legitimate reason to abort: nobody is waiting for the
      // answer. It is counted as a scope abort rather than an event abort, so
      // the two cannot be confused when reading the stats.
      owned.close();
      if (family.current === owned) {
        family.current = undefined;
      }
      request.current?.abort();
    };
  }, [cache, cacheKey, isFresh, publish, reload, subscribe]);

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
    if (invalidatedRunIds.current.size > 0) {
      family.current?.request("retry");
      return;
    }
    void reload();
  }, [cache, cacheKey, reload]);
  return { loadMore, retry, state };
}

function runsHistoryCacheKey(filter: RunsFilter, scope: RunHistoryScope): string {
  return dataCacheKey(
    "runs-history",
    filter,
    scope.gaggle ?? "",
    scope.workflow ?? "",
    scope.stage ?? "",
    scope.outcome ?? "",
    scope.population ?? "",
    scope.since ?? "",
    scope.until ?? "",
    scope.showNoWork ? "1" : "0",
  );
}

function runsHistoryDependencies(scope: RunHistoryScope): readonly DataCacheDependency[] {
  return [{ model: "run", gaggle: scope.gaggle, workflow: scope.workflow }];
}

// Advances every non-exhausted stream by one page and returns the newly fetched
// runs. Each stream tracks its own keyset cursor so the union filters (attention)
// stay correctly paginated.
async function advanceStreams(
  client: DaemonClient,
  streams: RunsStream[],
  scope: RunHistoryScope,
  signal: AbortSignal,
): Promise<RunSummary[]> {
  const pages = await Promise.all(
    streams.map(async (stream) => {
      if (stream.exhausted) {
        return [] as RunSummary[];
      }
      const page = await client.listRuns(
        { ...scope, phase: stream.phase, cursor: stream.cursor, limit: RUNS_PAGE_SIZE },
        { signal },
      );
      stream.cursor = page.nextCursor;
      stream.exhausted = !page.nextCursor;
      return page.runs;
    }),
  );
  return pages.flat();
}

function mergeRuns(existing: RunSummary[], incoming: RunSummary[]): RunSummary[] {
  const byId = new Map<string, RunSummary>();
  for (const run of existing) {
    byId.set(run.id, run);
  }
  for (const run of incoming) {
    const current = byId.get(run.id);
    if (!current || run.lastSeq >= current.lastSeq) {
      byId.set(run.id, run);
    }
  }
  return [...byId.values()].sort(
    (left, right) =>
      Date.parse(right.startedAt) - Date.parse(left.startedAt) ||
      left.id.localeCompare(right.id),
  );
}

async function instanceHasRuns(client: DaemonClient, signal: AbortSignal): Promise<boolean> {
  const page = await client.listRuns({ limit: 1, showNoWork: true }, { signal });
  return page.runs.length > 0;
}

function collectInvalidatedRunIds(
  invalidations: readonly ModelInvalidation[] | undefined,
  loadedRuns: readonly RunSummary[],
  target: Set<string>,
): void {
  if (!invalidations) {
    return;
  }
  const loadedIds = new Set(loadedRuns.map((run) => run.id));
  for (const invalidation of invalidations) {
    for (const runId of invalidation.runIds ?? []) {
      if (loadedIds.has(runId)) {
        target.add(runId);
      }
    }
  }
}

function runsError(
  current: QueryState<RunsHistory>,
  error: unknown,
): QueryState<RunsHistory> {
  const queryError = error instanceof Error ? error : new Error("Unable to read run history.");
  if (current.status === "ready" || current.status === "stale") {
    return { status: "stale", data: current.data, error: queryError };
  }
  return { status: "error", error: queryError };
}
