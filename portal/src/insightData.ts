import type { QueryState } from "./api/queryState";
import type {
  DaemonClient,
  TelemetryErrorSignaturesOptions,
  TelemetryErrorSignaturesResult,
  TelemetryStatsOptions,
  TelemetryStatsResult,
  TelemetryTrendBucket,
  TelemetryUsageStats,
  UpdateModel,
} from "./api/types";
import { dataCacheKey, type DataCacheDependency } from "./dataCache";
import { useLiveQuery } from "./liveQuery";

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
  enabled = true,
): {
  retry: () => void;
  state: QueryState<InsightSnapshot>;
} {
  return useLiveQuery<InsightSnapshot>({
    cacheKey: scopeRequest
      ? dataCacheKey("insight-stats", window, gaggle ?? "", workflow ?? "")
      : dataCacheKey("insight-stats", window),
    enabled,
    dependencies: RUN_DATA_DEPENDENCIES,
    models: RUN_MODELS,
    scope: { gaggle, workflow },
    isCurrent: (data) => data.window === window,
    errorMessage: "Unable to read telemetry statistics.",
    load: async (signal) => {
      const filters = scopeRequest
        ? insightStatsFilters(window, gaggle, workflow)
        : insightWindowFilters(window);
      const stats = await client.getTelemetryStats(filters, { signal });
      return { filters, stats, window };
    },
  });
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

export function selectInsightCostTrendBuckets(
  trend: TelemetryTrendBucket[],
  bucketCount: number,
  hasPrevious: boolean,
): TelemetryTrendBucket[] {
  const combinedCount = hasPrevious ? bucketCount * 2 : bucketCount;
  const combinedTrend = trend.slice(0, combinedCount);
  const currentStart = hasPrevious ? bucketCount : 0;
  return combinedTrend.slice(currentStart, currentStart + bucketCount);
}

export function useInsightCostTrend(
  client: DaemonClient,
  window: InsightWindow,
  gaggle?: string,
  workflow?: string,
  enabled = true,
): {
  retry: () => void;
  state: QueryState<InsightCostTrendSnapshot>;
} {
  return useLiveQuery<InsightCostTrendSnapshot>({
    cacheKey: dataCacheKey("insight-cost-trend", window, gaggle ?? "", workflow ?? ""),
    enabled,
    dependencies: insightDependencies(gaggle, workflow),
    models: RUN_MODELS,
    scope: { gaggle, workflow },
    isCurrent: (data) => data.window === window,
    errorMessage: "Unable to read the cost trend.",
    load: async (signal) => {
      const bucketRanges = insightTrendBuckets(window);
      if (bucketRanges.length === 0) {
        return { buckets: [], window };
      }
      const previousRange = insightPreviousWindowFilters(window);
      const currentRange = insightWindowFilters(window);
      const trendSince = previousRange?.since ?? bucketRanges[0]?.since;
      const trendUntil = bucketRanges.at(-1)?.until;
      const trendBucketCount = previousRange ? bucketRanges.length * 2 : bucketRanges.length;
      const stats = await client.getTelemetryStats(
        {
          gaggle,
          workflow,
          since: currentRange.since,
          until: currentRange.until,
          trendSince,
          trendUntil,
          trendBuckets: trendSince && trendUntil ? trendBucketCount : undefined,
          trendPreviousSince: previousRange?.since,
          trendPreviousUntil: previousRange?.until,
        },
        { signal },
      );
      const buckets = selectInsightCostTrendBuckets(
        stats.trend ?? [],
        bucketRanges.length,
        previousRange !== undefined,
      ).map(({ since, until, usage }) => ({ since, until, usage }));
      return { buckets, previous: previousRange ? stats.trendPrevious : undefined, window };
    },
  });
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
  enabled = true,
): {
  retry: () => void;
  state: QueryState<InsightCostRollupSnapshot>;
} {
  return useLiveQuery<InsightCostRollupSnapshot>({
    cacheKey: dataCacheKey("insight-cost-rollup", window),
    enabled,
    dependencies: RUN_DATA_DEPENDENCIES,
    models: RUN_MODELS,
    isCurrent: (data) => data.window === window,
    errorMessage: "Unable to read instance spend.",
    load: async (signal) => {
      const filters = insightWindowFilters(window);
      const stats = await client.getTelemetryStats(filters, { signal });
      return costRollupFromStats(filters, stats, window);
    },
  });
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
  enabled = true,
): {
  retry: () => void;
  state: QueryState<InsightErrorSignaturesSnapshot>;
} {
  const requestKey = JSON.stringify([window, gaggle ?? "", workflow ?? "", stage ?? ""]);
  return useLiveQuery<InsightErrorSignaturesSnapshot>({
    cacheKey: dataCacheKey("insight-error-signatures", requestKey),
    enabled,
    dependencies: insightDependencies(gaggle, workflow),
    models: RUN_MODELS,
    scope: { gaggle, workflow },
    isCurrent: (data) => data.requestKey === requestKey,
    errorMessage: "Unable to read failure reasons.",
    load: async (signal) => {
      const filters = insightErrorSignatureFilters(window, gaggle, workflow, stage);
      const result = await client.getTelemetryErrorSignatures(filters, { signal });
      return { filters, requestKey, result };
    },
  });
}

const RUN_MODELS: readonly UpdateModel[] = ["run"];

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

function insightStatsFilters(
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
