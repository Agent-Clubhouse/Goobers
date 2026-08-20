import { scopeLabel, type ScopeFilters } from "../scope";

// The one "current scope + clear filters" affordance shared by Runs, Insight,
// and Errors (#2528) — previously three independently-styled/worded
// implementations (RunsPage's "Clear Insight scope" link, ErrorsPage's "Back
// to Insight" link, and no equivalent at all on InsightPage).
export function ScopeStrip({
  ariaLabel,
  clearHref,
  clearLabel = "Clear filters",
  filters,
  onClear,
  suffix,
}: {
  ariaLabel: string;
  clearHref: string;
  clearLabel?: string;
  filters: ScopeFilters;
  onClear?: () => void;
  suffix?: string;
}) {
  return (
    <div aria-label={ariaLabel} className="run-scope-strip">
      <strong>
        {scopeLabel(filters)}
        {suffix}
      </strong>
      <a href={clearHref} onClick={onClear}>
        {clearLabel}
      </a>
    </div>
  );
}
