import type { OutcomeFilter, StagePopulationFilter } from "./api/types";
import type { InsightWindow } from "./insightData";
import { formatTimestamp } from "./runDetailData";

// The single scope/filter model shared by Runs, Insight, and Errors (#2528).
// gaggle/workflow/stage/since/until/window are the cross-view "scope"; outcome
// and population are Runs/Insight-specific refinements carried on the same
// shape so drill-through links can be built with one function instead of
// three parallel ones. A view that has no use for a field simply never reads
// or writes it — that isn't a new dimension, just an unused optional field.
export interface ScopeFilters {
  gaggle?: string;
  workflow?: string;
  stage?: string;
  outcome?: OutcomeFilter;
  population?: StagePopulationFilter;
  since?: string;
  until?: string;
  window?: InsightWindow;
}

export function hasScopeFilters(filters: ScopeFilters): boolean {
  return Object.values(filters).some(Boolean);
}

// The gaggle/workflow/stage identity, independent of any view-specific
// refinement (outcome, population, window) or absolute time range — this is
// what "the scope" means when preserving it across Runs <-> Insight
// navigation (#2528 acceptance criterion 4).
export function scopeIdentity(
  filters: ScopeFilters | undefined,
): Pick<ScopeFilters, "gaggle" | "workflow" | "stage"> {
  return { gaggle: filters?.gaggle, workflow: filters?.workflow, stage: filters?.stage };
}

export function hasScopeIdentity(filters: ScopeFilters): boolean {
  return Boolean(filters.gaggle || filters.workflow || filters.stage);
}

export function scopeLabel(filters: ScopeFilters): string {
  if (filters.stage) {
    return `${filters.gaggle ?? "All gaggles"} / ${filters.workflow ?? "All workflows"} / ${filters.stage}`;
  }
  if (filters.workflow) {
    return `${filters.gaggle ?? "All gaggles"} / ${filters.workflow}`;
  }
  return filters.gaggle ? `Gaggle: ${filters.gaggle}` : "Instance";
}

export function scopeWindowLabel(filters: ScopeFilters): string {
  if (filters.since && filters.until) {
    return ` from ${formatTimestamp(filters.since)} to ${formatTimestamp(filters.until)}`;
  }
  if (filters.since) {
    return ` since ${formatTimestamp(filters.since)}`;
  }
  if (filters.until) {
    return ` through ${formatTimestamp(filters.until)}`;
  }
  return "";
}
