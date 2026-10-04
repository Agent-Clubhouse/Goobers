import { PageHeading, SectionHeading } from "../ui/Heading";
import { Timestamp } from "../ui/Timestamp";
import { FilterField, FilterOption, FilterOptions } from "../ui/Filters";
import { Action } from "../ui/Action";
import { useEffect, useId, useMemo, useRef, useState } from "react";
import type {
  DaemonClient,
  TelemetryErrorSignature,
  TelemetryCurationStats,
  NodeCredit,
  TelemetryCostAmount,
  TelemetryCostRunAggregate,
  TelemetryReadyPool,
  TelemetryStageStats,
  TelemetryStatsOptions,
  TelemetryUsageStats,
} from "../api/types";
import { isMissingCostCapability } from "../api/errors";
import type { QueryState } from "../api/queryState";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { SectionQueryStatus } from "../components/SectionQueryStatus";
import { InsightFilters } from "../components/InsightFilters";
import {
  type InsightCostRollupSnapshot,
  type InsightErrorSignaturesSnapshot,
  type InsightExternalCostSnapshot,
  type InsightGaggleSpend,
  type InsightWindow,
  useInsightErrorSignatures,
  useInsightStats,
} from "../insightData";
import {
  deriveExternalCostRows,
  externalCostGaggles,
  filterExternalCostRows,
  sortExternalCostRows,
  type ExternalCostSortDirection,
  type ExternalCostSortKey,
} from "../costView";
import {
  deriveInsightViewModel,
  type InsightCostTrendViewModel,
  type InsightScope,
  type InsightViewModel,
  insightRunFilters,
  insightScopeApiParameters,
  insightScopeFromRoute,
  insightScopeKey,
  insightScopeOption,
  insightScopeOptions,
  insightScopeRouteFilters,
  type OutcomeMetric,
} from "../insightScope";
import {
  routeHash,
  type ErrorRouteFilters,
  type InsightRouteFilters,
  type Navigate,
  type RunRouteFilters,
} from "../routing";
import { formatDuration, formatTimestamp } from "../runDetailData";
import { Icon } from "../ui/Icon";

const INITIAL_DETAIL_ROWS = 5;

