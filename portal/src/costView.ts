import type {
  TelemetryCostAggregate,
  TelemetryCostAmount,
  TelemetryCostRunAggregate,
  TelemetryCostResult,
} from "./api/types";

export interface ExternalCostRow {
  key: string;
  label: string;
  repository?: string;
  externalKind: "pr" | "issue";
  externalId: string;
  provider: string;
  aic: string;
  aicValue?: number;
  coverage: string;
  coverageRatio: number;
  lowerBound: boolean;
  models: string[];
  runs: TelemetryCostRunAggregate[];
}

export type ExternalCostSortKey =
  | "work-item"
  | "gaggle"
  | "aic"
  | "runs";

export type ExternalCostSortDirection = "asc" | "desc";

export function externalCostGaggles(row: ExternalCostRow): string[] {
  return [...new Set(
    row.runs.map((run) => run.gaggle).filter((gaggle): gaggle is string => Boolean(gaggle)),
  )].sort((left, right) => left.localeCompare(right));
}

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
      row.aic,
      row.coverage,
      ...row.models,
      ...row.runs.flatMap((run) => [
        run.runId,
        run.gaggle ?? "",
        run.workflow ?? "",
        run.status ?? "",
        run.startedAt,
        ...run.billingModels,
        ...run.models.map((model) => model.model),
      ]),
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
    key: `${aggregate.provider}:${aggregate.repository ?? ""}:${aggregate.externalKind}:${aggregate.externalId}`,
    label: aggregate.repository
      ? `${aggregate.repository}#${aggregate.externalId}`
      : `${kind} #${aggregate.externalId}`,
    repository: aggregate.repository,
    externalKind: aggregate.externalKind,
    externalId: aggregate.externalId,
    provider: aggregate.provider,
    aic: formatAIC(aggregate.nativeTotals, aggregate.normalizedTotals, "Unmeasured"),
    aicValue: findAIC(aggregate.nativeTotals, aggregate.normalizedTotals)?.value,
    coverage: coverage.lowerBound
      ? `Lower bound: ${coverage.measuredRuns} of ${coverage.totalRuns} runs and ${coverage.measuredAttempts} of ${coverage.totalAttempts} attempts measured.`
      : `Complete coverage: ${coverage.totalRuns} runs and ${coverage.totalAttempts} attempts measured.`,
    coverageRatio:
      coverage.totalAttempts > 0 ? coverage.measuredAttempts / coverage.totalAttempts : 0,
    lowerBound: coverage.lowerBound,
    models: aggregate.models.map(
      (model) =>
        `${model.model}: ${formatAIC(model.nativeTotals, model.normalizedTotals, "unmeasured")} · ${model.measuredAttempts}/${model.usageAttempts} attempts`,
    ),
    runs: aggregate.runs,
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
    case "gaggle":
      return externalCostGaggles(left).join(", ").localeCompare(externalCostGaggles(right).join(", ")) ||
        left.label.localeCompare(right.label);
    case "aic":
      return compareOptionalNumbers(left.aicValue, right.aicValue);
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
  if (key !== "aic") {
    return 0;
  }
  const leftValue = left.aicValue;
  const rightValue = right.aicValue;
  if (leftValue === undefined) {
    return rightValue === undefined ? 0 : 1;
  }
  return rightValue === undefined ? -1 : 0;
}

function findAIC(
  native: readonly TelemetryCostAmount[],
  normalized: readonly TelemetryCostAmount[],
): TelemetryCostAmount | undefined {
  return [...native, ...normalized].find((amount) => amount.unit === "aiCredits");
}

function formatAIC(
  native: readonly TelemetryCostAmount[],
  normalized: readonly TelemetryCostAmount[],
  empty: string,
): string {
  const amount = findAIC(native, normalized);
  if (!amount) {
    return empty;
  }
  return `${formatAICNumber(amount.value)} AIC`;
}

function formatAICNumber(value: number): string {
  return new Intl.NumberFormat("en-US", { maximumFractionDigits: 4 }).format(value);
}
