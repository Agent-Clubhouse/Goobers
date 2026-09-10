import type {
  TelemetryCostAggregate,
  TelemetryCostAmount,
  TelemetryCostResult,
} from "./api/types";

export interface ExternalCostRow {
  key: string;
  label: string;
  externalKind: "pr" | "issue";
  externalId: string;
  provider: string;
  native: string;
  nativeValue?: number;
  normalized: string;
  normalizedValue?: number;
  coverage: string;
  coverageRatio: number;
  lowerBound: boolean;
  models: string[];
  runs: string[];
}

export type ExternalCostSortKey =
  | "work-item"
  | "provider"
  | "native"
  | "normalized"
  | "coverage"
  | "runs";

export type ExternalCostSortDirection = "asc" | "desc";

export function deriveExternalCostRows(result: TelemetryCostResult): ExternalCostRow[] {
  return [...result.pullRequests, ...result.issues].map((aggregate) =>
    externalCostRow(aggregate),
  );
}

export function filterExternalCostRows(
  rows: readonly ExternalCostRow[],
  query: string,
  kind: "all" | "pr" | "issue",
): ExternalCostRow[] {
  const normalizedQuery = query.trim().toLocaleLowerCase();
  return rows.filter((row) => {
    if (kind !== "all" && row.externalKind !== kind) {
      return false;
    }
    if (normalizedQuery === "") {
      return true;
    }
    return [
      row.label,
      row.provider,
      row.native,
      row.normalized,
      row.coverage,
      ...row.models,
      ...row.runs,
    ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery));
  });
}

export function sortExternalCostRows(
  rows: readonly ExternalCostRow[],
  key: ExternalCostSortKey,
  direction: ExternalCostSortDirection,
): ExternalCostRow[] {
  return [...rows].sort((left, right) => {
    const missingValueOrder = compareMissingValues(left, right, key);
    if (missingValueOrder !== 0) {
      return missingValueOrder;
    }
    const comparison = compareExternalCostRows(left, right, key);
    return direction === "asc" ? comparison : -comparison;
  });
}

function externalCostRow(aggregate: TelemetryCostAggregate): ExternalCostRow {
  const kind = aggregate.externalKind === "pr" ? "PR" : "Issue";
  const coverage = aggregate.coverage;
  return {
    key: `${aggregate.provider}:${aggregate.externalKind}:${aggregate.externalId}`,
    label: `${kind} #${aggregate.externalId}`,
    externalKind: aggregate.externalKind,
    externalId: aggregate.externalId,
    provider: aggregate.provider,
    native: formatAmounts(aggregate.nativeTotals, "Unmeasured"),
    nativeValue: aggregate.nativeTotals[0]?.value,
    normalized: formatAmounts(aggregate.normalizedTotals, "Unavailable"),
    normalizedValue: aggregate.normalizedTotals[0]?.value,
    coverage: coverage.lowerBound
      ? `Lower bound: ${coverage.measuredRuns} of ${coverage.totalRuns} runs and ${coverage.measuredAttempts} of ${coverage.totalAttempts} attempts measured.`
      : `Complete coverage: ${coverage.totalRuns} runs and ${coverage.totalAttempts} attempts measured.`,
    coverageRatio:
      coverage.totalAttempts > 0 ? coverage.measuredAttempts / coverage.totalAttempts : 0,
    lowerBound: coverage.lowerBound,
    models: aggregate.models.map(
      (model) =>
        `${model.model}: ${formatAmounts(model.nativeTotals, "unmeasured")} · ${model.measuredAttempts}/${model.usageAttempts} attempts`,
    ),
    runs: aggregate.runs.map((run) => run.runId),
  };
}

function compareExternalCostRows(
  left: ExternalCostRow,
  right: ExternalCostRow,
  key: ExternalCostSortKey,
): number {
  switch (key) {
    case "work-item":
      return (
        left.externalKind.localeCompare(right.externalKind) ||
        compareExternalIds(left.externalId, right.externalId)
      );
    case "provider":
      return left.provider.localeCompare(right.provider) || left.label.localeCompare(right.label);
    case "native":
      return compareOptionalNumbers(left.nativeValue, right.nativeValue);
    case "normalized":
      return compareOptionalNumbers(left.normalizedValue, right.normalizedValue);
    case "coverage":
      return left.coverageRatio - right.coverageRatio || left.label.localeCompare(right.label);
    case "runs":
      return left.runs.length - right.runs.length || left.label.localeCompare(right.label);
  }
}

function compareExternalIds(left: string, right: string): number {
  const leftNumber = Number(left);
  const rightNumber = Number(right);
  if (Number.isFinite(leftNumber) && Number.isFinite(rightNumber)) {
    return leftNumber - rightNumber;
  }
  return left.localeCompare(right);
}

function compareOptionalNumbers(left: number | undefined, right: number | undefined): number {
  return (left ?? 0) - (right ?? 0);
}

function compareMissingValues(
  left: ExternalCostRow,
  right: ExternalCostRow,
  key: ExternalCostSortKey,
): number {
  if (key !== "native" && key !== "normalized") {
    return 0;
  }
  const leftValue = key === "native" ? left.nativeValue : left.normalizedValue;
  const rightValue = key === "native" ? right.nativeValue : right.normalizedValue;
  if (leftValue === undefined) {
    return rightValue === undefined ? 0 : 1;
  }
  return rightValue === undefined ? -1 : 0;
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