export function InsightPage({
  client,
  filters,
  navigate,
  standalone,
}: {
  client: DaemonClient;
  filters?: InsightRouteFilters;
  navigate: Navigate;
  standalone: boolean;
}) {
  const window = filters?.window ?? "7d";
  const requestedScope = insightScopeFromRoute(filters);
  const routeFilters = (scope: InsightScope, nextWindow: InsightWindow) =>
    insightScopeRouteFilters(scope, nextWindow);
  const setScope = (nextScope: InsightScope) =>
    navigate({ page: "insight", filters: routeFilters(nextScope, window) });
  const setWindow = (nextWindow: InsightWindow) =>
    navigate({ page: "insight", filters: routeFilters(requestedScope, nextWindow) });
  const errorScope = insightScopeApiParameters(requestedScope);
  const query = useInsightStats(client, window, errorScope.gaggle, errorScope.workflow);
  const errorSignatures = useInsightErrorSignatures(
    client,
    window,
    errorScope.gaggle,
    errorScope.workflow,
    errorScope.stage,
    query.state.status !== "loading",
  );

  if (query.state.status === "loading") {
    return <DaemonLoadingState standalone={standalone} />;
  }
  if (query.state.status === "error") {
    return (
      <DaemonErrorState error={query.state.error} retry={query.retry} standalone={standalone} />
    );
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
  return (
    <>
      <PageHeading title="Insight" className="" description="Run outcomes, usage, and latency." />

      <InsightFilters
        label="Insight filters"
        onScopeChange={setScope}
        onWindowChange={setWindow}
        scope={requestedScope}
        scopes={scopes}
        window={window}
      />

      {query.state.status === "stale" && query.state.error && (
        <SectionQueryStatus
          error
          message="Telemetry refresh failed. Showing the last successful snapshot for this window."
          retry={query.retry}
        />
      )}

      <InsightContent
        errorSignatures={errorSignatures.state}
        errorSignaturesRetry={errorSignatures.retry}
        view={view}
      />
    </>
  );
}

function InsightContent({
  errorSignatures,
  errorSignaturesRetry,
  view,
}: {
  errorSignatures: QueryState<InsightErrorSignaturesSnapshot>;
  errorSignaturesRetry: () => void;
  view: InsightViewModel;
}) {
  const { breakdown, creditAssignment, curationHealth, filters, stages, summary, usage } = view;
  const hasOutcomes = Boolean(summary) || breakdown.length > 0;
  const hasFailureReasons =
    (errorSignatures.status === "ready" || errorSignatures.status === "stale") &&
    errorSignatures.data.result.items.length > 0;
  const failureReasonsFailed =
    errorSignatures.status === "error" ||
    (errorSignatures.status === "stale" && Boolean(errorSignatures.error));

  const isEmpty =
    !hasOutcomes &&
    creditAssignment.length === 0 &&
    !usage &&
    stages.length === 0 &&
    !hasFailureReasons &&
    !failureReasonsFailed &&
    !curationHealth &&
    errorSignatures.status !== "loading";

  return (
    <>
      {isEmpty ? (
        <section className="empty-state insight-empty">
          <span className="insight-empty-icon">
            <Icon name="insight" size={24} />
          </span>
          <div>
            <h2>No telemetry in this window</h2>
            <p>Choose a wider time window or another scope to inspect recorded runs.</p>
          </div>
        </section>
      ) : (
        <>
          {hasOutcomes && (
            <section className="content-section">
              <SectionHeading
                title="Success and failure"
                className=""
                actions={
                  <>
                    <span className="section-count">Terminal outcomes exclude other states</span>
                  </>
                }
              />
              <div className="data-table-shell insight-outcomes">
                <div aria-hidden="true" className="data-table-header insight-outcome-header">
                  <span>Scope</span>
                  <span>Success rate</span>
                  <span>Succeeded</span>
                  <span>Failed</span>
                  <span>Other</span>
                  <span>Total</span>
                </div>
                {summary && <OutcomeRow emphasis metric={summary} />}
                {breakdown.map((metric) => (
                  <OutcomeRow key={`${metric.unit}:${metric.label}`} metric={metric} />
                ))}
              </div>
            </section>
          )}

          {curationHealth && (
            <CurationHealth
              curation={curationHealth.curation}
              readyPool={curationHealth.readyPool}
            />
          )}

          {creditAssignment.length > 0 && (
            <InsightReportSection title="Highest-contributing nodes">
              <CreditAssignment credits={creditAssignment} filters={filters} />
            </InsightReportSection>
          )}

          {usage && (
            <InsightReportSection title="Tokens and retry waste">
              <UsageAnalytics
                filters={filters}
                mode="insight"
                totalRuns={summary?.unit === "runs" ? summary.total : undefined}
                usage={usage}
              />
            </InsightReportSection>
          )}

          <InsightReportSection title="Failure reasons">
            <FailureReasonBreakdown retry={errorSignaturesRetry} state={errorSignatures} />
          </InsightReportSection>

          {(hasOutcomes || stages.length > 0) && (
            <InsightReportSection title="Slowest stages">
              {stages.length === 0 ? (
                <p className="inline-empty">No stage duration samples in this scope.</p>
              ) : (
                <StageDistributions filters={filters} stages={stages} />
              )}
            </InsightReportSection>
          )}
        </>
      )}
    </>
  );
}

function InsightReportSection({ children, title }: { children: React.ReactNode; title: string }) {
  return (
    <section className="content-section insight-report-section">
      <SectionHeading title={title} className="" />
      {children}
    </section>
  );
}

function CreditAssignment({
  credits,
  filters,
}: {
  credits: NodeCredit[];
  filters: TelemetryStatsOptions;
}) {
  const [showAll, setShowAll] = useState(false);
  const visibleCredits = showAll ? credits : credits.slice(0, INITIAL_DETAIL_ROWS);
  return (
    <>
      <p className="usage-description">Failure, escalation, and retry-waste contributors.</p>
      <div className="data-table-shell insight-outcomes">
        <div
          aria-hidden="true"
          className="credit-assignment-row credit-assignment-header data-table-header"
        >
          <span>Node</span>
          <span>Failure share</span>
          <span>Failures</span>
          <span>Escalations</span>
          <span>Retry waste</span>
        </div>
        {visibleCredits.map((credit) => (
          <a
            aria-label={`View runs behind ${credit.gaggle} ${credit.workflow} ${credit.stage}: ${credit.failureRuns} failures, ${credit.escalationRuns} escalations, ${credit.retryWasteAttempts} wasted attempts`}
            className="credit-assignment-row credit-assignment-link"
            href={routeHash({
              page: "runs",
              filters: insightRunFilters(
                filters,
                credit.gaggle,
                credit.workflow,
                credit.kind === "stage" ? credit.stage : undefined,
              ),
            })}
            key={`${credit.gaggle}:${credit.workflow}:${credit.kind}:${credit.stage}:${credit.identity ?? ""}`}
          >
            <span className="distribution-name">
              <strong>{credit.stage}</strong>
              <small>
                {credit.kind} · {credit.gaggle} / {credit.workflow} · {credit.routedRuns} routed
                runs
              </small>
            </span>
            <strong>{formatRate(credit.failureShare)}</strong>
            <strong>{credit.failureRuns}</strong>
            <strong>{credit.escalationRuns}</strong>
            <strong>{credit.retryWasteAttempts}</strong>
          </a>
        ))}
        {credits.length > INITIAL_DETAIL_ROWS && (
          <button
            className="data-table-disclosure"
            onClick={() => setShowAll((value) => !value)}
            type="button"
          >
            {showAll ? "Show fewer contributors" : `View all ${credits.length} contributors`}
          </button>
        )}
      </div>
    </>
  );
}

function CurationHealth({
  curation,
  readyPool,
}: {
  curation: TelemetryCurationStats;
  readyPool: TelemetryReadyPool;
}) {
  const depth = readyPool.depth;
  return (
    <section className="content-section">
      <SectionHeading title="Ready-pool health" className="" />
      <dl className="curation-health">
        <div>
          <dt>Ready depth</dt>
          <dd className={readyPool.starved ? "curation-health-alert" : undefined}>
            {depth === undefined
              ? unmeasuredLabel(readyPool.sampleEverRecorded)
              : readyPool.starved
                ? "0 · Starved"
                : depth}
          </dd>
        </div>
        <div>
          <dt>Oldest ready</dt>
          <dd>{formatSeconds(readyPool.oldestAgeSeconds, readyPool.sampleEverRecorded)}</dd>
        </div>
        <div>
          <dt>Age before claim</dt>
          <dd>{formatSeconds(readyPool.averageClaimAgeSeconds, true)}</dd>
        </div>
        <div>
          <dt title="Time since claim for implementation items currently in progress">
            In flight now
          </dt>
          <dd>
            {readyPool.inFlightClaimSamples === 0
              ? "0"
              : `${formatDuration(readyPool.averageInFlightClaimAgeSeconds * 1_000)} average · ${readyPool.inFlightClaimSamples} claimed`}
          </dd>
        </div>
        <div>
          <dt title="Share of items marked ready in the selected window that later moved to not-ready">
            Bounce rate
          </dt>
          <dd>
            {readyPool.bounceRate === undefined
              ? unmeasuredLabel(readyPool.bounceEverRecorded)
              : `${(readyPool.bounceRate * 100).toFixed(1)}%`}
          </dd>
        </div>
        <div>
          <dt>Throughput / demand</dt>
          <dd>
            {curation.everRecorded ? readyPool.forwardCurationThroughput : unmeasuredLabel(false)} /{" "}
            {readyPool.implementationDemand}
          </dd>
        </div>
        <div>
          <dt>Curation actions</dt>
          <dd>
            {curation.everRecorded
              ? `${curation.ready} ready · ${curation.needsHuman} needs human · ${curation.closed} closed`
              : unmeasuredLabel(false)}
          </dd>
        </div>
      </dl>
    </section>
  );
}

// unmeasuredLabel distinguishes a metric whose writer has never once
// produced data (a dead write path, #2278) from one that simply has no
// samples in the currently selected window — both otherwise look identical
// (an absent/zero value) to an operator staring at the panel.
function unmeasuredLabel(everRecorded: boolean): string {
  return everRecorded ? "No data in window" : "Never recorded";
}

function formatSeconds(value: number | undefined, everRecorded: boolean): string {
  return value === undefined ? unmeasuredLabel(everRecorded) : formatDuration(value * 1_000);
}

function FailureReasonBreakdown({
  retry,
  state,
}: {
  retry: () => void;
  state: QueryState<InsightErrorSignaturesSnapshot>;
}) {
  const snapshot = state.status === "ready" || state.status === "stale" ? state.data : undefined;
  return (
    <>
      {state.status === "loading" ? (
        <SectionQueryStatus loading message="Loading failure reasons…" />
      ) : state.status === "error" ? (
        <SectionQueryStatus error message="Failure reasons could not be loaded." retry={retry} />
      ) : (
        <>
          {state.status === "stale" && state.error && (
            <SectionQueryStatus
              error
              message="Failure reasons could not be refreshed. Showing the last successful breakdown."
              retry={retry}
            />
          )}
          {snapshot && snapshot.result.items.length > 0 ? (
            <FailureReasonRows snapshot={snapshot} />
          ) : (
            <p className="inline-empty">No coded failures in this scope and time window.</p>
          )}
        </>
      )}
    </>
  );
}

function FailureReasonRows({ snapshot }: { snapshot: InsightErrorSignaturesSnapshot }) {
  const [showAll, setShowAll] = useState(false);
  const items = snapshot.result.items;
  const visibleItems = showAll ? items : items.slice(0, INITIAL_DETAIL_ROWS);
  return (
    <>
      <div className="data-table-shell error-signatures">
        <div aria-hidden="true" className="data-table-header error-signature-header">
          <span>Code</span>
          <span>Coarse class</span>
          <span>Count</span>
          <span>Last seen</span>
          <span>Matching example</span>
          <span />
        </div>
        {visibleItems.map((signature) => (
          <FailureReasonRow
            filters={snapshot.filters}
            key={`${signature.code}:${signature.errorClass}`}
            signature={signature}
          />
        ))}
        {items.length > INITIAL_DETAIL_ROWS && (
          <button
            className="data-table-disclosure"
            onClick={() => setShowAll((value) => !value)}
            type="button"
          >
            {showAll ? "Show fewer failure reasons" : `View all ${items.length} failure reasons`}
          </button>
        )}
      </div>
    </>
  );
}

function FailureReasonRow({
  filters,
  signature,
}: {
  filters: ErrorRouteFilters;
  signature: TelemetryErrorSignature;
}) {
  const code = signature.code || "uncoded";
  const errorClass = signature.errorClass || "unknown";
  const example = signature.exampleRunId
    ? [
        signature.exampleStage,
        signature.exampleAttempt ? `attempt ${signature.exampleAttempt}` : undefined,
      ]
        .filter(Boolean)
        .join(" · ")
    : "Instance event";
  const content = (
    <>
      <span className="error-signature-code">
        <strong>{code}</strong>
        <small>{signature.count === 1 ? "1 occurrence" : `${signature.count} occurrences`}</small>
      </span>
      <span className="error-class-label">{errorClass}</span>
      <strong className="error-signature-count">{signature.count}</strong>
      <Timestamp value={signature.lastSeen} />
      <span className="error-signature-example">{example}</span>
      <Icon name="chevron" size={15} />
    </>
  );

  return (
    <a
      aria-label={`View ${signature.count} matching ${signature.count === 1 ? "error" : "errors"} for ${code}`}
      className="error-signature-row"
      href={routeHash({
        page: "errors",
        filters: {
          gaggle: filters.gaggle,
          workflow: filters.workflow,
          stage: filters.stage,
          code: signature.code,
          errorClass: signature.errorClass,
          since: filters.since,
          until: filters.until,
        },
      })}
    >
      {content}
    </a>
  );
}

function OutcomeRow({ emphasis = false, metric }: { emphasis?: boolean; metric: OutcomeMetric }) {
  const terminal = metric.succeeded + metric.failed;
  const successWidth = terminal > 0 ? (metric.succeeded / terminal) * 100 : 0;
  const failureWidth = terminal > 0 ? (metric.failed / terminal) * 100 : 0;
  return (
    <div
      className={
        emphasis ? "insight-outcome-row insight-outcome-row-summary" : "insight-outcome-row"
      }
    >
      <span className="insight-scope-label">
        <strong>{metric.label}</strong>
      </span>
      <a
        aria-label={`View terminal ${metric.unit} behind ${metric.label} for success rate ${formatRate(metric.successRate)}`}
        className="insight-rate insight-metric-link"
        href={metricHref(metric, "terminal")}
      >
        <span aria-hidden="true" className="outcome-bar">
          <span className="outcome-bar-success" style={{ width: `${successWidth}%` }} />
          <span className="outcome-bar-failure" style={{ width: `${failureWidth}%` }} />
        </span>
        <strong>{formatRate(metric.successRate)}</strong>
      </a>
      <a
        aria-label={`View successful ${metric.unit} behind ${metric.label}: ${metric.succeeded}`}
        className="insight-number insight-number-success insight-metric-link"
        href={metricHref(metric, "success")}
      >
        {metric.succeeded}
      </a>
      <a
        aria-label={`View failed ${metric.unit} behind ${metric.label}: ${metric.failed}`}
        className="insight-number insight-number-failure insight-metric-link"
        href={metricHref(metric, "failure")}
      >
        {metric.failed}
      </a>
      <a
        aria-label={`View other ${metric.unit} behind ${metric.label}: ${metric.other}`}
        className="insight-number insight-metric-link"
        href={metricHref(metric, "other")}
      >
        {metric.other}
      </a>
      <a
        aria-label={`View all ${metric.unit} behind ${metric.label}: ${metric.total}`}
        className="insight-number insight-metric-link"
        href={metricHref(metric)}
      >
        {metric.total}
      </a>
    </div>
  );
}

export function UsageAnalytics({
  filters,
  mode,
  totalRuns,
  usage,
}: {
  filters: TelemetryStatsOptions;
  mode: "cost" | "insight";
  totalRuns?: number;
  usage: TelemetryUsageStats;
}) {
  const cost = mode === "cost";
  const label = usageMetricLabel(usage);
  const tokenHref = routeHash({
    page: "runs",
    filters: insightRunFilters(
      filters,
      usage.gaggle,
      usage.workflow,
      usage.stage,
      undefined,
      "token-measured",
    ),
  });
  const wasteHref = routeHash({
    page: "runs",
    filters: insightRunFilters(
      filters,
      usage.gaggle,
      usage.workflow,
      usage.stage,
      undefined,
      "retry-waste",
    ),
  });
  const formatUsage = cost ? formatMeasuredAIC : formatMeasuredTokens;
  const metric = (name: string, value: string, href?: string, ariaLabel?: string) => (
    <div>
      <dt>{name}</dt>
      <dd>
        {href ? (
          <a aria-label={ariaLabel} className="usage-summary-link" href={href}>
            {value}
          </a>
        ) : (
          value
        )}
      </dd>
    </div>
  );
  return (
    <div
      aria-label={`${cost ? "Cost" : "Token"} measurements: ${cost ? usage.costSamples : usage.tokenSamples} of ${usage.totalAttempts} stage executions, including retries`}
      className={`cost-usage-summary${cost ? "" : " usage-summary-tokens"}`}
      role="group"
    >
      <div className="cost-usage-panels">
        <dl
          className={`cost-usage-spend${totalRuns !== undefined ? " cost-usage-spend-with-runs" : ""}`}
        >
          {cost && metric("Total cost", formatMeasuredAIC(usage.costAIC))}
          {totalRuns !== undefined && metric("Total runs", totalRuns.toLocaleString())}
          {metric(
            "P50 / stage attempt",
            formatUsage(cost ? usage.p50CostAIC : usage.p50Tokens),
            cost ? undefined : tokenHref,
            `View P50 token usage runs behind ${label}`,
          )}
          {metric(
            "P95 / stage attempt",
            formatUsage(cost ? usage.p95CostAIC : usage.p95Tokens),
            cost ? undefined : tokenHref,
            `View P95 token usage runs behind ${label}`,
          )}
        </dl>
        <div className="cost-usage-waste">
          <dl>
            {metric(
              "Superseded attempts",
              usage.retryWasteAttempts.toLocaleString(),
              cost ? undefined : wasteHref,
              `View superseded stage attempts behind ${label}`,
            )}
            {metric(
              cost ? "Retry waste cost" : "Retry waste tokens",
              usage.retryWasteAttempts === 0
                ? "No retry waste"
                : formatUsage(cost ? usage.retryWasteCostAIC : usage.retryWasteTokens),
              cost ? undefined : wasteHref,
              `View retry-waste runs behind ${label}`,
            )}
          </dl>
        </div>
      </div>
    </div>
  );
}

export function CostTrend({
  costTrend,
  currentUsage,
  refreshing,
  retry,
  window,
}: {
  costTrend: QueryState<InsightCostTrendViewModel>;
  currentUsage: TelemetryUsageStats;
  refreshing: boolean;
  retry: () => void;
  window: InsightWindow;
}) {
  if (window === "all") {
    return (
      <p className="usage-trend-note">
        Trend and period comparison need a bounded time window — choose 24h, 7d, or 30d.
      </p>
    );
  }
  const data =
    costTrend.status === "ready" || costTrend.status === "stale" ? costTrend.data : undefined;
  const points = data?.points ?? [];
  const hasSamples = points.some((point) => (point.usage?.costSamples ?? 0) > 0);
  const error =
    costTrend.status === "error" || costTrend.status === "stale" ? costTrend.error : undefined;
  const unavailable = error && isMissingCostCapability(error);
  const loading = costTrend.status === "loading";

  return (
    <div className="usage-trend">
      <div className="usage-trend-status">
        <SectionQueryStatus
          error={Boolean(error)}
          loading={loading || refreshing}
          message={
            unavailable
              ? "Cost trends are not supported by this daemon. Upgrade Goobers to enable this section."
              : error
                ? data
                  ? "Cost trend refresh failed. Showing the last successful read."
                  : "Unable to load the cost trend."
                : loading
                  ? "Loading cost trend…"
                  : refreshing
                    ? "Refreshing cost trend…"
                    : !hasSamples
                      ? "No cost samples across buckets in this scope."
                      : undefined
          }
          retry={unavailable ? undefined : retry}
        />
      </div>
      <div className="usage-trend-heading">
        <h3>Cost over time</h3>
        <p className="usage-trend-note">
          Total cost per 24-hour day in the selected rolling window.
        </p>
      </div>
      {hasSamples ? (
        <CostTrendSparkline points={points} window={window} />
      ) : (
        <div aria-hidden="true" className="usage-trend-placeholder">
          <CostTrendSparkline points={[]} window={window} />
        </div>
      )}
      {data && (
        <CostTrendComparison current={currentUsage} previous={data.previousUsage} window={window} />
      )}
    </div>
  );
}

function CostTrendSparkline({
  points,
  window,
}: {
  points: { since: string; until: string; usage: TelemetryUsageStats | undefined }[];
  window: InsightWindow;
}) {
  const tooltipId = useId();
  const plotRef = useRef<HTMLDivElement>(null);
  const [tooltip, setTooltip] = useState<{
    since: string;
    day: string;
    name: string;
    value: number;
    x: number;
    y: number;
  }>();
  const width = 720;
  const height = 240;
  const margin = { top: 34, right: 80, bottom: 42, left: 80 };
  const plotWidth = width - margin.left - margin.right;
  const plotHeight = height - margin.top - margin.bottom;
  const slotWidth = plotWidth / Math.max(1, points.length);
  const barWidth = Math.min(44, slotWidth * 0.65);
  const chartPoints = points.map((point, index) => {
    const measured = (point.usage?.costSamples ?? 0) > 0;
    return {
      ...point,
      dailyCost: measured ? point.usage?.costAIC : undefined,
      p50: measured ? point.usage?.p50CostAIC : undefined,
      p95: measured ? point.usage?.p95CostAIC : undefined,
      x: margin.left + (index + 0.5) * slotWidth,
    };
  });
  const totalMax = Math.max(...chartPoints.map((point) => point.dailyCost ?? 0), 0.0001);
  const percentileMax = Math.max(
    ...chartPoints.flatMap((point) => [point.p50 ?? 0, point.p95 ?? 0]),
    0.0001,
  );
  const baseline = margin.top + plotHeight;
  const plottedPoints = chartPoints.map((point) => ({
    ...point,
    totalY:
      point.dailyCost === undefined
        ? undefined
        : baseline - (point.dailyCost / totalMax) * plotHeight,
    p50Y: point.p50 === undefined ? undefined : baseline - (point.p50 / percentileMax) * plotHeight,
    p95Y: point.p95 === undefined ? undefined : baseline - (point.p95 / percentileMax) * plotHeight,
  }));
  const series = [
    { key: "p50", name: "P50 per attempt" },
    { key: "p95", name: "P95 per attempt" },
  ] as const;
  const xTickIndexes = [...new Set([0, Math.floor((points.length - 1) / 2), points.length - 1])];
  const yTicks = [1, 0.5, 0];
  const showTooltip = (since: string, name: string, value: number, target: SVGGraphicsElement) => {
    if (!plotRef.current) return;
    const plot = plotRef.current.getBoundingClientRect();
    const mark = target.getBoundingClientRect();
    setTooltip({
      since,
      day: formatDateTime(since, "date"),
      name,
      value,
      x: ((mark.left + mark.width / 2 - plot.left) / Math.max(1, plot.width)) * 100,
      y: ((mark.top - plot.top) / Math.max(1, plot.height)) * 100,
    });
  };

  return (
    <div
      className="usage-trend-chart"
      onKeyDown={(event) => {
        if (event.key === "Escape") setTooltip(undefined);
      }}
    >
      <div
        className="usage-trend-plot-wrap"
        onMouseLeave={() => setTooltip(undefined)}
        ref={plotRef}
      >
        <svg
          aria-label={sparklineAriaLabel(points)}
          className="usage-trend-chart-plot"
          role="img"
          viewBox={`0 0 ${width} ${height}`}
        >
          <text className="usage-trend-axis-title" x={margin.left} y="16">
            Total / day (AIC)
          </text>
          <text className="usage-trend-axis-title" textAnchor="end" x={width - margin.right} y="16">
            P50 / P95 per attempt (AIC)
          </text>
          {yTicks.map((fraction) => {
            const y = baseline - fraction * plotHeight;
            return (
              <g className="usage-trend-gridline" key={fraction}>
                <line x1={margin.left} x2={width - margin.right} y1={y} y2={y} />
                <text x={margin.left - 10} y={y + 4}>
                  {formatMeasuredAIC(totalMax * fraction)}
                </text>
                <text
                  className="usage-trend-secondary-tick"
                  x={width - margin.right + 10}
                  y={y + 4}
                >
                  {formatMeasuredAIC(percentileMax * fraction)}
                </text>
              </g>
            );
          })}
          {plottedPoints.map((point) => {
            const { dailyCost, totalY } = point;
            if (dailyCost === undefined || totalY === undefined) return null;
            return (
              <rect
                aria-describedby={
                  tooltip?.since === point.since && tooltip.name === "Total per day"
                    ? tooltipId
                    : undefined
                }
                aria-label={`Total per day: ${formatMeasuredAIC(point.dailyCost)}`}
                className="usage-trend-bar"
                height={baseline - totalY}
                key={point.since}
                onBlur={() => setTooltip(undefined)}
                onFocus={(event) =>
                  showTooltip(point.since, "Total per day", dailyCost, event.currentTarget)
                }
                onMouseEnter={(event) =>
                  showTooltip(point.since, "Total per day", dailyCost, event.currentTarget)
                }
                onMouseLeave={() => setTooltip(undefined)}
                tabIndex={0}
                width={barWidth}
                x={point.x - barWidth / 2}
                y={totalY}
              />
            );
          })}
          {series.map(({ key, name }) => (
            <g key={key}>
              <path
                className={`usage-trend-line usage-trend-line-${key}`}
                d={costLinePath(
                  plottedPoints.map((point) => ({
                    x: point.x,
                    y: key === "p50" ? point.p50Y : point.p95Y,
                  })),
                )}
              />
              {plottedPoints.map((point) => {
                const y = key === "p50" ? point.p50Y : point.p95Y;
                const value = point[key];
                if (y === undefined || value === undefined) return null;
                return (
                  <g key={point.since}>
                    <circle
                      className={`usage-trend-point usage-trend-point-${key}`}
                      cx={point.x}
                      cy={y}
                      r="3"
                    />
                    <circle
                      aria-describedby={
                        tooltip?.since === point.since && tooltip.name === name
                          ? tooltipId
                          : undefined
                      }
                      aria-label={`${name}: ${formatMeasuredAIC(value)}`}
                      className="usage-trend-hit-target"
                      cx={point.x}
                      cy={y}
                      onBlur={() => setTooltip(undefined)}
                      onFocus={(event) =>
                        showTooltip(point.since, name, value, event.currentTarget)
                      }
                      onMouseEnter={(event) =>
                        showTooltip(point.since, name, value, event.currentTarget)
                      }
                      onMouseLeave={() => setTooltip(undefined)}
                      r="10"
                      tabIndex={0}
                    />
                  </g>
                );
              })}
            </g>
          ))}
          {[margin.left, width - margin.right].map((x) => (
            <line
              className="usage-trend-axis"
              key={x}
              x1={x}
              x2={x}
              y1={margin.top}
              y2={baseline}
            />
          ))}
          <line
            className="usage-trend-axis"
            x1={margin.left}
            x2={width - margin.right}
            y1={baseline}
            y2={baseline}
          />
          {xTickIndexes.map((index) => {
            const point = chartPoints[index];
            return point ? (
              <text
                className="usage-trend-x-label"
                key={point.since}
                textAnchor={index === 0 ? "start" : index === points.length - 1 ? "end" : "middle"}
                x={point.x}
                y={height - 15}
              >
                {formatBucketTick(point.since, window)}
              </text>
            ) : null;
          })}
        </svg>
        {tooltip && (
          <div
            className="usage-trend-tooltip"
            id={tooltipId}
            role="tooltip"
            style={{
              left: `${Math.min(80, Math.max(20, tooltip.x))}%`,
              top: `${tooltip.y}%`,
            }}
          >
            <span className="usage-trend-tooltip-day">{tooltip.day}</span>
            <span className="usage-trend-tooltip-value">
              <span>{tooltip.name}</span>
              <strong>{formatMeasuredAIC(tooltip.value)}</strong>
            </span>
          </div>
        )}
      </div>
      <div aria-label="Cost chart legend" className="usage-trend-legend" role="list">
        <span role="listitem">
          <i aria-hidden="true" className="usage-trend-key usage-trend-key-total" />
          Total per day (AIC, left axis)
        </span>
        <span role="listitem">
          <i aria-hidden="true" className="usage-trend-key usage-trend-key-p50" />
          P50 per attempt (AIC, right axis)
        </span>
        <span role="listitem">
          <i aria-hidden="true" className="usage-trend-key usage-trend-key-p95" />
          P95 per attempt (AIC, right axis)
        </span>
      </div>
    </div>
  );
}

function costLinePath(points: { x: number; y: number | undefined }[]): string {
  let previousMeasured = false;
  return points
    .map(({ x, y }) => {
      if (y === undefined) {
        previousMeasured = false;
        return "";
      }
      const command = previousMeasured ? "L" : "M";
      previousMeasured = true;
      return `${command} ${x} ${y}`;
    })
    .join(" ");
}

function sparklineAriaLabel(
  points: { since: string; until: string; usage: TelemetryUsageStats | undefined }[],
): string {
  const summary = points
    .map((point) => {
      const measured = (point.usage?.costSamples ?? 0) > 0;
      return `${formatBucketLabel(point.since, point.until)}: total ${formatMeasuredAIC(measured ? point.usage?.costAIC : undefined)}, P50 ${formatMeasuredAIC(measured ? point.usage?.p50CostAIC : undefined)}, P95 ${formatMeasuredAIC(measured ? point.usage?.p95CostAIC : undefined)}`;
    })
    .join("; ");
  return `Daily cost: total bars on the left axis, P50 and P95 per-attempt lines on the right axis. ${summary}`;
}

function formatBucketLabel(since: string, until: string): string {
  return `${formatTimestamp(since)} to ${formatTimestamp(until)}`;
}

function formatBucketTick(since: string, window: InsightWindow): string {
  return formatDateTime(since, window === "24h" ? "hour" : "date");
}

function CostTrendComparison({
  current,
  previous,
  window,
}: {
  current: TelemetryUsageStats;
  previous: TelemetryUsageStats | undefined;
  window: InsightWindow;
}) {
  const duration = windowDurationLabel(window);
  if (!previous || previous.costSamples === 0) {
    return null;
  }
  return (
    <dl className="usage-trend-comparison">
      <div>
        <dt>Cost vs. previous {duration}</dt>
        <dd>
          {formatMeasuredAIC(current.p50CostAIC)}
          <DeltaBadge current={current.p50CostAIC} previous={previous.p50CostAIC} />
        </dd>
      </div>
      <div>
        <dt>Tokens vs. previous {duration}</dt>
        <dd>
          {formatMeasuredTokens(current.p50Tokens)}
          <DeltaBadge current={current.p50Tokens} previous={previous.p50Tokens} />
        </dd>
      </div>
    </dl>
  );
}

function windowDurationLabel(window: InsightWindow): string {
  switch (window) {
    case "24h":
      return "24 hours";
    case "7d":
      return "7 days";
    case "30d":
      return "30 days";
    case "all":
      return "all time";
  }
}

function DeltaBadge({
  current,
  previous,
}: {
  current: number | undefined;
  previous: number | undefined;
}) {
  if (current === undefined || previous === undefined || previous === 0) {
    return <span className="usage-trend-delta usage-trend-delta-flat">Unmeasured</span>;
  }
  const change = (current - previous) / previous;
  const direction = change > 0 ? "up" : change < 0 ? "down" : "flat";
  const label = `${change > 0 ? "+" : ""}${(change * 100).toFixed(1)}%`;
  return <span className={`usage-trend-delta usage-trend-delta-${direction}`}>{label}</span>;
}

export function ExternalCostBreakdown({
  costs,
  refreshing,
  retry,
}: {
  costs: QueryState<InsightExternalCostSnapshot>;
  refreshing: boolean;
  retry: () => void;
}) {
  const [filter, setFilter] = useState("");
  const [kind, setKind] = useState<"all" | "pr" | "issue">("all");
  const [sortKey, setSortKey] = useState<ExternalCostSortKey>("aic");
  const [sortDirection, setSortDirection] = useState<ExternalCostSortDirection>("desc");
  const [openRuns, setOpenRuns] = useState<{
    label: string;
    runs: TelemetryCostRunAggregate[];
  }>();
  const runsDialog = useRef<HTMLElement>(null);
  useEffect(() => {
    if (!openRuns) return;
    const previousFocus =
      document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    runsDialog.current?.querySelector<HTMLButtonElement>(".dialog-close")?.focus();
    const handleKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpenRuns(undefined);
      if (event.key !== "Tab") return;
      const controls = [
        ...(runsDialog.current?.querySelectorAll<HTMLElement>("button, a[href], [tabindex='0']") ??
          []),
      ];
      const first = controls[0];
      const last = controls.at(-1);
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last?.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first?.focus();
      }
    };
    document.addEventListener("keydown", handleKey);
    return () => {
      document.body.style.overflow = previousOverflow;
      document.removeEventListener("keydown", handleKey);
      previousFocus?.focus();
    };
  }, [openRuns]);
  const rows = useMemo(
    () =>
      costs.status === "ready" || costs.status === "stale"
        ? deriveExternalCostRows(costs.data.result)
        : [],
    [costs],
  );
  const visibleRows = useMemo(
    () => sortExternalCostRows(filterExternalCostRows(rows, filter, kind), sortKey, sortDirection),
    [filter, kind, rows, sortDirection, sortKey],
  );
  const selectSort = (nextSortKey: ExternalCostSortKey) => {
    if (nextSortKey === sortKey) {
      setSortDirection((current) => (current === "asc" ? "desc" : "asc"));
      return;
    }
    setSortKey(nextSortKey);
    setSortDirection(nextSortKey === "work-item" || nextSortKey === "gaggle" ? "asc" : "desc");
  };
  const sortHeading = (label: string, key: ExternalCostSortKey) => (
    <button className="external-cost-sort-heading" onClick={() => selectSort(key)} type="button">
      {label}
      {sortKey === key && <span aria-hidden="true">{sortDirection === "asc" ? "↑" : "↓"}</span>}
    </button>
  );

  if (costs.status === "error") {
    const unavailable = isMissingCostCapability(costs.error);
    return (
      <section className="content-section cost-section-stable cost-section-attribution">
        <ExternalCostHeading />
        <SectionQueryStatus
          error
          message={
            unavailable
              ? "Attributed costs are not supported by this daemon. Upgrade Goobers to enable pull request and issue cost reporting."
              : "Unable to load pull request and issue costs."
          }
          retry={unavailable ? undefined : retry}
        />
      </section>
    );
  }
  if (costs.status === "loading") {
    return (
      <section className="content-section cost-section-stable cost-section-attribution">
        <ExternalCostHeading statusMessage="Loading attributed costs…" />
      </section>
    );
  }
  if (costs.status !== "ready" && costs.status !== "stale") {
    return null;
  }
  return (
    <section className="content-section cost-section-stable cost-section-attribution">
      <ExternalCostHeading
        statusMessage={refreshing ? "Refreshing attributed costs…" : undefined}
      />
      {costs.status === "stale" && costs.error && (
        <SectionQueryStatus
          error
          message={`Attributed cost refresh failed. Showing data loaded ${formatTimestamp(costs.data.loadedAt)}.`}
          retry={retry}
        />
      )}
      {costs.data.boundedAllTime && (
        <p className="usage-description">
          “All time” cost attribution is bounded to the latest 90 days.
        </p>
      )}
      {rows.length === 0 ? (
        <p className="inline-empty">No pull request or issue cost was attributed in this window.</p>
      ) : (
        <>
          <FilterOptions
            label="Cost work item filters"
            className="filter-bar external-cost-controls"
          >
            {(
              [
                ["all", "all"],
                ["pull requests", "pr"],
                ["issues", "issue"],
              ] as const
            ).map(([label, value]) => (
              <FilterOption
                selected={kind === value}
                key={value}
                onClick={() => setKind(value)}
                type="button"
              >
                {label}
              </FilterOption>
            ))}
            <FilterField
              kind="search"
              label="Filter"
              className="filter-search external-cost-filter-field"
            >
              <input
                onChange={(event) => setFilter(event.target.value)}
                placeholder="PR, issue, gaggle, model, or run"
                type="search"
                value={filter}
              />
            </FilterField>
          </FilterOptions>
          {visibleRows.length === 0 ? (
            <p className="inline-empty">No attributed costs match the current filters.</p>
          ) : (
            <>
              <TableShell
                ariaLabel="Attributed costs comparison"
                className="external-cost-table-wrap"
                tabIndex={0}
              >
                <div aria-label="Attributed costs" className="external-cost-table" role="table">
                  <div
                    className="data-table-header external-cost-grid external-cost-header"
                    role="row"
                  >
                    <span
                      aria-sort={
                        sortKey === "work-item"
                          ? sortDirection === "asc"
                            ? "ascending"
                            : "descending"
                          : "none"
                      }
                      role="columnheader"
                    >
                      {sortHeading("Work item", "work-item")}
                    </span>
                    <span
                      aria-sort={
                        sortKey === "gaggle"
                          ? sortDirection === "asc"
                            ? "ascending"
                            : "descending"
                          : "none"
                      }
                      role="columnheader"
                    >
                      {sortHeading("Gaggle", "gaggle")}
                    </span>
                    <span
                      aria-sort={
                        sortKey === "aic"
                          ? sortDirection === "asc"
                            ? "ascending"
                            : "descending"
                          : "none"
                      }
                      role="columnheader"
                    >
                      {sortHeading("AIC", "aic")}
                    </span>
                    <span
                      aria-sort={
                        sortKey === "runs"
                          ? sortDirection === "asc"
                            ? "ascending"
                            : "descending"
                          : "none"
                      }
                      role="columnheader"
                    >
                      {sortHeading("Runs / models", "runs")}
                    </span>
                  </div>
                  {visibleRows.map((row) => (
                    <div className="external-cost-grid external-cost-row" key={row.key} role="row">
                      <span className="work-item-identity external-cost-item" role="cell">
                        {row.repository ? (
                          <a
                            className="data-table-link data-table-primary"
                            href={routeHash({
                              page: "work-items",
                              provider: row.provider,
                              repository: row.repository,
                              kind: row.externalKind,
                              id: row.externalId,
                            })}
                          >
                            {row.label}
                          </a>
                        ) : (
                          <strong className="data-table-primary">{row.label}</strong>
                        )}
                        <small className="data-table-meta">
                          {row.provider} · {row.externalKind === "pr" ? "pull request" : "issue"}
                        </small>
                      </span>
                      <span className="external-cost-gaggle" role="cell">
                        {externalCostGaggles(row).join(", ") || "Unknown gaggle"}
                      </span>
                      <span className="external-cost-values" role="cell">
                        <strong className="data-table-number">{row.aic}</strong>
                        <small
                          className={row.lowerBound ? "cost-coverage-warning" : "data-table-meta"}
                        >
                          {row.coverage}
                        </small>
                      </span>
                      <span className="external-cost-runs" role="cell">
                        {row.models.length > 0 ? (
                          <ul
                            className="external-cost-models"
                            aria-label={`${row.label} model breakdown`}
                          >
                            {row.models.map((model) => (
                              <li key={model}>{model}</li>
                            ))}
                          </ul>
                        ) : (
                          <small className="data-table-meta">Model not recorded</small>
                        )}
                        {row.runs.length > 0 && (
                          <Action
                            variant="text"
                            size="compact"
                            aria-label={`View ${row.runs.length} run${row.runs.length === 1 ? "" : "s"} for ${row.label}`}
                            className="text-button run-link-action external-cost-runs-button"
                            onClick={() => setOpenRuns({ label: row.label, runs: row.runs })}
                            type="button"
                          >
                            {row.runs.length} run{row.runs.length === 1 ? "" : "s"}
                          </Action>
                        )}
                      </span>
                    </div>
                  ))}
                </div>
              </TableShell>
            </>
          )}
          {openRuns && (
            <div
              className="artifact-dialog-backdrop"
              onMouseDown={(event) => {
                if (event.target === event.currentTarget) setOpenRuns(undefined);
              }}
            >
              <section
                aria-labelledby="external-cost-runs-title"
                aria-modal="true"
                className="artifact-dialog external-cost-runs-dialog"
                ref={runsDialog}
                role="dialog"
              >
                <header>
                  <h2 id="external-cost-runs-title">{openRuns.label} runs</h2>
                  <button
                    aria-label="Close run list"
                    className="dialog-close"
                    onClick={() => setOpenRuns(undefined)}
                    type="button"
                  >
                    <Icon name="close" size={16} />
                  </button>
                </header>
                <DataTable
                  ariaLabel={`${openRuns.label} run breakdown`}
                  shellLabel={`${openRuns.label} run comparison`}
                  shellClassName="external-cost-run-table-wrap"
                  className="external-cost-run-table"
                  columns={["Run", "Started", "Attempts", "AIC", "Models"]}
                >
                  {openRuns.runs.map((run) => (
                    <tr key={run.runId}>
                      <td>
                        <a
                          aria-label={`Open run ${run.runId}`}
                          href={routeHash({ page: "run", id: run.runId })}
                          title={run.runId}
                        >
                          {run.runId}
                        </a>
                      </td>
                      <td>
                        <Timestamp value={run.startedAt} />
                      </td>
                      <td>
                        {run.measuredAttempts}/{run.usageAttempts} measured
                      </td>
                      <td>
                        {formatAICAmounts(run.nativeTotals, run.normalizedTotals, "Unmeasured")}
                      </td>
                      <td>
                        {run.models.map((model) => model.model).join(", ") || "Model not recorded"}
                      </td>
                    </tr>
                  ))}
                </DataTable>
              </section>
            </div>
          )}
        </>
      )}
    </section>
  );
}

