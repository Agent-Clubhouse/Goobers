import { useCallback, useEffect, useRef, useState } from "react";
import type { QueryState } from "./api/queryState";
import type { DaemonClient, RunPhase, RunSummary } from "./api/types";
import { useLiveData } from "./liveData";

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
  loadingMore: boolean;
}

export interface RunsHistoryQuery {
  loadMore: () => void;
  retry: () => void;
  state: QueryState<RunsHistory>;
}

interface RunsStream {
  phase: RunPhase | undefined;
  cursor: string | undefined; // undefined = not yet requested
  exhausted: boolean;
}

export function useRunsHistory(client: DaemonClient, filter: RunsFilter): RunsHistoryQuery {
  const [state, setState] = useState<QueryState<RunsHistory>>({ status: "loading" });
  const request = useRef<AbortController | undefined>(undefined);
  const streams = useRef<RunsStream[]>([]);
  const runs = useRef<RunSummary[]>([]);
  const loadingMore = useRef(false);
  const { freshness, isFresh, subscribe } = useLiveData();

  const publish = useCallback((fresh: boolean) => {
    const data: RunsHistory = {
      runs: runs.current,
      hasMore: streams.current.some((stream) => !stream.exhausted),
      loadingMore: loadingMore.current,
    };
    setState(fresh ? { status: "ready", data } : { status: "stale", data });
  }, []);

  // Reset pagination and load the first bounded page. Used on mount, filter
  // change, retry, and live run invalidation — the history reflects current
  // daemon state, always starting from one bounded page.
  const refresh = useCallback(() => {
    request.current?.abort();
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

    return advanceStreams(client, streams.current, controller.signal).then(
      (fetched) => {
        if (controller.signal.aborted) {
          return true;
        }
        runs.current = mergeRuns([], fetched);
        publish(isFresh());
        return true;
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          setState((current) => runsError(current, error));
        }
        return false;
      },
    );
  }, [client, filter, isFresh, publish]);

  const loadMore = useCallback(() => {
    if (loadingMore.current || !streams.current.some((stream) => !stream.exhausted)) {
      return;
    }
    const controller = new AbortController();
    request.current = controller;
    loadingMore.current = true;
    publish(isFresh());

    void advanceStreams(client, streams.current, controller.signal).then(
      (fetched) => {
        if (controller.signal.aborted) {
          return;
        }
        loadingMore.current = false;
        runs.current = mergeRuns(runs.current, fetched);
        publish(isFresh());
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          loadingMore.current = false;
          setState((current) => runsError(current, error));
        }
      },
    );
  }, [client, isFresh, publish]);

  useEffect(() => {
    const unsubscribe = subscribe(["run"], refresh);
    return () => {
      unsubscribe();
      request.current?.abort();
    };
  }, [refresh, subscribe]);

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

  return { loadMore, retry: refresh, state };
}

// Advances every non-exhausted stream by one page and returns the newly fetched
// runs. Each stream tracks its own keyset cursor so the union filters (attention)
// stay correctly paginated.
async function advanceStreams(
  client: DaemonClient,
  streams: RunsStream[],
  signal: AbortSignal,
): Promise<RunSummary[]> {
  const pages = await Promise.all(
    streams.map(async (stream) => {
      if (stream.exhausted) {
        return [] as RunSummary[];
      }
      const page = await client.listRuns(
        { phase: stream.phase, cursor: stream.cursor, limit: RUNS_PAGE_SIZE },
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
    byId.set(run.id, run);
  }
  return [...byId.values()].sort(
    (left, right) =>
      Date.parse(right.startedAt) - Date.parse(left.startedAt) ||
      left.id.localeCompare(right.id),
  );
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
