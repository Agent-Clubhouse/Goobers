import { useCallback, useEffect, useRef, useState } from "react";
import type { QueryState } from "./api/queryState";
import type {
  DaemonClient,
  TelemetryErrorSignaturesOptions,
  TelemetryErrorSignaturesResult,
  TelemetryStatsOptions,
  TelemetryStatsResult,
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

export function useInsightStats(
  client: DaemonClient,
  window: InsightWindow,
  gaggle?: string,
  workflow?: string,
): {
  retry: () => void;
  state: QueryState<InsightSnapshot>;
} {
  const { cache, freshness, isFresh, subscribe } = useLiveData();
  const cacheKey = dataCacheKey("insight-stats", window);
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
  }, [cache, cacheKey, client, gaggle, isFresh, window, workflow]);

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

export function insightWindowFilters(
  window: InsightWindow,
  now = new Date(),
): TelemetryStatsOptions {
  const milliseconds: Record<Exclude<InsightWindow, "all">, number> = {
    "24h": 24 * 60 * 60 * 1_000,
    "7d": 7 * 24 * 60 * 60 * 1_000,
    "30d": 30 * 24 * 60 * 60 * 1_000,
  };
  const until = now.toISOString();
  return window === "all"
    ? { until }
    : {
        since: new Date(now.getTime() - milliseconds[window]).toISOString(),
        until,
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
