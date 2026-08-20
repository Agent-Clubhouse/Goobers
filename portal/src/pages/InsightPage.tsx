import { useCallback, useState } from "react";
import type {
  DaemonClient,
  TelemetryErrorSignature,
  TelemetryCurationStats,
  NodeCredit,
  TelemetryReadyPool,
  TelemetryStageStats,
  TelemetryStatsOptions,
  TelemetryUsageStats,
} from "../api/types";
import type { QueryState } from "../api/queryState";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import { ScopeStrip } from "../components/ScopeStrip";
import {
  type InsightCostRollupSnapshot,
  type InsightErrorSignaturesSnapshot,
  type InsightGaggleSpend,
  type InsightWindow,
  useInsightCostRollup,
  useInsightCostTrend,
  useInsightErrorSignatures,
  useInsightStats,
} from "../insightData";
import {
  deriveInsightCostTrendState,
  deriveInsightViewModel,
  hasInsightScopeIdentity,
  type InsightCostTrendViewModel,
  type InsightScope,
  type InsightViewModel,
  insightRunFilters,
  insightScopeApiParameters,
  insightScopeFromKey,
  insightScopeFromRoute,
  insightScopeKey,
  insightScopeOption,
  insightScopeOptions,
  insightScopeRouteFilters,
  type OutcomeMetric,
} from "../insightScope";
import { routeHash, type ErrorRouteFilters, type Navigate, type RunRouteFilters } from "../routing";
import type { ScopeFilters } from "../scope";
import { formatDuration, formatTimestamp } from "../runDetailData";
import { Icon } from "../ui/Icon";

const WINDOWS: readonly { label: string; value: InsightWindow }[] = [
  { label: "Last 24 hours", value: "24h" },
  { label: "Last 7 days", value: "7d" },
  { label: "Last 30 days", value: "30d" },
  { label: "All time", value: "all" },
];