function ExternalCostHeading({ statusMessage }: { statusMessage?: string }) {
  return (
    <SectionHeading
      title="Cost by pull request and issue"
      className=""
      actions={
        <>
          <div className="section-heading-meta">
            {statusMessage && <SectionQueryStatus loading message={statusMessage} />}
          </div>
        </>
      }
    />
  );
}

export function InstanceCostRollup({
  costRollup,
  refreshing,
  retry,
}: {
  costRollup: QueryState<InsightCostRollupSnapshot>;
  refreshing: boolean;
  retry: () => void;
}) {
  if (costRollup.status === "loading") {
    return (
      <section className="content-section cost-section-stable">
        <RollupHeading statusMessage="Loading instance spend…" />
      </section>
    );
  }
  if (costRollup.status === "error") {
    return (
      <section className="content-section cost-section-stable">
        <RollupHeading />
        <SectionQueryStatus error message="Unable to load instance spend." retry={retry} />
      </section>
    );
  }
  if (costRollup.status !== "ready" && costRollup.status !== "stale") {
    return null;
  }
  const data = costRollup.data;
  const rankedGaggles = data.byGaggle.filter((entry) => (entry.usage?.costSamples ?? 0) > 0);

  return (
    <section className="content-section cost-section-stable">
      <RollupHeading statusMessage={refreshing ? "Refreshing instance spend…" : undefined} />
      {costRollup.status === "stale" && costRollup.error && (
        <SectionQueryStatus
          error
          message="Instance spend refresh failed. Showing the last successful read."
          retry={retry}
        />
      )}
      {rankedGaggles.length === 0 ? (
        <p className="inline-empty">No gaggle has measured AIC in this window.</p>
      ) : (
        <div className="data-table-shell gaggle-spend-table">
          <div aria-hidden="true" className="data-table-header gaggle-spend-header">
            <span>Gaggle</span>
            <span>Total AIC</span>
            <span>P50 AIC</span>
            <span>P95 AIC</span>
            <span>Measured attempts</span>
          </div>
          {rankedGaggles.map((entry) => (
            <GaggleSpendRow entry={entry} filters={data.filters} key={entry.gaggle} />
          ))}
        </div>
      )}
    </section>
  );
}

