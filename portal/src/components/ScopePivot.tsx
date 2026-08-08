import { routeHash } from "../routing";
import type { ScopeFilters } from "../scope";
import { Icon } from "../ui/Icon";

// The single, discoverable "view this gaggle/workflow's Runs / Insight"
// affordance (#2529) — every place a gaggle or workflow name/badge is
// rendered gets one of these instead of scoping only being reachable via
// URL params passed at Insight drill-through time. `since`/`until`/`window`
// are accepted so a caller that does have an active time window in scope can
// carry it forward instead of resetting to "all time" (acceptance criterion
// 2); the four render sites named in the issue have no such window today, so
// they simply omit those fields.
export function ScopePivot({
  label,
  scope,
}: {
  label: string;
  scope: Pick<ScopeFilters, "gaggle" | "workflow" | "since" | "until" | "window">;
}) {
  return (
    <span aria-label={`Pivot ${label} into`} className="scope-pivot" role="group">
      <a
        aria-label={`View ${label} in Runs`}
        className="scope-pivot-link"
        href={routeHash({ page: "runs", filters: scope })}
      >
        <Icon name="run" size={13} />
        Runs
      </a>
      <a
        aria-label={`View ${label} in Insight`}
        className="scope-pivot-link"
        href={routeHash({ page: "insight", filters: scope })}
      >
        <Icon name="insight" size={13} />
        Insight
      </a>
    </span>
  );
}
