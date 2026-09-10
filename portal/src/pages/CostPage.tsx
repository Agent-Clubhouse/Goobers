import type { DaemonClient } from "../api/types";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
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
  type InsightScope,
  insightScopeApiParameters,
  insightScopeFromKey,
  insightScopeFromRoute,
  insightScopeKey,
  insightScopeOption,
  insightScopeOptions,
  insightScopeRouteFilters,
} from "../insightScope";
import type { Navigate } from "../routing";
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
        <h1>Cost</h1>
        <p>
          Instance spend, selected-scope AI cost, retry waste, and attributed pull request and
          issue costs.
        </p>
      </header>

      <div className="insight-controls" aria-label="Cost filters">
        <label>
          <span>Scope</span>
          <CostScopeSelect
            onChange={(key) => setScope(insightScopeFromKey(key))}
            scopes={scopes}
            value={insightScopeKey(requestedScope)}
          />
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

      {query.state.status === "stale" && query.state.error && (
        <div className="insight-stale-error" role="alert">
          Cost telemetry refresh failed. Showing the last successful snapshot for this window.
        </div>
      )}

      {view.usage && (
        <section className="content-section">
          <div className="section-heading">
            <h2>Cost summary</h2>
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

      {requestedScope.kind === "instance" && (
        <InstanceCostRollup
          costRollup={costRollup.state}
          refreshing={costRollup.refreshing}
          retry={costRollup.retry}
          window={window}
        />
      )}

      <ExternalCostBreakdown
        costs={externalCosts.state}
        refreshing={externalCosts.refreshing}
        retry={externalCosts.retry}
      />
    </>
  );
}

function CostScopeSelect({
  onChange,
  scopes,
  value,
}: {
  onChange: (value: string) => void;
  scopes: { key: string; label: string }[];
  value: string;
}) {
  const parsed = scopes.map((option) => ({
    ...option,
    scope: insightScopeFromKey(option.key),
  }));
  const instance = parsed.find(({ scope }) => scope.kind === "instance");
  const gaggles = parsed
    .filter(({ scope }) => scope.kind === "gaggle")
    .map(({ scope }) => scope)
    .filter((scope): scope is Extract<InsightScope, { kind: "gaggle" }> => scope.kind === "gaggle");

  return (
    <select aria-label="Scope" onChange={(event) => onChange(event.target.value)} value={value}>
      {instance && <option value={instance.key}>Instance</option>}
      {gaggles.map((gaggle) => {
        const gaggleOption = parsed.find(
          ({ scope }) => scope.kind === "gaggle" && scope.gaggle === gaggle.gaggle,
        );
        const descendants = parsed.filter(
          ({ scope }) => scope.kind !== "instance" && scope.gaggle === gaggle.gaggle,
        );
        return (
          <optgroup key={gaggle.gaggle} label={gaggle.gaggle}>
            {gaggleOption && (
              <option value={gaggleOption.key}>All {gaggle.gaggle}</option>
            )}
            {descendants
              .filter(({ scope }) => scope.kind === "workflow")
              .map(({ key, scope }) => {
                if (scope.kind !== "workflow") return null;
                return (
                  <option key={key} value={key}>
                    Workflow · {scope.workflow}
                  </option>
                );
              })}
            {descendants
              .filter(({ scope }) => scope.kind === "stage")
              .map(({ key, scope }) => {
                if (scope.kind !== "stage") return null;
                return (
                  <option key={key} value={key}>
                    Stage · {scope.workflow} / {scope.stage}
                  </option>
                );
              })}
          </optgroup>
        );
      })}
    </select>
  );
}
