import type { DaemonClient } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { ScopeStrip } from "../components/ScopeStrip";
import {
  type InsightWindow,
  useInsightCostRollup,
  useInsightCostTrend,
  useInsightExternalCosts,
  useInsightStats,
} from "../insightData";
import {
  deriveInsightCostTrendState,
  deriveInsightViewModel,
  hasInsightScopeIdentity,
  type InsightScope,
  insightScopeApiParameters,
  insightScopeFromKey,
  insightScopeFromRoute,
  insightScopeKey,
  insightScopeOption,
  insightScopeOptions,
  insightScopeRouteFilters,
} from "../insightScope";
import { routeHash, type Navigate } from "../routing";
import type { ScopeFilters } from "../scope";
import {
  CostTrend,
  ExternalCostBreakdown,
  INSIGHT_WINDOWS,
  InstanceCostRollup,
  UsageAnalytics,
} from "./InsightPage";

export function CostPage({
  client,
  filters,
  navigate,
  standalone,
}: {
  client: DaemonClient;
  filters?: ScopeFilters;
  navigate: Navigate;
  standalone: boolean;
}) {
  const window = filters?.window ?? "7d";
  const requestedScope = insightScopeFromRoute(filters);
  const scope = insightScopeApiParameters(requestedScope);
  const setScope = (nextScope: InsightScope) =>
    navigate({ page: "cost", filters: insightScopeRouteFilters(nextScope, window) });
  const setWindow = (nextWindow: InsightWindow) =>
    navigate({ page: "cost", filters: insightScopeRouteFilters(requestedScope, nextWindow) });

  const query = useInsightStats(client, window, scope.gaggle, scope.workflow);
  const costTrend = useInsightCostTrend(client, window, scope.gaggle, scope.workflow);
  const costRollup = useInsightCostRollup(client, window);
  const externalCosts = useInsightExternalCosts(client, window);

  if (query.state.status === "loading") {
    return <DaemonLoadingState standalone={standalone} />;
  }
  if (query.state.status === "error") {
    return <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />;
  }
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  const snapshot = query.state.data;
  const availableScopes = insightScopeOptions(snapshot.stats);
  const scopes = availableScopes.some((option) => option.key === insightScopeKey(requestedScope))
    ? availableScopes
    : [...availableScopes, insightScopeOption(requestedScope)];
  const view = deriveInsightViewModel(requestedScope, snapshot);
  const costTrendView = deriveInsightCostTrendState(requestedScope, costTrend.state);

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Telemetry</p>
        <h1>Cost</h1>
        <p>
          Instance spend, selected-scope AI cost, retry waste, and attributed pull request and
          issue costs.
        </p>
      </header>

      <div className="insight-controls" aria-label="Cost filters">
        <label>
          <span>Scope</span>
          <select
            aria-label="Scope"
            onChange={(event) => setScope(insightScopeFromKey(event.target.value))}
            value={insightScopeKey(requestedScope)}
          >
            {scopes.map((option) => (
              <option key={option.key} value={option.key}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
        <label>
          <span>Time window</span>
          <select
            aria-label="Time window"
            onChange={(event) => setWindow(event.target.value as InsightWindow)}
            value={window}
          >
            {INSIGHT_WINDOWS.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
      </div>

      {hasInsightScopeIdentity(requestedScope) && (
        <ScopeStrip
          ariaLabel="Cost scope"
          clearHref={routeHash({
            page: "cost",
            filters: insightScopeRouteFilters({ kind: "instance" }, window),
          })}
          filters={scope}
        />
      )}

      {query.state.status === "stale" && query.state.error && (
        <div className="insight-stale-error" role="alert">
          Cost telemetry refresh failed. Showing the last successful snapshot for this window.
        </div>
      )}

      <InstanceCostRollup
        costRollup={costRollup.state}
        refreshing={costRollup.refreshing}
        retry={costRollup.retry}
        window={window}
      />

      {view.usage && (
        <section className="content-section">
          <div className="section-heading">
            <div>
              <p className="section-kicker">AI usage</p>
              <h2>Selected-scope cost</h2>
            </div>
            <span className="section-count">Measured attempts only</span>
          </div>
          <p className="usage-description">
            Cost measurements are aggregated for the selected scope. Runners that do not report
            usage remain unmeasured.
          </p>
          <UsageAnalytics filters={view.filters} mode="cost" usage={view.usage} />
          <CostTrend
            costTrend={costTrendView}
            currentUsage={view.usage}
            refreshing={costTrend.refreshing}
            retry={costTrend.retry}
            window={window}
          />
        </section>
      )}

      <ExternalCostBreakdown
        costs={externalCosts.state}
        refreshing={externalCosts.refreshing}
        retry={externalCosts.retry}
      />
    </>
  );
}
