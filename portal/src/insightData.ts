import { useCallback, useEffect, useRef, useState } from "react";
import type { QueryState } from "./api/queryState";
import type {
  DaemonClient,
  TelemetryStatsOptions,
  TelemetryStatsResult,
} from "./api/types";
import { useLiveData } from "./liveData";

export type InsightWindow = "24h" | "7d" | "30d" | "all";

export interface InsightSnapshot {
  filters: TelemetryStatsOptions;
  stats: TelemetryStatsResult;
  window: InsightWindow;
}

export function useInsightStats(
  client: DaemonClient,
  window: InsightWindow,
): {
  retry: () => void;
  state: QueryState<InsightSnapshot>;
} {
  const [state, setState] = useState<QueryState<InsightSnapshot>>({ status: "loading" });
  const request = useRef<AbortController | undefined>(undefined);
  const { freshness, isFresh, subscribe } = useLiveData();

  const refresh = useCallback(() => {
    request.current?.abort();
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
  }, [client, isFresh, window]);

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

  return { retry: refresh, state };
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
