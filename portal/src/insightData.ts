import { useCallback, useEffect, useRef, useState } from "react";
import type { QueryState } from "./api/queryState";
import type {
  DaemonClient,
  TelemetryErrorSignaturesOptions,
  TelemetryErrorSignaturesResult,
  TelemetryStatsOptions,
  TelemetryStatsResult,
  TelemetryUsageStats,
} from "./api/types";
import { dataCacheKey, type DataCacheDependency } from "./dataCache";
import { useLiveData } from "./liveData";

export type InsightWindow = "24h" | "7d" | "30d" | "all";

export interface InsightSnapshot {
  filters: TelemetryStatsOptions;
  stats: TelemetryStatsResult;
  window: InsightWindow;
}

export interface InsightErrorSignaturesSnapshot {
  filters: TelemetryErrorSignaturesOptions;
  requestKey: string;
  result: TelemetryErrorSignaturesResult;
}

/** A single point in the cost/token trend line: one bucket's usage rollups. */
export interface InsightCostTrendPeriod {
  since: string;
  until: string;
  usage: TelemetryUsageStats[];
}

export interface InsightCostTrendSnapshot {
  /** Ascending, oldest to newest. Empty when `window` is "all" — there is no
   * fixed length to divide into buckets or to compare against a preceding
   * period of the same length. */
  buckets: InsightCostTrendPeriod[];
  /** The window of the same length immediately preceding the selected one.
   * Undefined for "all", for the same reason `buckets` is empty. */
  previous?: InsightCostTrendPeriod;
  window: InsightWindow;
}

export interface InsightGaggleSpend {
  gaggle: string;
  usage?: TelemetryUsageStats;
}

export interface InsightCostRollupSnapshot {
  filters: TelemetryStatsOptions;
  /** Undefined when no model reported a measured cost in this window. */
  totalCostUSD?: number;
  totalCostSamples: number;
  /** Descending by estimated spend (P50 cost × cost samples) — the wire
   * contract totals cost per model, not per gaggle, so a gaggle's own total
   * is not directly queryable; this ranks gaggles the same way the rest of
   * Insight already reports cost, by percentile. */
  byGaggle: InsightGaggleSpend[];
  window: InsightWindow;
}

export function useInsightStats(
  client: DaemonClient,
  window: InsightWindow,
  gaggle?: string,
  workflow?: string,
  scopeRequest = false,
): {
  retry: () => void;
  state: QueryState<InsightSnapshot>;
} {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cacheKey = scopeRequest
    ? dataCacheKey("insight-stats", window, gaggle ?? "", workflow ?? "")
    : dataCacheKey("insight-stats", window);
  const [state, setState] = useState<QueryState<InsightSnapshot>>(() => {
    const cached = cache.get<InsightSnapshot>(cacheKey);
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });
  const request = useRef<AbortController | undefined>(undefined);

  const refresh = useCallback(() => {
    request.current?.abort();
    const cacheRevision = cache.beginWrite(cacheKey, RUN_DATA_DEPENDENCIES);
    const controller = new AbortController();
    request.current = controller;
    const filters = scopeRequest
      ? insightStatsFilters(window, gaggle, workflow)
      : insightWindowFilters(window);
    setState((current) =>
      (current.status === "ready" || current.status === "stale") &&
      current.data.window === window
        ? { status: "stale", data: current.data }
        : { status: "loading" },
    );

    return client.getTelemetryStats(filters, { signal: controller.signal }).then(
      (stats) => {
        if (controller.signal.aborted) {
          return true;
        }
        const data = { filters, stats, window };
        cache.set(cacheKey, data, RUN_DATA_DEPENDENCIES, cacheRevision);
        setState(isFresh() ? { status: "ready", data } : { status: "stale", data });
        return true;
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          const queryError =
            error instanceof Error ? error : new Error("Unable to read telemetry statistics.");
          setState((current) =>
            (current.status === "ready" || current.status === "stale") &&
            current.data.window === window
              ? { status: "stale", data: current.data, error: queryError }
              : { status: "error", error: queryError },
          );
        }
        return false;
      },
    );
  }, [cache, cacheKey, client, gaggle, isFresh, scopeRequest, window, workflow]);

  useEffect(() => {
    const cached = cache.get<InsightSnapshot>(cacheKey);
    setState(cached ? { status: "ready", data: cached } : { status: "loading" });
    const unsubscribe = subscribe(
      ["run"],
      (_models, reason) => {
        const current = reason === "initial" ? cache.get<InsightSnapshot>(cacheKey) : undefined;
        if (current) {
          setState(
            isFresh() ? { status: "ready", data: current } : { status: "stale", data: current },
          );
          return true;
        }
        return refresh();
      },
      { gaggle, workflow },
    );
    return () => {
      unsubscribe();
      request.current?.abort();
    };
  }, [cache, cacheKey, gaggle, isFresh, refresh, subscribe, workflow]);

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
    return refresh();
  }, [cache, cacheKey, refresh]);
  return { retry, state };
}