export function InsightPage({
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
  const setScope = (nextScope: InsightScope) =>
    navigate({ page: "insight", filters: insightScopeRouteFilters(nextScope, window) });
  const setWindow = (nextWindow: InsightWindow) =>
    navigate({ page: "insight", filters: insightScopeRouteFilters(requestedScope, nextWindow) });
  const errorScope = insightScopeApiParameters(requestedScope);
  const query = useInsightStats(client, window, errorScope.gaggle, errorScope.workflow);
  const errorSignatures = useInsightErrorSignatures(
    client,
    window,
    errorScope.gaggle,
    errorScope.workflow,
    errorScope.stage,
  );
  const costTrend = useInsightCostTrend(client, window, errorScope.gaggle, errorScope.workflow);
  const costRollup = useInsightCostRollup(client, window);

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
        <h1>Insight</h1>
        <p>
          Success, backlog health, failure-reason, AI usage, and latency diagnostics for
          the selected operational scope. Run-backed metrics open their source history.
        </p>
      </header>

      <div className="insight-controls" aria-label="Insight filters">
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
            {WINDOWS.map((option) => (
              <option key={option.value} value={option.value}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
      </div>

      {hasInsightScopeIdentity(requestedScope) && (
        <ScopeStrip
          ariaLabel="Insight scope"
          clearHref={routeHash({
            page: "insight",
            filters: insightScopeRouteFilters({ kind: "instance" }, window),
          })}
          filters={errorScope}
        />
      )}

      {query.state.status === "stale" && query.state.error && (
        <div className="insight-stale-error" role="alert">
          Telemetry refresh failed. Showing the last successful snapshot for this window.
        </div>
      )}

      <InsightContent
        costRollup={costRollup.state}
        costRollupRetry={costRollup.retry}
        costTrend={costTrendView}
        costTrendRetry={costTrend.retry}
        errorSignatures={errorSignatures.state}
        errorSignaturesRetry={errorSignatures.retry}
        view={view}
      />
    </>
  );
}

function InsightContent({
  costRollup,
  costRollupRetry,
  costTrend,
  costTrendRetry,
  errorSignatures,
  errorSignaturesRetry,
  view,
}: {
  costRollup: QueryState<InsightCostRollupSnapshot>;
  costRollupRetry: () => void;
  costTrend: QueryState<InsightCostTrendViewModel>;
  costTrendRetry: () => void;
  errorSignatures: QueryState<InsightErrorSignaturesSnapshot>;
  errorSignaturesRetry: () => void;
  view: InsightViewModel;
}) {
  const { breakdown, creditAssignment, curationHealth, filters, stages, summary, usage, window } =
    view;
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
      <InstanceCostRollup costRollup={costRollup} retry={costRollupRetry} window={window} />

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
          <div className="section-heading">
            <div>
              <p className="section-kicker">Outcomes</p>
              <h2>Success and failure</h2>
            </div>
            <span className="section-count">Terminal outcomes exclude other states</span>
          </div>
          <div className="insight-outcomes">
            <div aria-hidden="true" className="insight-outcome-header">
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
        <CreditAssignment credits={creditAssignment} filters={filters} />
      )}

      {usage && (
        <section className="content-section">
          <div className="section-heading">
            <div>
              <p className="section-kicker">AI usage</p>
              <h2>Cost and tokens</h2>
            </div>
            <span className="section-count">Selected scope rollup</span>
          </div>
          <p className="usage-description">
            Attempt measurements are aggregated for the selected scope. Runners that do not
            report usage remain unmeasured.
          </p>
          <UsageAnalytics filters={filters} usage={usage} />
          <CostTrend
            costTrend={costTrend}
            currentUsage={usage}
            retry={costTrendRetry}
            window={window}
          />
        </section>
      )}

      <FailureReasonBreakdown retry={errorSignaturesRetry} state={errorSignatures} />

      {(hasOutcomes || stages.length > 0) && (
        <section className="content-section">
          <div className="section-heading">
            <div>
              <p className="section-kicker">Latency</p>
              <h2>Slowest stages</h2>
            </div>
            <span className="section-count">Ordered by P95 duration</span>
          </div>
          {stages.length === 0 ? (
            <p className="inline-empty">No stage duration samples in this scope.</p>
          ) : (
            <StageDistributions filters={filters} stages={stages} />
          )}
        </section>
      )}
        </>
      )}
    </>
  );
}