function RollupHeading({ statusMessage }: { statusMessage?: string }) {
  return (
    <SectionHeading
      title="Cost by gaggle"
      className=""
      actions={
        <>
          <div className="section-heading-meta">
            {statusMessage && <SectionQueryStatus loading message={statusMessage} />}
          </div>
        </>
      }
    />
  );
}

function GaggleSpendRow({
  entry,
  filters,
}: {
  entry: InsightGaggleSpend;
  filters: TelemetryStatsOptions;
}) {
  const usage = entry.usage;
  const href = routeHash({
    page: "runs",
    filters: insightRunFilters(
      filters,
      entry.gaggle,
      undefined,
      undefined,
      undefined,
      "cost-measured",
    ),
  });
  return (
    <a
      aria-label={`View instance cost for gaggle ${entry.gaggle}: total ${formatMeasuredAIC(usage?.costAIC)}, ${formatMeasuredAttemptCount(usage?.costSamples ?? 0)}, P50 ${formatMeasuredAIC(usage?.p50CostAIC)}, P95 ${formatMeasuredAIC(usage?.p95CostAIC)}`}
      className="gaggle-spend-row"
      href={href}
    >
      <span className="distribution-name">
        <strong>{entry.gaggle}</strong>
      </span>
      <span data-label="Total AIC">{formatMeasuredAIC(usage?.costAIC)}</span>
      <span data-label="P50 AIC">{formatMeasuredAIC(usage?.p50CostAIC)}</span>
      <span data-label="P95 AIC">{formatMeasuredAIC(usage?.p95CostAIC)}</span>
      <span data-label="Measured attempts">
        {formatMeasuredAttemptCount(usage?.costSamples ?? 0)}
      </span>
    </a>
  );
}

