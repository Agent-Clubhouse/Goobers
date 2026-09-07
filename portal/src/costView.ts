import type {
  TelemetryCostAggregate,
  TelemetryCostAmount,
  TelemetryCostResult,
} from "./api/types";

export interface ExternalCostRow {
  key: string;
  label: string;
  provider: string;
  native: string;
  normalized: string;
  coverage: string;
  lowerBound: boolean;
  models: string[];
  runs: string[];
}

export function deriveExternalCostRows(result: TelemetryCostResult): ExternalCostRow[] {
  return [...result.pullRequests, ...result.issues].map((aggregate) =>
    externalCostRow(aggregate),
  );
}

function externalCostRow(aggregate: TelemetryCostAggregate): ExternalCostRow {
  const kind = aggregate.externalKind === "pr" ? "PR" : "Issue";
  const coverage = aggregate.coverage;
  return {
    key: `${aggregate.provider}:${aggregate.externalKind}:${aggregate.externalId}`,
    label: `${kind} #${aggregate.externalId}`,
    provider: aggregate.provider,
    native: formatAmounts(aggregate.nativeTotals, "Unmeasured"),
    normalized: formatAmounts(aggregate.normalizedTotals, "Unavailable"),
    coverage: coverage.lowerBound
      ? `Lower bound: ${coverage.measuredRuns} of ${coverage.totalRuns} runs and ${coverage.measuredAttempts} of ${coverage.totalAttempts} attempts measured.`
      : `Complete coverage: ${coverage.totalRuns} runs and ${coverage.totalAttempts} attempts measured.`,
    lowerBound: coverage.lowerBound,
    models: aggregate.models.map(
      (model) =>
        `${model.model}: ${formatAmounts(model.nativeTotals, "unmeasured")} · ${model.measuredAttempts}/${model.usageAttempts} attempts`,
    ),
    runs: aggregate.runs.map(
      (run) =>
        `${run.runId}: ${formatAmounts(run.nativeTotals, "unmeasured")} · ${run.measuredAttempts}/${run.usageAttempts} attempts`,
    ),
  };
}

function formatAmounts(
  amounts: readonly TelemetryCostAmount[],
  empty: string,
): string {
  if (amounts.length === 0) {
    return empty;
  }
  return amounts.map(formatAmount).join(" · ");
}

function formatAmount(amount: TelemetryCostAmount): string {
  let value: string;
  switch (amount.unit) {
    case "aiCredits":
      value = `${formatNumber(amount.value)} AI credits`;
      break;
    case "usd":
      value = new Intl.NumberFormat("en-US", {
        style: "currency",
        currency: "USD",
        minimumFractionDigits: 2,
        maximumFractionDigits: 4,
      }).format(amount.value);
      break;
    case "premiumRequests":
      value = `${formatNumber(amount.value)} premium requests`;
      break;
  }
  return amount.estimated ? `${value} estimated` : value;
}

function formatNumber(value: number): string {
  return new Intl.NumberFormat("en-US", { maximumFractionDigits: 4 }).format(value);
}