const TREND_BUCKET_COUNTS: Record<Exclude<InsightWindow, "all">, number> = {
  "24h": 8,
  "7d": 7,
  "30d": 10,
};

/**
 * Splits the current window into evenly-sized buckets, oldest first.
 *
 * Bucket counts are fixed per window rather than one-bucket-per-hour/day,
 * because each bucket costs a network round trip (there is no bucketed
 * telemetry endpoint) — 8/7/10 buckets keeps 24h/7d/30d all readable as a
 * sparkline without firing dozens of requests.
 */
export function insightTrendBuckets(
  window: InsightWindow,
  now = new Date(),
): { since: string; until: string }[] {
  if (window === "all") {
    return [];
  }
  const totalMs = WINDOW_MILLISECONDS[window];
  const bucketCount = TREND_BUCKET_COUNTS[window];
  const bucketMs = totalMs / bucketCount;
  const end = now.getTime();
  const start = end - totalMs;
  return Array.from({ length: bucketCount }, (_, index) => {
    const bucketStart = start + index * bucketMs;
    const bucketEnd = index === bucketCount - 1 ? end : bucketStart + bucketMs;
    return { since: new Date(bucketStart).toISOString(), until: new Date(bucketEnd).toISOString() };
  });
}

/** The window of the same length immediately preceding the selected one. Undefined for "all". */
export function insightPreviousWindowFilters(
  window: InsightWindow,
  now = new Date(),
): { since: string; until: string } | undefined {
  if (window === "all") {
    return undefined;
  }
  const totalMs = WINDOW_MILLISECONDS[window];
  const currentSince = now.getTime() - totalMs;
  return {
    since: new Date(currentSince - totalMs).toISOString(),
    until: new Date(currentSince).toISOString(),
  };
}

export function useInsightCostTrend(
  client: DaemonClient,
  window: InsightWindow,
  gaggle?: string,
  workflow?: string,
): {
  retry: () => void;
  state: QueryState<InsightCostTrendSnapshot>;
} {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cacheKey = dataCacheKey("insight-cost-trend", window, gaggle ?? "", workflow ?? "");
  const [state, setState] = useState<QueryState<InsightCostTrendSnapshot>>(() => {
    const cached = cache.get<InsightCostTrendSnapshot>(cacheKey);
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });
  const request = useRef<AbortController | undefined>(undefined);

  const refresh = useCallback(() => {
    request.current?.abort();
    const dependencies = insightDependencies(gaggle, workflow);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const controller = new AbortController();
    request.current = controller;
    const bucketRanges = insightTrendBuckets(window);
    const previousRange = insightPreviousWindowFilters(window);
    setState((current) =>
      (current.status === "ready" || current.status === "stale") &&
      current.data.window === window
        ? { status: "stale", data: current.data }
        : { status: "loading" },
    );

    const fetchPeriod = (range: { since: string; until: string }) =>
      client
        .getTelemetryStats({ ...range, gaggle, workflow }, { signal: controller.signal })
        .then((stats) => ({ since: range.since, until: range.until, usage: stats.usage }));

    return Promise.all([
      Promise.all(bucketRanges.map(fetchPeriod)),
      previousRange ? fetchPeriod(previousRange) : Promise.resolve(undefined),
    ]).then(
      ([buckets, previous]) => {
        if (controller.signal.aborted) {
          return true;
        }
        const data: InsightCostTrendSnapshot = { buckets, previous, window };
        cache.set(cacheKey, data, dependencies, cacheRevision);
        setState(isFresh() ? { status: "ready", data } : { status: "stale", data });
        return true;
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          const queryError =
            error instanceof Error ? error : new Error("Unable to read the cost trend.");
          setState((current) =>
            (current.status === "ready" || current.status === "stale") &&
            current.data.window === window
              ? { status: "stale", data: current.data, error: queryError }
              : { status: "error", error: queryError },
          );
        }
        return false;
      },
    );
  }, [cache, cacheKey, client, gaggle, isFresh, window, workflow]);

  useEffect(() => {
    const cached = cache.get<InsightCostTrendSnapshot>(cacheKey);
    setState(cached ? { status: "ready", data: cached } : { status: "loading" });
    const unsubscribe = subscribe(
      ["run"],
      (_models, reason) => {
        const current =
          reason === "initial" ? cache.get<InsightCostTrendSnapshot>(cacheKey) : undefined;
        if (current) {
          setState(
            isFresh() ? { status: "ready", data: current } : { status: "stale", data: current },
          );
          return true;
        }
        return refresh();
      },
      { gaggle, workflow },
    );
    return () => {
      unsubscribe();
      request.current?.abort();
    };
  }, [cache, cacheKey, gaggle, isFresh, refresh, subscribe, workflow]);

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
    return refresh();
  }, [cache, cacheKey, refresh]);
  return { retry, state };
}