function formatMeasuredAttemptCount(value: number): string {
  return `${value.toLocaleString()} measured ${value === 1 ? "attempt" : "attempts"}`;
}

function formatAICAmounts(
  native: readonly TelemetryCostAmount[],
  normalized: readonly TelemetryCostAmount[],
  empty: string,
): string {
  const amount = [...native, ...normalized].find((item) => item.unit === "aiCredits");
  if (!amount) {
    return empty;
  }
  return `${new Intl.NumberFormat("en-US", { maximumFractionDigits: 4 }).format(amount.value)} AIC`;
}

function StageDistributions({
  filters,
  stages,
}: {
  filters: TelemetryStatsOptions;
  stages: TelemetryStageStats[];
}) {
  const [showAll, setShowAll] = useState(false);
  const scaleMax = Math.max(...stages.map((stage) => stage.maxDurationMs ?? 0), 1);
  const visibleStages = showAll ? stages : stages.slice(0, INITIAL_DETAIL_ROWS);
  return (
    <>
      <div className="data-table-shell stage-distributions">
        <div className="data-table-header distribution-legend">
          <span>
            <i className="distribution-mark distribution-mark-p50" /> P50
          </span>
          <span>
            <i className="distribution-mark distribution-mark-p95" /> P95
          </span>
        </div>
        {visibleStages.map((stage) => (
          <StageDistributionRow
            filters={filters}
            key={`${stage.gaggle}:${stage.workflow}:${stage.stage}`}
            scaleMax={scaleMax}
            stage={stage}
          />
        ))}
        {stages.length > INITIAL_DETAIL_ROWS && (
          <button
            className="data-table-disclosure"
            onClick={() => setShowAll((value) => !value)}
            type="button"
          >
            {showAll ? "Show fewer stages" : `View all ${stages.length} stages`}
          </button>
        )}
      </div>
    </>
  );
}

