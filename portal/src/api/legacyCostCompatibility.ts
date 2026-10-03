import type {
  TelemetryModelStats,
  TelemetryStageStats,
  TelemetryStatsResult,
  TelemetryUsageStats,
  WorkItemDetail,
} from "./types";

const AIC_PER_USD = 100;
const NANO_AIU_PER_USD = 100_000_000_000;

// Remove these wire fallbacks after the v0.5 compatibility window closes (#6687).
export function normalizeLegacyTelemetryCosts(
  result: TelemetryStatsResult,
): TelemetryStatsResult {
  return {
    ...result,
    ...(Array.isArray(result.stages)
      ? { stages: result.stages.map(normalizeStageCosts) }
      : {}),
    ...(Array.isArray(result.usage)
      ? { usage: result.usage.map(normalizeUsageCosts) }
      : {}),
    ...(Array.isArray(result.models)
      ? { models: result.models.map(normalizeModelCosts) }
      : {}),
    trend: result.trend?.map((bucket) => ({
      ...bucket,
      usage: bucket.usage.map(normalizeUsageCosts),
    })),
    trendPrevious: result.trendPrevious && {
      ...result.trendPrevious,
      usage: result.trendPrevious.usage.map(normalizeUsageCosts),
    },
  };
}

export function normalizeLegacyWorkItemCost(
  detail: WorkItemDetail,
): WorkItemDetail {
  if (
    !detail.cost ||
    detail.cost.nanoAIU !== undefined ||
    detail.cost.costUSD === undefined
  ) {
    return detail;
  }
  return {
    ...detail,
    cost: {
      ...detail.cost,
      nanoAIU: detail.cost.costUSD * NANO_AIU_PER_USD,
    },
  };
}

function normalizeStageCosts(stage: TelemetryStageStats): TelemetryStageStats {
  return {
    ...stage,
    p50CostAIC: preferAIC(stage.p50CostAIC, stage.p50CostUSD),
    p95CostAIC: preferAIC(stage.p95CostAIC, stage.p95CostUSD),
    retryWasteCostAIC: preferAIC(
      stage.retryWasteCostAIC,
      stage.retryWasteCostUSD,
    ),
  };
}

function normalizeUsageCosts(usage: TelemetryUsageStats): TelemetryUsageStats {
  return {
    ...usage,
    costAIC: preferAIC(usage.costAIC, usage.costUSD),
    p50CostAIC: preferAIC(usage.p50CostAIC, usage.p50CostUSD),
    p95CostAIC: preferAIC(usage.p95CostAIC, usage.p95CostUSD),
    retryWasteCostAIC: preferAIC(
      usage.retryWasteCostAIC,
      usage.retryWasteCostUSD,
    ),
  };
}

function normalizeModelCosts(model: TelemetryModelStats): TelemetryModelStats {
  return {
    ...model,
    costAIC: preferAIC(model.costAIC, model.costUSD),
  };
}

function preferAIC(
  current: number | undefined,
  legacyUSD: number | undefined,
): number | undefined {
  return current ?? (legacyUSD === undefined ? undefined : legacyUSD * AIC_PER_USD);
}