function CreditAssignment({
  credits,
  filters,
}: {
  credits: NodeCredit[];
  filters: TelemetryStatsOptions;
}) {
  return (
    <section className="content-section">
      <div className="section-heading">
        <div>
          <p className="section-kicker">Credit assignment</p>
          <h2>Highest-contributing nodes</h2>
        </div>
        <span className="section-count">Failure, escalation, and retry waste</span>
      </div>
      <div className="insight-outcomes">
        <div aria-hidden="true" className="credit-assignment-row credit-assignment-header">
          <span>Node</span>
          <span>Failure share</span>
          <span>Failures</span>
          <span>Escalations</span>
          <span>Retry waste</span>
        </div>
        {credits.map((credit) => (
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
                {credit.kind} · {credit.gaggle} / {credit.workflow} · {credit.routedRuns} routed runs
              </small>
            </span>
            <strong>{formatRate(credit.failureShare)}</strong>
            <strong>{credit.failureRuns}</strong>
            <strong>{credit.escalationRuns}</strong>
            <strong>{credit.retryWasteAttempts}</strong>
          </a>
        ))}
      </div>
    </section>
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
      <div className="section-heading">
        <div>
          <p className="section-kicker">Backlog</p>
          <h2>Ready-pool health</h2>
        </div>
        <span className="section-count">{curation.runs} curation runs</span>
      </div>
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
          <dt title="Time since claim for implementation items currently in progress">In flight now</dt>
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
    <section className="content-section">
      <div className="section-heading">
        <div>
          <p className="section-kicker">Failures</p>
          <h2>Failure reasons</h2>
        </div>
        <span className="section-count">Grouped by code + coarse class</span>
      </div>
      <p className="error-signature-description">
        Error class is a coarse telemetry label and may be unknown.
      </p>
      {state.status === "loading" ? (
        <p className="inline-empty">Loading failure reasons…</p>
      ) : state.status === "error" ? (
        <div className="inline-empty insight-inline-error" role="alert">
          <span>Failure reasons could not be loaded.</span>
          <button className="text-button" onClick={retry} type="button">
            Retry
          </button>
        </div>
      ) : (
        <>
          {state.status === "stale" && state.error && (
            <div className="insight-inline-error" role="alert">
              <span>
                Failure reasons could not be refreshed. Showing the last successful breakdown.
              </span>
              <button className="text-button" onClick={retry} type="button">
                Retry
              </button>
            </div>
          )}
          {snapshot && snapshot.result.items.length > 0 ? (
            <div className="error-signatures">
              <div aria-hidden="true" className="error-signature-header">
                <span>Code</span>
                <span>Coarse class</span>
                <span>Count</span>
                <span>Last seen</span>
                <span>Matching example</span>
                <span />
              </div>
              {snapshot.result.items.map((signature) => (
                <FailureReasonRow
                  filters={snapshot.filters}
                  key={`${signature.code}:${signature.errorClass}`}
                  signature={signature}
                />
              ))}
            </div>
          ) : (
            <p className="inline-empty">No coded failures in this scope and time window.</p>
          )}
        </>
      )}
    </section>
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
      <time dateTime={signature.lastSeen}>{formatTimestamp(signature.lastSeen)}</time>
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
      className={emphasis ? "insight-outcome-row insight-outcome-row-summary" : "insight-outcome-row"}
    >
      <span className="insight-scope-label">
        <strong>{metric.label}</strong>
        <small>{metric.unit}</small>
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

function UsageAnalytics({
  filters,
  usage,
}: {
  filters: TelemetryStatsOptions;
  usage: TelemetryUsageStats;
}) {
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
  const costHref = routeHash({
    page: "runs",
    filters: insightRunFilters(
      filters,
      usage.gaggle,
      usage.workflow,
      usage.stage,
      undefined,
      "cost-measured",
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
  return (
    <div className="usage-analytics">
      <div aria-hidden="true" className="usage-header">
        <span>Scope</span>
        <span>Tokens</span>
        <span>AI cost</span>
        <span>Retry waste</span>
      </div>
      <div className="usage-row">
        <span className="distribution-name">
          <strong>{usageMetricName(usage)}</strong>
          <small>
            {usageMetricContext(usage)} · {usage.totalAttempts}{" "}
            {usage.totalAttempts === 1 ? "attempt" : "attempts"}
          </small>
        </span>
        <UsagePercentiles
          ariaLabel={`View token usage runs behind ${label}: ${formatSamples(usage.tokenSamples)}, P50 ${formatMeasuredTokens(usage.p50Tokens)}, P95 ${formatMeasuredTokens(usage.p95Tokens)}`}
          formatter={formatMeasuredTokens}
          href={tokenHref}
          label="Tokens"
          p50={usage.p50Tokens}
          p95={usage.p95Tokens}
          samples={usage.tokenSamples}
        />
        <UsagePercentiles
          ariaLabel={`View AI cost runs behind ${label}: ${formatSamples(usage.costSamples)}, P50 ${formatMeasuredCost(usage.p50CostUSD)}, P95 ${formatMeasuredCost(usage.p95CostUSD)}`}
          formatter={formatMeasuredCost}
          href={costHref}
          label="AI cost"
          p50={usage.p50CostUSD}
          p95={usage.p95CostUSD}
          samples={usage.costSamples}
        />
        <RetryWasteMetric href={wasteHref} label={label} usage={usage} />
      </div>
    </div>
  );
}

function UsagePercentiles({
  ariaLabel,
  formatter,
  href,
  label,
  p50,
  p95,
  samples,
}: {
  ariaLabel: string;
  formatter: (value: number | undefined) => string;
  href: string;
  label: string;
  p50?: number;
  p95?: number;
  samples: number;
}) {
  return (
    <a aria-label={ariaLabel} className="usage-metric-link" href={href}>
      <span className="usage-metric-heading">
        <strong>{label}</strong>
        <small>{formatSamples(samples)}</small>
      </span>
      <span className="usage-percentiles">
        <span>
          <small>P50</small>
          <strong>{formatter(p50)}</strong>
        </span>
        <span>
          <small>P95</small>
          <strong>{formatter(p95)}</strong>
        </span>
      </span>
    </a>
  );
}

function RetryWasteMetric({
  href,
  label,
  usage,
}: {
  href: string;
  label: string;
  usage: TelemetryUsageStats;
}) {
  const description =
    usage.retryWasteAttempts === 0
      ? "no superseded attempts"
      : `${usage.retryWasteAttempts} superseded ${usage.retryWasteAttempts === 1 ? "attempt" : "attempts"}, ${formatMeasuredTokens(usage.retryWasteTokens)}, ${formatMeasuredCost(usage.retryWasteCostUSD)}`;
  return (
    <a
      aria-label={`View retry-waste runs behind ${label}: ${description}`}
      className="usage-metric-link usage-waste-link"
      href={href}
    >
      <span className="usage-metric-heading">
        <strong>Retry waste</strong>
        <small>
          {usage.retryWasteAttempts} superseded{" "}
          {usage.retryWasteAttempts === 1 ? "attempt" : "attempts"}
        </small>
      </span>
      {usage.retryWasteAttempts === 0 ? (
        <span className="usage-no-waste">
          <strong>No retry waste</strong>
        </span>
      ) : (
        <span className="usage-waste-values">
          <span>
            <small>Attempts</small>
            <strong>{usage.retryWasteAttempts}</strong>
          </span>
          <span>
            <small>Tokens</small>
            <strong>{formatMeasuredTokens(usage.retryWasteTokens)}</strong>
          </span>
          <span>
            <small>Cost</small>
            <strong>{formatMeasuredCost(usage.retryWasteCostUSD)}</strong>
          </span>
        </span>
      )}
    </a>
  );
}

function CostTrend({
  costTrend,
  currentUsage,
  retry,
  window,
}: {
  costTrend: QueryState<InsightCostTrendViewModel>;
  currentUsage: TelemetryUsageStats;
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
  if (costTrend.status === "loading") {
    return <p className="usage-trend-note">Loading cost trend…</p>;
  }
  if (costTrend.status === "error") {
    return (
      <div className="insight-inline-error">
        <span>Unable to load the cost trend.</span>
        <button onClick={retry} type="button">
          Retry
        </button>
      </div>
    );
  }
  if (costTrend.status !== "ready" && costTrend.status !== "stale") {
    return null;
  }
  const data = costTrend.data;
  const points = data.points;
  const hasSamples = points.some((point) => (point.usage?.costSamples ?? 0) > 0);

  return (
    <div className="usage-trend">
      {costTrend.status === "stale" && costTrend.error && (
        <div className="insight-inline-error">
          <span>Cost trend refresh failed. Showing the last successful read.</span>
          <button onClick={retry} type="button">
            Retry
          </button>
        </div>
      )}
      <div className="usage-trend-heading">
        <p className="section-kicker">Trend</p>
        <h3>Cost over time</h3>
      </div>
      {hasSamples ? (
        <CostTrendSparkline points={points} window={window} />
      ) : (
        <p className="usage-trend-note">No cost samples across buckets in this scope.</p>
      )}
      <CostTrendComparison current={currentUsage} previous={data.previousUsage} window={window} />
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
  const scaleMax = Math.max(...points.map((point) => point.usage?.p95CostUSD ?? 0), 0.0001);
  return (
    <div className="usage-trend-sparkline" role="img" aria-label={sparklineAriaLabel(points)}>
      {points.map((point) => {
        const p50 = point.usage?.p50CostUSD;
        const p95 = point.usage?.p95CostUSD;
        const p50Height = `${Math.min(100, ((p50 ?? 0) / scaleMax) * 100)}%`;
        const p95Height = `${Math.min(100, ((p95 ?? 0) / scaleMax) * 100)}%`;
        return (
          <span
            className="usage-trend-bar"
            key={point.since}
            title={`${formatBucketLabel(point.since, point.until)}: P50 ${formatMeasuredCost(p50)}, P95 ${formatMeasuredCost(p95)}`}
          >
            <span className="usage-trend-bar-track">
              <span className="usage-trend-bar-p95" style={{ height: p95Height }} />
              <span className="usage-trend-bar-p50" style={{ height: p50Height }} />
            </span>
            <small>{formatBucketTick(point.since, window)}</small>
          </span>
        );
      })}
    </div>
  );
}

function sparklineAriaLabel(
  points: { since: string; until: string; usage: TelemetryUsageStats | undefined }[],
): string {
  const summary = points
    .map(
      (point) =>
        `${formatBucketLabel(point.since, point.until)}: P50 ${formatMeasuredCost(point.usage?.p50CostUSD)}`,
    )
    .join("; ");
  return `AI cost trend by bucket. ${summary}`;
}

function formatBucketLabel(since: string, until: string): string {
  return `${formatTimestamp(since)} to ${formatTimestamp(until)}`;
}

function formatBucketTick(since: string, window: InsightWindow): string {
  const date = new Date(since);
  // Buckets for the 24h window are only hours apart, so a date-only tick
  // (e.g. "Jul 22") renders identically for every bar. Buckets for 7d/30d
  // are always at least a day apart, where the date is the meaningful axis.
  return window === "24h"
    ? date.toLocaleTimeString("en-US", { hour: "numeric" })
    : date.toLocaleDateString("en-US", { month: "short", day: "numeric" });
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
    return <p className="usage-trend-note">No prior {duration} to compare against in this scope.</p>;
  }
  return (
    <dl className="usage-trend-comparison">
      <div>
        <dt>AI cost vs. previous {duration}</dt>
        <dd>
          {formatMeasuredCost(current.p50CostUSD)}
          <DeltaBadge current={current.p50CostUSD} previous={previous.p50CostUSD} />
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

const BUDGET_THRESHOLD_STORAGE_KEY = "goobers-insight-budget-threshold-usd";

/**
 * The daemon has no budget-config endpoint (#2533 is portal-only), so the
 * soft threshold an operator sets is a local browser preference, not shared
 * instance state. Reads/writes are wrapped in try/catch (matching how
 * unavailable storage is handled elsewhere, e.g. private-browsing quota
 * errors) so a storage failure degrades to "no threshold set" instead of
 * crashing the page.
 */
function readStoredThreshold(): number | undefined {
  try {
    const stored = window.localStorage.getItem(BUDGET_THRESHOLD_STORAGE_KEY);
    const parsed = stored ? Number(stored) : NaN;
    return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
  } catch {
    return undefined;
  }
}

function writeStoredThreshold(value: number | undefined): void {
  try {
    if (value === undefined) {
      window.localStorage.removeItem(BUDGET_THRESHOLD_STORAGE_KEY);
    } else {
      window.localStorage.setItem(BUDGET_THRESHOLD_STORAGE_KEY, String(value));
    }
  } catch {
    // Storage unavailable (private browsing, quota) — the in-memory state
    // set alongside this call still drives the UI for the rest of the
    // session; it just won't survive a reload.
  }
}

function useBudgetThreshold(): [number | undefined, (value: number | undefined) => void] {
  const [threshold, setThresholdState] = useState<number | undefined>(readStoredThreshold);
  const setThreshold = useCallback((value: number | undefined) => {
    setThresholdState(value);
    writeStoredThreshold(value);
  }, []);
  return [threshold, setThreshold];
}

function InstanceCostRollup({
  costRollup,
  retry,
  window,
}: {
  costRollup: QueryState<InsightCostRollupSnapshot>;
  retry: () => void;
  window: InsightWindow;
}) {
  const [threshold, setThreshold] = useBudgetThreshold();

  if (costRollup.status === "loading") {
    return (
      <section className="content-section">
        <RollupHeading window={window} />
        <p className="inline-empty">Loading instance spend…</p>
      </section>
    );
  }
  if (costRollup.status === "error") {
    return (
      <section className="content-section">
        <RollupHeading window={window} />
        <div className="insight-inline-error">
          <span>Unable to load instance spend.</span>
          <button onClick={retry} type="button">
            Retry
          </button>
        </div>
      </section>
    );
  }
  if (costRollup.status !== "ready" && costRollup.status !== "stale") {
    return null;
  }
  const data = costRollup.data;
  const rankedGaggles = data.byGaggle.filter((entry) => (entry.usage?.costSamples ?? 0) > 0);
  const total = data.totalCostSamples === 0 ? undefined : data.totalCostUSD;

  return (
    <section className="content-section">
      <RollupHeading window={window} />
      {costRollup.status === "stale" && costRollup.error && (
        <div className="insight-inline-error">
          <span>Instance spend refresh failed. Showing the last successful read.</span>
          <button onClick={retry} type="button">
            Retry
          </button>
        </div>
      )}
      <div className="instance-spend-summary">
        <div className="instance-spend-total">
          <small>Total AI cost · all gaggles</small>
          <strong>{data.totalCostSamples === 0 ? "Unmeasured" : formatMeasuredCost(total)}</strong>
        </div>
        <BudgetThreshold onChange={setThreshold} threshold={threshold} total={total} />
      </div>
      {rankedGaggles.length === 0 ? (
        <p className="inline-empty">No gaggle has a measured AI cost in this window.</p>
      ) : (
        <div className="gaggle-spend-table">
          <div aria-hidden="true" className="gaggle-spend-header">
            <span>Gaggle</span>
            <span>P50 cost</span>
            <span>P95 cost</span>
            <span>Samples</span>
          </div>
          {rankedGaggles.map((entry) => (
            <GaggleSpendRow entry={entry} filters={data.filters} key={entry.gaggle} />
          ))}
        </div>
      )}
    </section>
  );
}

function RollupHeading({ window }: { window: InsightWindow }) {
  return (
    <div className="section-heading">
      <div>
        <p className="section-kicker">AI usage</p>
        <h2>Instance spend</h2>
      </div>
      <span className="section-count">All gaggles · {windowDurationLabel(window)}</span>
    </div>
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
      aria-label={`View instance spend for gaggle ${entry.gaggle}: ${formatSamples(usage?.costSamples ?? 0)}, P50 ${formatMeasuredCost(usage?.p50CostUSD)}, P95 ${formatMeasuredCost(usage?.p95CostUSD)}`}
      className="gaggle-spend-row"
      href={href}
    >
      <span className="distribution-name">
        <strong>{entry.gaggle}</strong>
      </span>
      <span>{formatMeasuredCost(usage?.p50CostUSD)}</span>
      <span>{formatMeasuredCost(usage?.p95CostUSD)}</span>
      <span>{formatSamples(usage?.costSamples ?? 0)}</span>
    </a>
  );
}

function BudgetThreshold({
  onChange,
  threshold,
  total,
}: {
  onChange: (value: number | undefined) => void;
  threshold: number | undefined;
  total: number | undefined;
}) {
  const [draft, setDraft] = useState(() => (threshold === undefined ? "" : String(threshold)));

  const commit = () => {
    const parsed = Number(draft);
    onChange(draft.trim() !== "" && Number.isFinite(parsed) && parsed > 0 ? parsed : undefined);
  };

  const status = budgetStatus(total, threshold);

  return (
    <div className="budget-threshold">
      <label>
        <small>Soft budget (USD)</small>
        <input
          inputMode="decimal"
          onBlur={commit}
          onChange={(event) => setDraft(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              commit();
              event.currentTarget.blur();
            }
          }}
          placeholder="Not set"
          type="number"
          value={draft}
        />
      </label>
      {status && (
        <span className={`budget-status budget-status-${status.kind}`} role="status">
          {status.label}
        </span>
      )}
    </div>
  );
}

function budgetStatus(
  total: number | undefined,
  threshold: number | undefined,
): { kind: "under" | "over"; label: string } | undefined {
  if (threshold === undefined || total === undefined) {
    return undefined;
  }
  const ratio = total / threshold;
  return ratio >= 1
    ? {
        kind: "over",
        label: `${(ratio * 100).toFixed(0)}% of budget — over by ${formatMeasuredCost(total - threshold)}`,
      }
    : { kind: "under", label: `${(ratio * 100).toFixed(0)}% of budget` };
}

function StageDistributions({
  filters,
  stages,
}: {
  filters: TelemetryStatsOptions;
  stages: TelemetryStageStats[];
}) {
  const scaleMax = Math.max(...stages.map((stage) => stage.maxDurationMs ?? 0), 1);
  return (
    <div className="stage-distributions">
      <div className="distribution-legend">
        <span>
          <i className="distribution-mark distribution-mark-p50" /> P50
        </span>
        <span>
          <i className="distribution-mark distribution-mark-p95" /> P95
        </span>
        <span className="distribution-scale">
          Scale 0 to {formatDuration(scaleMax)}
        </span>
      </div>
      {stages.map((stage) => (
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
          key={`${stage.gaggle}:${stage.workflow}:${stage.stage}`}
        >
          <span className="distribution-name">
            <strong>{stage.stage}</strong>
            <small>
              {stage.gaggle} / {stage.workflow} · {stage.durationSamples} samples
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
      ))}
    </div>
  );
}

function DistributionPlot({
  scaleMax,
  stage,
}: {
  scaleMax: number;
  stage: TelemetryStageStats;
}) {
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
      <span className="distribution-dot distribution-dot-p50" style={{ left: position(stage.p50DurationMs) }} />
      <span className="distribution-dot distribution-dot-p95" style={{ left: position(stage.p95DurationMs) }} />
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

function usageMetricName(usage: TelemetryUsageStats): string {
  switch (usage.scope) {
    case "instance":
      return "Instance";
    case "gaggle":
      return usage.gaggle ?? "Gaggle";
    case "workflow":
      return usage.workflow ?? "Workflow";
    case "stage":
      return usage.stage ?? "Stage";
  }
}

function usageMetricContext(usage: TelemetryUsageStats): string {
  switch (usage.scope) {
    case "instance":
      return "All gaggles";
    case "gaggle":
      return "Gaggle";
    case "workflow":
      return usage.gaggle ?? "Workflow";
    case "stage":
      return [usage.gaggle, usage.workflow].filter(Boolean).join(" / ");
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

function formatMeasuredCost(value: number | undefined): string {
  if (value === undefined) {
    return "Unmeasured";
  }
  return new Intl.NumberFormat("en-US", {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: 2,
    maximumFractionDigits: 4,
  }).format(value);
}

function formatSamples(samples: number): string {
  return samples === 0 ? "Unmeasured" : `${samples} ${samples === 1 ? "sample" : "samples"}`;
}