function StageDistributionRow({
  filters,
  scaleMax,
  stage,
}: {
  filters: TelemetryStatsOptions;
  scaleMax: number;
  stage: TelemetryStageStats;
}) {
  return (
    <a
      aria-label={`View runs behind ${stage.gaggle} ${stage.workflow} ${stage.stage}: ${stage.durationSamples} samples, P50 ${formatMeasuredDuration(stage.p50DurationMs)}, P95 ${formatMeasuredDuration(stage.p95DurationMs)}, minimum ${formatMeasuredDuration(stage.minDurationMs)}, average ${formatMeasuredDuration(stage.avgDurationMs)}, maximum ${formatMeasuredDuration(stage.maxDurationMs)}${stage.stuckAbortedAttempts > 0 ? `, ${stage.stuckAbortedAttempts} stuck-aborted attempts excluded` : ""}`}
      className="stage-distribution-row"
      href={routeHash({
        page: "runs",
        filters: insightRunFilters(
          filters,
          stage.gaggle,
          stage.workflow,
          stage.stage,
          "finished",
          "measured",
        ),
      })}
    >
      <span className="distribution-name">
        <strong>
          {stage.gaggle} / {stage.workflow}
        </strong>
        <small>
          {stage.stage} · {stage.durationSamples} samples
          {stage.stuckAbortedAttempts > 0 && (
            <span
              className="distribution-excluded"
              title="Attempts whose run hung and was later aborted (max-duration expiry) are excluded from these duration stats so they don't skew the range."
            >
              {" "}
              · {stage.stuckAbortedAttempts} stuck-aborted excluded
            </span>
          )}
        </small>
      </span>
      <DistributionPlot scaleMax={scaleMax} stage={stage} />
      <span className="distribution-values">
        <span>
          <small>P50</small>
          <strong>{formatMeasuredDuration(stage.p50DurationMs)}</strong>
        </span>
        <span>
          <small>P95</small>
          <strong>{formatMeasuredDuration(stage.p95DurationMs)}</strong>
        </span>
        <span>
          <small>Min</small>
          <strong>{formatMeasuredDuration(stage.minDurationMs)}</strong>
        </span>
        <span>
          <small>Avg</small>
          <strong>{formatMeasuredDuration(stage.avgDurationMs)}</strong>
        </span>
        <span>
          <small>Max</small>
          <strong>{formatMeasuredDuration(stage.maxDurationMs)}</strong>
        </span>
      </span>
      <Icon name="chevron" size={15} />
    </a>
  );
}