/**
 * Instance-wide cost rollup: total spend and a per-gaggle ranking.
 *
 * Deliberately unscoped — no gaggle/workflow arguments. #2533 asks for a
 * number visible "without selecting a specific scope", but the rest of
 * Insight's `getTelemetryStats` calls narrow by whatever scope is currently
 * selected in the UI (see `useInsightStats`). This hook always asks for the
 * whole instance, independent of that selection, so switching the Scope
 * dropdown does not change what it reports.
 */
export function useInsightCostRollup(
  client: DaemonClient,
  window: InsightWindow,
): {
  retry: () => void;
  state: QueryState<InsightCostRollupSnapshot>;
} {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cacheKey = dataCacheKey("insight-cost-rollup", window);
  const [state, setState] = useState<QueryState<InsightCostRollupSnapshot>>(() => {
    const cached = cache.get<InsightCostRollupSnapshot>(cacheKey);
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });
  const request = useRef<AbortController | undefined>(undefined);

  const refresh = useCallback(() => {
    request.current?.abort();
    const cacheRevision = cache.beginWrite(cacheKey, RUN_DATA_DEPENDENCIES);
    const controller = new AbortController();
    request.current = controller;
    const filters = insightWindowFilters(window);
    setState((current) =>
      (current.status === "ready" || current.status === "stale") &&
      current.data.window === window
        ? { status: "stale", data: current.data }
        : { status: "loading" },
    );

    return client.getTelemetryStats(filters, { signal: controller.signal }).then(
      (stats) => {
        if (controller.signal.aborted) {
          return true;
        }
        const data = costRollupFromStats(filters, stats, window);
        cache.set(cacheKey, data, RUN_DATA_DEPENDENCIES, cacheRevision);
        setState(isFresh() ? { status: "ready", data } : { status: "stale", data });
        return true;
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          const queryError =
            error instanceof Error ? error : new Error("Unable to read instance spend.");
          setState((current) =>
            (current.status === "ready" || current.status === "stale") &&
            current.data.window === window
              ? { status: "stale", data: current.data, error: queryError }
              : { status: "error", error: queryError },
          );
        }
        return false;
      },
    );
  }, [cache, cacheKey, client, isFresh, window]);

  useEffect(() => {
    const cached = cache.get<InsightCostRollupSnapshot>(cacheKey);
    setState(cached ? { status: "ready", data: cached } : { status: "loading" });
    const unsubscribe = subscribe(["run"], (_models, reason) => {
      const current =
        reason === "initial" ? cache.get<InsightCostRollupSnapshot>(cacheKey) : undefined;
      if (current) {
        setState(
          isFresh() ? { status: "ready", data: current } : { status: "stale", data: current },
        );
        return true;
      }
      return refresh();
    });
    return () => {
      unsubscribe();
      request.current?.abort();
    };
  }, [cache, cacheKey, isFresh, refresh, subscribe]);

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
    return refresh();
  }, [cache, cacheKey, refresh]);
  return { retry, state };
}

function costRollupFromStats(
  filters: TelemetryStatsOptions,
  stats: TelemetryStatsResult,
  window: InsightWindow,
): InsightCostRollupSnapshot {
  const totalCostSamples = stats.models.reduce((sum, model) => sum + model.costSamples, 0);
  const totalCostUSD =
    totalCostSamples === 0
      ? undefined
      : stats.models.reduce((sum, model) => sum + (model.costUSD ?? 0), 0);
  const byGaggle = stats.gaggles
    .map((gaggle) => ({
      gaggle: gaggle.gaggle,
      usage: stats.usage.find(
        (item) => item.scope === "gaggle" && item.gaggle === gaggle.gaggle,
      ),
    }))
    .sort((left, right) => (right.usage?.costUSD ?? 0) - (left.usage?.costUSD ?? 0));
  return { filters, totalCostUSD, totalCostSamples, byGaggle, window };
}

