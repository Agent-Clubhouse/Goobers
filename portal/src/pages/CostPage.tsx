import { PageHeading } from "../ui/Heading";
import type { DaemonClient, TelemetryUsageStats } from "../api/types";
import { DaemonErrorState } from "../components/DaemonQueryState";
import { SectionQueryStatus } from "../components/SectionQueryStatus";
import { InsightFilters } from "../components/InsightFilters";
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
  InstanceCostRollup,
  UsageAnalytics,
} from "./InsightPage";

const LOADING_USAGE: TelemetryUsageStats = {
  scope: "instance",
  totalAttempts: 0,
  tokenSamples: 0,
  premiumRequestSamples: 0,
  costSamples: 0,
  retryWasteAttempts: 0,
};

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
  const externalCosts = useInsightExternalCosts(
    client,
    window,
    scope.gaggle,
    scope.workflow,
    scope.stage,
  );

  const snapshot =
    query.state.status === "ready" || query.state.status === "stale" ? query.state.data : undefined;
  const availableScopes = snapshot
    ? insightScopeOptions(snapshot.stats)
    : [insightScopeOption({ kind: "instance" })];
  const scopes = availableScopes.some((option) => option.key === insightScopeKey(requestedScope))
    ? availableScopes
    : [...availableScopes, insightScopeOption(requestedScope)];
  const view = snapshot ? deriveInsightViewModel(requestedScope, snapshot) : undefined;
  const costTrendView = deriveInsightCostTrendState(requestedScope, costTrend.state);

  if (query.state.status === "error") {
    return (
      <DaemonErrorState
        error={query.state.error}
        retry={() => {
          query.retry();
          costTrend.retry();
          costRollup.retry();
          externalCosts.retry();
        }}
        standalone={standalone}
      />
    );
  }

  return (
    <>
      <PageHeading
        title="Cost"
        className=""
        description="Instance spend, selected-scope cost, retry waste, and attributed pull request and issue costs."
        titleActions={
          <>
            <div className="section-heading-meta">
              <SectionQueryStatus
                error={query.state.status === "stale" && Boolean(query.state.error)}
                loading={query.state.status === "loading" || query.refreshing}
                message={
                  query.state.status === "stale" && query.state.error
                    ? "Cost refresh failed."
                    : query.state.status === "loading"
                      ? "Loading cost summary…"
                      : query.refreshing
                        ? "Refreshing cost summary…"
                        : undefined
                }
                retry={query.retry}
              />
            </div>
          </>
        }
      />

      <InsightFilters
        label="Cost filters"
        onScopeChange={setScope}
        onWindowChange={setWindow}
        scope={requestedScope}
        scopes={scopes}
        window={window}
      />

      <section className="content-section">
        <div className="cost-summary-metrics">
          <div
            aria-hidden={!view?.usage || undefined}
            className={!view?.usage ? "cost-summary-placeholder" : undefined}
          >
            <UsageAnalytics
              filters={view?.filters ?? {}}
              mode="cost"
              totalRuns={
                requestedScope.kind === "stage"
                  ? undefined
                  : (view?.summary?.total ?? (snapshot ? undefined : 0))
              }
              usage={view?.usage ?? LOADING_USAGE}
            />
          </div>
          {snapshot && !view?.usage && (
            <div className="cost-summary-status">
              <SectionQueryStatus message="No cost measurements in this scope." />
            </div>
          )}
        </div>
        <CostTrend
          costTrend={costTrendView}
          currentUsage={view?.usage ?? LOADING_USAGE}
          refreshing={costTrend.refreshing}
          retry={costTrend.retry}
          window={window}
        />
      </section>

      {requestedScope.kind === "instance" && (
        <InstanceCostRollup
          costRollup={costRollup.state}
          refreshing={costRollup.refreshing}
          retry={costRollup.retry}
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