function DistributionPlot({ scaleMax, stage }: { scaleMax: number; stage: TelemetryStageStats }) {
  const position = (value: number | undefined) =>
    `${Math.min(100, Math.max(0, ((value ?? 0) / scaleMax) * 100))}%`;
  const min = stage.minDurationMs ?? 0;
  const max = stage.maxDurationMs ?? min;
  return (
    <span
      aria-label={`Duration range ${formatMeasuredDuration(min)} to ${formatMeasuredDuration(max)}, average ${formatMeasuredDuration(stage.avgDurationMs)}, P50 ${formatMeasuredDuration(stage.p50DurationMs)}, P95 ${formatMeasuredDuration(stage.p95DurationMs)}`}
      className="distribution-plot"
      role="img"
    >
      <span className="distribution-track" />
      <span
        className="distribution-range"
        style={{ left: position(min), width: position(max - min) }}
      />
      <span
        className="distribution-dot distribution-dot-p50"
        style={{ left: position(stage.p50DurationMs) }}
      />
      <span
        className="distribution-dot distribution-dot-p95"
        style={{ left: position(stage.p95DurationMs) }}
      />
    </span>
  );
}

function usageMetricLabel(usage: TelemetryUsageStats): string {
  switch (usage.scope) {
    case "instance":
      return "Instance";
    case "gaggle":
      return usage.gaggle ?? "Gaggle";
    case "workflow":
      return [usage.gaggle, usage.workflow].filter(Boolean).join(" / ");
    case "stage":
      return [usage.gaggle, usage.workflow, usage.stage].filter(Boolean).join(" / ");
  }
}

function metricHref(
  metric: OutcomeMetric,
  outcome: RunRouteFilters["outcome"] = "finished",
): string {
  return routeHash({
    page: "runs",
    filters: {
      ...metric.filters,
      outcome,
      population: metric.unit === "attempts" ? "attempts" : undefined,
    },
  });
}

function formatRate(value: number | undefined): string {
  return value === undefined ? "Unmeasured" : `${(value * 100).toFixed(1)}%`;
}

function formatMeasuredDuration(value: number | undefined): string {
  return value === undefined ? "Unmeasured" : formatDuration(value);
}

function formatMeasuredTokens(value: number | undefined): string {
  return value === undefined ? "Unmeasured" : `${value.toLocaleString("en-US")} tokens`;
}

function formatMeasuredAIC(value: number | undefined): string {
  if (value === undefined) {
    return "Unmeasured";
  }
  return `${new Intl.NumberFormat("en-US", { maximumFractionDigits: 4 }).format(value)} AIC`;
}
import { DataTable, TableShell } from "../ui/DataTable";
import { formatDateTime } from "../dateTime";
