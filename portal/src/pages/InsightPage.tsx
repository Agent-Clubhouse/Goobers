import { useState } from "react";
import type {
  DaemonClient,
  TelemetryErrorSignature,
  TelemetryGaggleStats,
  TelemetryRunStats,
  TelemetryStageStats,
  TelemetryStatsOptions,
  TelemetryStatsResult,
} from "../api/types";
import type { QueryState } from "../api/queryState";
import { DaemonErrorState, DaemonLoadingState } from "../components/DaemonQueryState";
import {
  type InsightErrorSignaturesSnapshot,
  type InsightSnapshot,
  type InsightWindow,
  useInsightErrorSignatures,
  useInsightStats,
} from "../insightData";
import { routeHash, type RunRouteFilters } from "../routing";
import { formatDuration, formatTimestamp } from "../runDetailData";
import { Icon } from "../ui/Icon";

type InsightScope =
  | { kind: "instance" }
  | { kind: "gaggle"; gaggle: string }
  | { kind: "workflow"; gaggle: string; workflow: string }
  | { kind: "stage"; gaggle: string; workflow: string; stage: string };

interface OutcomeMetric {
  failed: number;
  filters: RunRouteFilters;
  label: string;
  other: number;
  successRate?: number;
  succeeded: number;
  total: number;
  unit: "attempts" | "runs";
}

const WINDOWS: readonly { label: string; value: InsightWindow }[] = [
  { label: "Last 24 hours", value: "24h" },
  { label: "Last 7 days", value: "7d" },
  { label: "Last 30 days", value: "30d" },
  { label: "All time", value: "all" },
];

export function InsightPage({
  client,
  standalone,
}: {
  client: DaemonClient;
  standalone: boolean;
}) {
  const [window, setWindow] = useState<InsightWindow>("7d");
  const [scopeKey, setScopeKey] = useState(scopeToKey({ kind: "instance" }));
  const requestedScope = scopeFromKey(scopeKey);
  const errorScope = errorSignatureScope(requestedScope);
  const query = useInsightStats(client, window);
  const errorSignatures = useInsightErrorSignatures(
    client,
    window,
    errorScope.gaggle,
    errorScope.workflow,
    errorScope.stage,
  );

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
  const availableScopes = scopeOptions(snapshot.stats);
  const scopes = availableScopes.some((option) => option.key === scopeToKey(requestedScope))
    ? availableScopes
    : [...availableScopes, scopeOption(requestedScope)];

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Telemetry</p>
        <h1>Insight</h1>
        <p>
          Success, failure-reason, and latency diagnostics for the selected operational scope.
          Every metric opens the runs behind it.
        </p>
      </header>

      <div className="insight-controls" aria-label="Insight filters">
        <label>
          <span>Scope</span>
          <select
            aria-label="Scope"
            onChange={(event) => setScopeKey(event.target.value)}
            value={scopeToKey(requestedScope)}
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

      {query.state.status === "stale" && query.state.error && (
        <div className="insight-stale-error" role="alert">
          Telemetry refresh failed. Showing the last successful snapshot for this window.
        </div>
      )}

      <InsightContent
        errorSignatures={errorSignatures.state}
        errorSignaturesRetry={errorSignatures.retry}
        scope={requestedScope}
        snapshot={snapshot}
      />
    </>
  );
}

function InsightContent({
  errorSignatures,
  errorSignaturesRetry,
  scope,
  snapshot,
}: {
  errorSignatures: QueryState<InsightErrorSignaturesSnapshot>;
  errorSignaturesRetry: () => void;
  scope: InsightScope;
  snapshot: InsightSnapshot;
}) {
  const summary = scopeMetric(scope, snapshot.stats, snapshot.filters);
  const breakdown = outcomeBreakdown(scope, snapshot.stats, snapshot.filters);
  const stages = stagesInScope(scope, snapshot.stats.stages)
    .filter((stage) => stage.durationSamples > 0)
    .sort(
      (left, right) =>
        (right.p95DurationMs ?? -1) - (left.p95DurationMs ?? -1) ||
        left.stage.localeCompare(right.stage),
    );
  const hasOutcomes = Boolean(summary) || breakdown.length > 0;
  const hasFailureReasons =
    (errorSignatures.status === "ready" || errorSignatures.status === "stale") &&
    errorSignatures.data.result.items.length > 0;
  const failureReasonsFailed =
    errorSignatures.status === "error" ||
    (errorSignatures.status === "stale" && Boolean(errorSignatures.error));

  if (
    !hasOutcomes &&
    stages.length === 0 &&
    !hasFailureReasons &&
    !failureReasonsFailed &&
    errorSignatures.status !== "loading"
  ) {
    return (
      <section className="empty-state insight-empty">
        <span className="insight-empty-icon">
          <Icon name="insight" size={24} />
        </span>
        <div>
          <h2>No telemetry in this window</h2>
          <p>Choose a wider time window or another scope to inspect recorded runs.</p>
        </div>
      </section>
    );
  }

  return (
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
            <StageDistributions filters={snapshot.filters} stages={stages} />
          )}
        </section>
      )}
    </>
  );
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

