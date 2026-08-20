import type { QueryState } from "./api/queryState";
import type {
  NodeCredit,
  TelemetryCurationStats,
  TelemetryGaggleStats,
  TelemetryReadyPool,
  TelemetryRunStats,
  TelemetryStageStats,
  TelemetryStatsOptions,
  TelemetryStatsResult,
  TelemetryUsageStats,
} from "./api/types";
import type {
  InsightCostTrendSnapshot,
  InsightSnapshot,
  InsightWindow,
} from "./insightData";
import type { RunRouteFilters } from "./routing";
import { hasScopeFilters, type ScopeFilters } from "./scope";

export type InsightScope =
  | { kind: "instance" }
  | { kind: "gaggle"; gaggle: string }
  | { kind: "workflow"; gaggle: string; workflow: string }
  | { kind: "stage"; gaggle: string; workflow: string; stage: string };

export interface InsightScopeIdentity {
  gaggle?: string;
  workflow?: string;
  stage?: string;
}

export interface OutcomeMetric {
  failed: number;
  filters: RunRouteFilters;
  label: string;
  other: number;
  successRate?: number;
  succeeded: number;
  total: number;
  unit: "attempts" | "runs";
}

export interface InsightViewModel {
  breakdown: OutcomeMetric[];
  creditAssignment: NodeCredit[];
  curationHealth?: {
    curation: TelemetryCurationStats;
    readyPool: TelemetryReadyPool;
  };
  filters: TelemetryStatsOptions;
  stages: TelemetryStageStats[];
  summary?: OutcomeMetric;
  usage?: TelemetryUsageStats;
  window: InsightWindow;
}

export interface InsightCostTrendViewModel {
  points: {
    since: string;
    until: string;
    usage: TelemetryUsageStats | undefined;
  }[];
  previousUsage?: TelemetryUsageStats;
}

export function insightScopeKey(scope: InsightScope): string {
  switch (scope.kind) {
    case "instance":
      return JSON.stringify(["instance"]);
    case "gaggle":
      return JSON.stringify(["gaggle", scope.gaggle]);
    case "workflow":
      return JSON.stringify(["workflow", scope.gaggle, scope.workflow]);
    case "stage":
      return JSON.stringify(["stage", scope.gaggle, scope.workflow, scope.stage]);
  }
}

export function insightScopeFromKey(key: string): InsightScope {
  try {
    const parts = JSON.parse(key) as unknown;
    if (!Array.isArray(parts) || !parts.every((part) => typeof part === "string")) {
      return { kind: "instance" };
    }
    if (parts.length === 1 && parts[0] === "instance") {
      return { kind: "instance" };
    }
    if (parts.length === 2 && parts[0] === "gaggle" && parts[1]) {
      return { kind: "gaggle", gaggle: parts[1] };
    }
    if (parts.length === 3 && parts[0] === "workflow" && parts[1] && parts[2]) {
      return { kind: "workflow", gaggle: parts[1], workflow: parts[2] };
    }
    if (
      parts.length === 4 &&
      parts[0] === "stage" &&
      parts[1] &&
      parts[2] &&
      parts[3]
    ) {
      return { kind: "stage", gaggle: parts[1], workflow: parts[2], stage: parts[3] };
    }
  } catch {
    return { kind: "instance" };
  }
  return { kind: "instance" };
}

export function insightScopeFromRoute(filters: ScopeFilters | undefined): InsightScope {
  if (filters?.gaggle && filters.workflow && filters.stage) {
    return {
      kind: "stage",
      gaggle: filters.gaggle,
      workflow: filters.workflow,
      stage: filters.stage,
    };
  }
  if (filters?.gaggle && filters.workflow) {
    return { kind: "workflow", gaggle: filters.gaggle, workflow: filters.workflow };
  }
  if (filters?.gaggle) {
    return { kind: "gaggle", gaggle: filters.gaggle };
  }
  return { kind: "instance" };
}

export function insightScopeRouteFilters(
  scope: InsightScope,
  window: InsightWindow,
): ScopeFilters | undefined {
  const filters: ScopeFilters = { ...insightScopeApiParameters(scope), window };
  return hasScopeFilters(filters) ? filters : undefined;
}