export function useInsightErrorSignatures(
  client: DaemonClient,
  window: InsightWindow,
  gaggle?: string,
  workflow?: string,
  stage?: string,
): {
  retry: () => void;
  state: QueryState<InsightErrorSignaturesSnapshot>;
} {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const request = useRef<AbortController | undefined>(undefined);
  const requestKey = JSON.stringify([window, gaggle ?? "", workflow ?? "", stage ?? ""]);
  const cacheKey = dataCacheKey("insight-error-signatures", requestKey);
  const [state, setState] = useState<QueryState<InsightErrorSignaturesSnapshot>>(() => {
    const cached = cache.get<InsightErrorSignaturesSnapshot>(cacheKey);
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });

  const refresh = useCallback(() => {
    request.current?.abort();
    const dependencies = insightDependencies(gaggle, workflow);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const controller = new AbortController();
    request.current = controller;
    const filters = insightErrorSignatureFilters(window, gaggle, workflow, stage);
    setState((current) =>
      (current.status === "ready" || current.status === "stale") &&
      current.data.requestKey === requestKey
        ? { status: "stale", data: current.data }
        : { status: "loading" },
    );

    return client.getTelemetryErrorSignatures(filters, { signal: controller.signal }).then(
      (result) => {
        if (controller.signal.aborted) {
          return true;
        }
        const data = { filters, requestKey, result };
        cache.set(cacheKey, data, dependencies, cacheRevision);
        setState(isFresh() ? { status: "ready", data } : { status: "stale", data });
        return true;
      },
      (error: unknown) => {
        if (!controller.signal.aborted) {
          const queryError =
            error instanceof Error ? error : new Error("Unable to read failure reasons.");
          setState((current) =>
            (current.status === "ready" || current.status === "stale") &&
            current.data.requestKey === requestKey
              ? { status: "stale", data: current.data, error: queryError }
              : { status: "error", error: queryError },
          );
        }
        return false;
      },
    );
  }, [cache, cacheKey, client, gaggle, isFresh, requestKey, stage, window, workflow]);

  useEffect(() => {
    const cached = cache.get<InsightErrorSignaturesSnapshot>(cacheKey);
    setState(cached ? { status: "ready", data: cached } : { status: "loading" });
    const unsubscribe = subscribe(
      ["run"],
      (_models, reason) => {
        const current =
          reason === "initial"
            ? cache.get<InsightErrorSignaturesSnapshot>(cacheKey)
            : undefined;
        if (current) {
          setState(
            isFresh() ? { status: "ready", data: current } : { status: "stale", data: current },
          );
          return true;
        }
        return refresh();
      },
      { gaggle, workflow },
    );
    return () => {
      unsubscribe();
      request.current?.abort();
    };
  }, [cache, cacheKey, gaggle, isFresh, refresh, subscribe, workflow]);

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
    return refresh();
  }, [cache, cacheKey, refresh]);
  return { retry, state };
}

const RUN_DATA_DEPENDENCIES: readonly DataCacheDependency[] = [{ model: "run" }];

function insightDependencies(
  gaggle: string | undefined,
  workflow: string | undefined,
): readonly DataCacheDependency[] {
  return [{ model: "run", gaggle, workflow }];
}

const WINDOW_MILLISECONDS: Record<Exclude<InsightWindow, "all">, number> = {
  "24h": 24 * 60 * 60 * 1_000,
  "7d": 7 * 24 * 60 * 60 * 1_000,
  "30d": 30 * 24 * 60 * 60 * 1_000,
};

export function insightWindowFilters(
  window: InsightWindow,
  now = new Date(),
): TelemetryStatsOptions {
  const until = now.toISOString();
  return window === "all"
    ? { until }
    : {
        since: new Date(now.getTime() - WINDOW_MILLISECONDS[window]).toISOString(),
        until,
      };
}

export function insightStatsFilters(
  window: InsightWindow,
  gaggle?: string,
  workflow?: string,
  now = new Date(),
): TelemetryStatsOptions {
  return {
    ...insightWindowFilters(window, now),
    ...(gaggle ? { gaggle } : {}),
    ...(workflow ? { workflow } : {}),
  };
}

export function insightErrorSignatureFilters(
  window: InsightWindow,
  gaggle?: string,
  workflow?: string,
  stage?: string,
  now = new Date(),
): TelemetryErrorSignaturesOptions {
  return {
    ...insightWindowFilters(window, now),
    gaggle,
    workflow,
    stage,
    limit: 20,
  };
}