function FailureReasonRow({ signature }: { signature: TelemetryErrorSignature }) {
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
      {signature.exampleRunId ? <Icon name="chevron" size={15} /> : <span />}
    </>
  );

  return signature.exampleRunId ? (
    <a
      aria-label={`Open example run ${signature.exampleRunId} for error ${code}`}
      className="error-signature-row"
      href={routeHash({ page: "run", id: signature.exampleRunId })}
    >
      {content}
    </a>
  ) : (
    <div className="error-signature-row">{content}</div>
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
          aria-label={`View runs behind ${stage.gaggle} ${stage.workflow} ${stage.stage}: ${stage.durationSamples} samples, P50 ${formatMeasuredDuration(stage.p50DurationMs)}, P95 ${formatMeasuredDuration(stage.p95DurationMs)}, minimum ${formatMeasuredDuration(stage.minDurationMs)}, average ${formatMeasuredDuration(stage.avgDurationMs)}, maximum ${formatMeasuredDuration(stage.maxDurationMs)}`}
          className="stage-distribution-row"
          href={routeHash({
            page: "runs",
            filters: drillFilters(
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

function scopeOptions(stats: TelemetryStatsResult): { key: string; label: string }[] {
  return [
    scopeOption({ kind: "instance" }),
    ...stats.gaggles.map((item) => scopeOption({ kind: "gaggle", gaggle: item.gaggle })),
    ...stats.runs.map((item) =>
      scopeOption({ kind: "workflow", gaggle: item.gaggle, workflow: item.workflow }),
    ),
    ...stats.stages.map((item) =>
      scopeOption({
        kind: "stage",
        gaggle: item.gaggle,
        workflow: item.workflow,
        stage: item.stage,
      }),
    ),
  ];
}

function scopeOption(scope: InsightScope): { key: string; label: string } {
  switch (scope.kind) {
    case "instance":
      return { key: scopeToKey(scope), label: "Instance" };
    case "gaggle":
      return { key: scopeToKey(scope), label: `Gaggle · ${scope.gaggle}` };
    case "workflow":
      return {
        key: scopeToKey(scope),
        label: `Workflow · ${scope.gaggle} / ${scope.workflow}`,
      };
    case "stage":
      return {
        key: scopeToKey(scope),
        label: `Stage · ${scope.gaggle} / ${scope.workflow} / ${scope.stage}`,
      };
  }
}

function errorSignatureScope(scope: InsightScope): {
  gaggle?: string;
  workflow?: string;
  stage?: string;
} {
  switch (scope.kind) {
    case "instance":
      return {};
    case "gaggle":
      return { gaggle: scope.gaggle };
    case "workflow":
      return { gaggle: scope.gaggle, workflow: scope.workflow };
    case "stage":
      return { gaggle: scope.gaggle, workflow: scope.workflow, stage: scope.stage };
  }
}

function scopeMetric(
  scope: InsightScope,
  stats: TelemetryStatsResult,
  filters: TelemetryStatsOptions,
): OutcomeMetric | undefined {
  switch (scope.kind) {
    case "instance":
      return sumGaggles(stats.gaggles, filters);
    case "gaggle": {
      const item = stats.gaggles.find((candidate) => candidate.gaggle === scope.gaggle);
      return item && gaggleMetric(item, filters);
    }
    case "workflow": {
      const item = stats.runs.find(
        (candidate) =>
          candidate.gaggle === scope.gaggle && candidate.workflow === scope.workflow,
      );
      return item && runMetric(item, filters);
    }
    case "stage": {
      const item = stats.stages.find(
        (candidate) =>
          candidate.gaggle === scope.gaggle &&
          candidate.workflow === scope.workflow &&
          candidate.stage === scope.stage,
      );
      return item && stageMetric(item, filters);
    }
  }
}

function outcomeBreakdown(
  scope: InsightScope,
  stats: TelemetryStatsResult,
  filters: TelemetryStatsOptions,
): OutcomeMetric[] {
  switch (scope.kind) {
    case "instance":
      return stats.gaggles.map((item) => gaggleMetric(item, filters));
    case "gaggle":
      return stats.runs
        .filter((item) => item.gaggle === scope.gaggle)
        .map((item) => runMetric(item, filters));
    case "workflow":
      return stats.stages
        .filter(
          (item) => item.gaggle === scope.gaggle && item.workflow === scope.workflow,
        )
        .map((item) => stageMetric(item, filters));
    case "stage":
      return [];
  }
}

function stagesInScope(
  scope: InsightScope,
  stages: TelemetryStageStats[],
): TelemetryStageStats[] {
  switch (scope.kind) {
    case "instance":
      return [...stages];
    case "gaggle":
      return stages.filter((stage) => stage.gaggle === scope.gaggle);
    case "workflow":
      return stages.filter(
        (stage) => stage.gaggle === scope.gaggle && stage.workflow === scope.workflow,
      );
    case "stage":
      return stages.filter(
        (stage) =>
          stage.gaggle === scope.gaggle &&
          stage.workflow === scope.workflow &&
          stage.stage === scope.stage,
      );
  }
}

function sumGaggles(
  gaggles: TelemetryGaggleStats[],
  filters: TelemetryStatsOptions,
): OutcomeMetric | undefined {
  if (gaggles.length === 0) {
    return undefined;
  }
  const total = gaggles.reduce(
    (sum, item) => ({
      completed: sum.completed + item.completedRuns,
      failed: sum.failed + item.failedRuns,
      other: sum.other + item.otherRuns,
      runs: sum.runs + item.totalRuns,
    }),
    { completed: 0, failed: 0, other: 0, runs: 0 },
  );
  const terminal = total.completed + total.failed;
  return {
    failed: total.failed,
    filters: drillFilters(filters),
    label: "Instance",
    other: total.other,
    successRate: terminal > 0 ? total.completed / terminal : undefined,
    succeeded: total.completed,
    total: total.runs,
    unit: "runs",
  };
}

function gaggleMetric(
  item: TelemetryGaggleStats,
  filters: TelemetryStatsOptions,
): OutcomeMetric {
  return {
    failed: item.failedRuns,
    filters: drillFilters(filters, item.gaggle),
    label: item.gaggle,
    other: item.otherRuns,
    successRate: item.successRate,
    succeeded: item.completedRuns,
    total: item.totalRuns,
    unit: "runs",
  };
}

function runMetric(item: TelemetryRunStats, filters: TelemetryStatsOptions): OutcomeMetric {
  return {
    failed: item.failedRuns,
    filters: drillFilters(filters, item.gaggle, item.workflow),
    label: `${item.gaggle} / ${item.workflow}`,
    other: item.otherRuns,
    successRate: item.successRate,
    succeeded: item.completedRuns,
    total: item.totalRuns,
    unit: "runs",
  };
}

function stageMetric(item: TelemetryStageStats, filters: TelemetryStatsOptions): OutcomeMetric {
  return {
    failed: item.failedAttempts,
    filters: drillFilters(filters, item.gaggle, item.workflow, item.stage),
    label: `${item.gaggle} / ${item.workflow} / ${item.stage}`,
    other: item.totalAttempts - item.succeededAttempts - item.failedAttempts,
    successRate: item.successRate,
    succeeded: item.succeededAttempts,
    total: item.totalAttempts,
    unit: "attempts",
  };
}

function drillFilters(
  filters: TelemetryStatsOptions,
  gaggle?: string,
  workflow?: string,
  stage?: string,
  outcome?: RunRouteFilters["outcome"],
  population?: RunRouteFilters["population"],
): RunRouteFilters {
  return {
    gaggle,
    workflow,
    stage,
    outcome,
    population,
    since: filters.since,
    until: filters.until,
  };
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

function scopeToKey(scope: InsightScope): string {
  switch (scope.kind) {
    case "instance":
      return JSON.stringify(["instance"]);
    case "gaggle":
      return JSON.stringify(["gaggle", scope.gaggle]);
    case "workflow":
      return JSON.stringify(["workflow", scope.gaggle, scope.workflow]);
    case "stage":
      return JSON.stringify(["stage", scope.gaggle, scope.workflow, scope.stage]);
  }
}

function scopeFromKey(key: string): InsightScope {
  try {
    const parts = JSON.parse(key) as unknown;
    if (!Array.isArray(parts) || !parts.every((part) => typeof part === "string")) {
      return { kind: "instance" };
    }
    if (parts[0] === "gaggle" && parts[1]) {
      return { kind: "gaggle", gaggle: parts[1] };
    }
    if (parts[0] === "workflow" && parts[1] && parts[2]) {
      return { kind: "workflow", gaggle: parts[1], workflow: parts[2] };
    }
    if (parts[0] === "stage" && parts[1] && parts[2] && parts[3]) {
      return { kind: "stage", gaggle: parts[1], workflow: parts[2], stage: parts[3] };
    }
  } catch {
    return { kind: "instance" };
  }
  return { kind: "instance" };
}

function formatRate(value: number | undefined): string {
  return value === undefined ? "Unmeasured" : `${(value * 100).toFixed(1)}%`;
}

function formatMeasuredDuration(value: number | undefined): string {
  return value === undefined ? "Unmeasured" : formatDuration(value);
}