export function insightScopeApiParameters(scope: InsightScope): InsightScopeIdentity {
  switch (scope.kind) {
    case "instance":
      return {};
    case "gaggle":
      return { gaggle: scope.gaggle };
    case "workflow":
      return { gaggle: scope.gaggle, workflow: scope.workflow };
    case "stage":
      return { gaggle: scope.gaggle, workflow: scope.workflow, stage: scope.stage };
  }
}

function insightScopeLabel(scope: InsightScope): string {
  switch (scope.kind) {
    case "instance":
      return "Instance";
    case "gaggle":
      return `Gaggle · ${scope.gaggle}`;
    case "workflow":
      return `Workflow · ${scope.gaggle} / ${scope.workflow}`;
    case "stage":
      return `Stage · ${scope.gaggle} / ${scope.workflow} / ${scope.stage}`;
  }
}

function isInInsightScope(
  scope: InsightScope,
  identity: InsightScopeIdentity,
): boolean {
  if (scope.kind === "instance") return true;
  if (identity.gaggle !== scope.gaggle) return false;
  if (scope.kind === "gaggle") return true;
  if (identity.workflow !== scope.workflow) return false;
  return scope.kind !== "stage" || identity.stage === scope.stage;
}

function isInsightScopeIdentity(
  scope: InsightScope,
  identity: InsightScopeIdentity & { scope: InsightScope["kind"] },
): boolean {
  return identity.scope === scope.kind && isInInsightScope(scope, identity);
}

export function insightScopeOptions(
  stats: TelemetryStatsResult,
): { key: string; label: string }[] {
  return [
    insightScopeOption({ kind: "instance" }),
    ...stats.gaggles.map((item) =>
      insightScopeOption({ kind: "gaggle", gaggle: item.gaggle }),
    ),
    ...stats.runs.map((item) =>
      insightScopeOption({ kind: "workflow", gaggle: item.gaggle, workflow: item.workflow }),
    ),
    ...stats.stages.map((item) =>
      insightScopeOption({
        kind: "stage",
        gaggle: item.gaggle,
        workflow: item.workflow,
        stage: item.stage,
      }),
    ),
  ];
}

export function insightScopeOption(scope: InsightScope): { key: string; label: string } {
  return { key: insightScopeKey(scope), label: insightScopeLabel(scope) };
}

export function hasInsightScopeIdentity(scope: InsightScope): boolean {
  return scope.kind !== "instance";
}

export function deriveInsightViewModel(
  scope: InsightScope,
  snapshot: InsightSnapshot,
): InsightViewModel {
  const summary = scopeMetric(scope, snapshot.stats, snapshot.filters);
  const breakdown = outcomeBreakdown(scope, snapshot.stats, snapshot.filters);
  const stages = snapshot.stats.stages
    .filter((stage) => isInInsightScope(scope, stage))
    .filter((stage) => stage.durationSamples > 0)
    .sort(
      (left, right) =>
        (right.p95DurationMs ?? -1) - (left.p95DurationMs ?? -1) ||
        left.stage.localeCompare(right.stage),
    );
  const hasCurationHealth =
    scope.kind === "instance" &&
    (snapshot.stats.curation.runs > 0 ||
      snapshot.stats.readyPool.depth !== undefined ||
      snapshot.stats.curation.everRecorded ||
      snapshot.stats.readyPool.sampleEverRecorded ||
      snapshot.stats.readyPool.bounceEverRecorded);

  return {
    breakdown,
    creditAssignment: snapshot.stats.creditAssignment.filter((credit) =>
      isInInsightScope(scope, credit),
    ),
    curationHealth: hasCurationHealth
      ? { curation: snapshot.stats.curation, readyPool: snapshot.stats.readyPool }
      : undefined,
    filters: snapshot.filters,
    stages,
    summary,
    usage: snapshot.stats.usage.find((item) => isInsightScopeIdentity(scope, item)),
    window: snapshot.window,
  };
}

export function deriveInsightCostTrendState(
  scope: InsightScope,
  state: QueryState<InsightCostTrendSnapshot>,
): QueryState<InsightCostTrendViewModel> {
  if (state.status !== "ready" && state.status !== "stale") {
    return state;
  }
  const data = {
    points: state.data.buckets.map((bucket) => ({
      since: bucket.since,
      until: bucket.until,
      usage: bucket.usage.find((item) => isInsightScopeIdentity(scope, item)),
    })),
    previousUsage: state.data.previous?.usage.find((item) =>
      isInsightScopeIdentity(scope, item),
    ),
  };
  return state.status === "ready"
    ? { status: "ready", data }
    : { status: "stale", data, error: state.error };
}

