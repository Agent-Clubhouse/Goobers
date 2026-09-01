import { useCallback, useEffect, useRef, useState } from "react";
import { QueryFamily } from "./api/queryFamily";
import type { QueryState } from "./api/queryState";
import type { UpdateModel } from "./api/types";
import type { DataCacheDependency } from "./dataCache";
import { useLiveData, type LiveDataScope } from "./liveData";

/**
 * The one live-query lifecycle, shared by every snapshot-shaped hook (#2455).
 *
 * # What was wrong
 *
 * Run detail, workflow detail and the Insight hooks each carried their own
 * copy of the same state machine, and every copy started its refresh with
 * `request.current?.abort()`. Under a live stream that is the pile-up
 * QueryFamily exists to end: an invalidation aborted the in-flight read to
 * start a newer one, the next invalidation aborted that, and at a high enough
 * event rate no read ever completed — the page stayed stale exactly while its
 * data was changing fastest.
 *
 * Routing every refresh through a QueryFamily fixes it for all of them at
 * once: a useful in-flight read is never abandoned for a newer event, and the
 * events that arrive while it runs collapse into a single follow-up pass. The
 * only aborts left are scope aborts — unmount, or a cache key change — where
 * the response would answer a question nobody is asking any more.
 */
export interface LiveQueryOptions<T> {
  /** Session cache key for the snapshot. A change re-subscribes and reloads. */
  cacheKey: string;
  dependencies: readonly DataCacheDependency[];
  /** Models whose invalidation should refresh this query. */
  models: readonly UpdateModel[];
  /** Subscription scope, so unrelated invalidations do not wake this query. */
  scope?: LiveDataScope;
  load: (signal: AbortSignal) => Promise<T>;
  /** Message for a rejection that is not an Error. */
  errorMessage: string;
  /**
   * Whether an already-held snapshot still answers the current inputs.
   *
   * Insight keeps data for the previously selected window in state; showing it
   * as "stale" while a different window loads would label the wrong numbers as
   * this window's. Defaults to keeping the snapshot.
   */
  isCurrent?: (data: T) => boolean;
}

export interface LiveQuery<T> {
  retry: () => void;
  state: QueryState<T>;
}

export function useLiveQuery<T>(options: LiveQueryOptions<T>): LiveQuery<T> {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const { cacheKey } = options;
  // Read at call time so an ordinary re-render does not rebuild the family and
  // forget what is in flight.
  const latest = useRef(options);
  latest.current = options;

  const [state, setState] = useState<QueryState<T>>(() => {
    const cached = cache.get<T>(cacheKey);
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });
  const family = useRef<QueryFamily | undefined>(undefined);

  const publish = useCallback(
    (data: T) => {
      setState(isFresh() ? { status: "ready", data } : { status: "stale", data });
    },
    [isFresh],
  );

  const load = useCallback(
    async (signal: AbortSignal): Promise<void> => {
      const { cacheKey: key, dependencies, errorMessage, isCurrent } = latest.current;
      const retains = (data: T): boolean => (isCurrent ? isCurrent(data) : true);
      const cacheRevision = cache.beginWrite(key, dependencies);
      setState((current) =>
        (current.status === "ready" || current.status === "stale") && retains(current.data)
          ? { status: "stale", data: current.data }
          : { status: "loading" },
      );

      try {
        const data = await latest.current.load(signal);
        if (signal.aborted) {
          return;
        }
        cache.set(key, data, dependencies, cacheRevision);
        // The stream can drop while the request is in flight; the freshness
        // effect below only fires on a freshness change, so publishing an
        // unconditional "ready" here would leave the page claiming live data
        // until the next transition (#3657).
        publish(data);
      } catch (error: unknown) {
        if (signal.aborted) {
          return;
        }
        const queryError = error instanceof Error ? error : new Error(errorMessage);
        setState((current) =>
          (current.status === "ready" || current.status === "stale") && retains(current.data)
            ? { status: "stale", data: current.data, error: queryError }
            : { status: "error", error: queryError },
        );
      }
    },
    [cache, publish],
  );

  const scope = options.scope;
  const scopeKey = `${scope?.gaggle ?? ""}|${scope?.runId ?? ""}|${scope?.workflow ?? ""}`;
  const modelsKey = options.models.join(",");

  useEffect(() => {
    const cached = cache.get<T>(cacheKey);
    setState(cached ? { status: "ready", data: cached } : { status: "loading" });
    const owned = new QueryFamily((context) => load(context.signal));
    family.current = owned;
    const unsubscribe = subscribe(
      latest.current.models,
      (_models, reason) => {
        const current = reason === "initial" ? cache.get<T>(cacheKey) : undefined;
        if (current) {
          publish(current);
          return true;
        }
        // Through the family, not straight to the loader: an event arriving
        // while a read is already running queues one follow-up pass rather
        // than cancelling the read that was about to answer the question.
        owned.request(reason === "initial" ? "initial" : "event");
        return true;
      },
      latest.current.scope,
    );
    return () => {
      unsubscribe();
      // Unmount and a cache key change are the same thing here: nobody is
      // waiting for the answer, so the abort is counted as a scope abort.
      owned.close();
      if (family.current === owned) {
        family.current = undefined;
      }
    };
  }, [cache, cacheKey, load, modelsKey, publish, scopeKey, subscribe]);

  // Freshness downgrade (#1714).
  //
  // Moves ready -> stale on disconnect and back on reconnect, and never clears
  // an error: a stale-with-error state must stay visibly errored rather than
  // silently becoming "ready" because the socket came back.
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
    // Evict the cached snapshot so a retry refetches rather than re-serving the
    // entry that just failed — but do NOT reset to "loading". Blanking the page
    // to a full skeleton on retry is the regression #1684 fixed; the loader
    // already moves ready/stale data to "stale" and keeps it visible while the
    // refetch runs.
    cache.remove(cacheKey);
    family.current?.request("retry");
  }, [cache, cacheKey]);

  return { retry, state };
}