export function insightRunFilters(
  filters: TelemetryStatsOptions,
  gaggle?: string,
  workflow?: string,
  stage?: string,
  outcome?: RunRouteFilters["outcome"],
  population?: RunRouteFilters["population"],
): RunRouteFilters {
  return {
    gaggle,
    workflow,
    stage,
    outcome,
    population,
    since: filters.since,
    until: filters.until,
  };
}

function scopeMetric(
  scope: InsightScope,
  stats: TelemetryStatsResult,
  filters: TelemetryStatsOptions,
): OutcomeMetric | undefined {
  switch (scope.kind) {
    case "instance":
      return sumGaggles(stats.gaggles, filters);
    case "gaggle": {
      const item = stats.gaggles.find((candidate) => candidate.gaggle === scope.gaggle);
      return item && gaggleMetric(item, filters);
    }
    case "workflow": {
      const item = stats.runs.find((candidate) => isInInsightScope(scope, candidate));
      return item && runMetric(item, filters);
    }
    case "stage": {
      const item = stats.stages.find((candidate) => isInInsightScope(scope, candidate));
      return item && stageMetric(item, filters);
    }
  }
}

function outcomeBreakdown(
  scope: InsightScope,
  stats: TelemetryStatsResult,
  filters: TelemetryStatsOptions,
): OutcomeMetric[] {
  switch (scope.kind) {
    case "instance":
      return stats.gaggles.map((item) => gaggleMetric(item, filters));
    case "gaggle":
      return stats.runs
        .filter((item) => isInInsightScope(scope, item))
        .map((item) => runMetric(item, filters));
    case "workflow":
      return stats.stages
        .filter((item) => isInInsightScope(scope, item))
        .map((item) => stageMetric(item, filters));
    case "stage":
      return [];
  }
}

function sumGaggles(
  gaggles: TelemetryGaggleStats[],
  filters: TelemetryStatsOptions,
): OutcomeMetric | undefined {
  if (gaggles.length === 0) return undefined;
  const total = gaggles.reduce(
    (sum, item) => ({
      completed: sum.completed + item.completedRuns,
      failed: sum.failed + item.failedRuns,
      other: sum.other + item.otherRuns,
      runs: sum.runs + item.totalRuns,
    }),
    { completed: 0, failed: 0, other: 0, runs: 0 },
  );
  const terminal = total.completed + total.failed;
  return {
    failed: total.failed,
    filters: insightRunFilters(filters),
    label: "Instance",
    other: total.other,
    successRate: terminal > 0 ? total.completed / terminal : undefined,
    succeeded: total.completed,
    total: total.runs,
    unit: "runs",
  };
}

function gaggleMetric(
  item: TelemetryGaggleStats,
  filters: TelemetryStatsOptions,
): OutcomeMetric {
  return {
    failed: item.failedRuns,
    filters: insightRunFilters(filters, item.gaggle),
    label: item.gaggle,
    other: item.otherRuns,
    successRate: item.successRate,
    succeeded: item.completedRuns,
    total: item.totalRuns,
    unit: "runs",
  };
}

function runMetric(item: TelemetryRunStats, filters: TelemetryStatsOptions): OutcomeMetric {
  return {
    failed: item.failedRuns,
    filters: insightRunFilters(filters, item.gaggle, item.workflow),
    label: `${item.gaggle} / ${item.workflow}`,
    other: item.otherRuns,
    successRate: item.successRate,
    succeeded: item.completedRuns,
    total: item.totalRuns,
    unit: "runs",
  };
}

function stageMetric(
  item: TelemetryStageStats,
  filters: TelemetryStatsOptions,
): OutcomeMetric {
  return {
    failed: item.failedAttempts,
    filters: insightRunFilters(filters, item.gaggle, item.workflow, item.stage),
    label: `${item.gaggle} / ${item.workflow} / ${item.stage}`,
    other: item.totalAttempts - item.succeededAttempts - item.failedAttempts,
    successRate: item.successRate,
    succeeded: item.succeededAttempts,
    total: item.totalAttempts,
    unit: "attempts",
  };
}
