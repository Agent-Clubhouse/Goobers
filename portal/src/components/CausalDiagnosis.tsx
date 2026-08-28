import type { RunDetail, RunEvent } from "../api/types";
import {
  deriveFailureBreadcrumb,
  deriveVisitLineage,
  deriveWaterfall,
} from "../causalDiagnosis";
import { formatDuration, formatTimestamp } from "../runDetailData";

export function CausalDiagnosis({ run, events }: { run: RunDetail; events: RunEvent[] }) {
  const lineage = deriveVisitLineage(events, run.id);
  const waterfall = deriveWaterfall(events, run.id);
  if (lineage.length === 0 && waterfall.length === 0) {
    return (
      <section aria-labelledby="causal-diagnosis-title" className="causal-diagnosis">
        <p className="section-kicker">Diagnosis</p>
        <h2 id="causal-diagnosis-title">Causal diagnosis</h2>
        <p className="diagnosis-muted">No stage transitions are available for this run.</p>
      </section>
    );
  }
  return (
    <section aria-labelledby="causal-diagnosis-title" className="causal-diagnosis">
      <div className="panel-heading-row">
        <div>
          <p className="section-kicker">Diagnosis</p>
          <h2 id="causal-diagnosis-title">Causal diagnosis</h2>
          <p className="diagnosis-muted">Derived from durable transitions and timestamps; missing data is left explicit.</p>
        </div>
      </div>
      {run.phase === "failed" && (
        <div className="diagnosis-breadcrumb" role="status">
          <strong>Failure path</strong>
          <span>
            {deriveFailureBreadcrumb(run, events)}
          </span>
        </div>
      )}
      <div className="diagnosis-grid">
        <div>
          <h3>Visit lineage</h3>
          <ol className="diagnosis-lineage">
            {lineage.map((visit) => (
              <li key={`${visit.branch}:${visit.stage}:${visit.visit}`}>
                <span className="mono">B{visit.branch} · Visit {visit.visit}</span>
                <strong>{visit.stage}</strong>
                <span>{visit.attempt !== undefined ? `Attempt ${visit.attempt}` : "Attempt unavailable"} · {visit.status ?? "in progress"}</span>
              </li>
            ))}
          </ol>
        </div>
        <div>
          <h3>Execution waterfall</h3>
          <ol className="diagnosis-waterfall">
            {waterfall.map((row) => (
              <li key={row.key}>
                <div className="waterfall-bar" style={{ width: `${Math.max(4, Math.min(100, (row.durationMillis ?? 0) / Math.max(run.durationMillis, 1) * 100))}%` }} />
                <span className="mono">{formatTimestamp(row.startedAt)}</span>
                <strong>{row.stage}</strong>
                <span>{row.durationMillis !== undefined ? formatDuration(row.durationMillis) : "Duration unavailable"} · {row.status ?? "running"}</span>
                {row.idleBeforeMillis > 0 && <small>Idle gap {formatDuration(row.idleBeforeMillis)}</small>}
              </li>
            ))}
          </ol>
        </div>
      </div>
    </section>
  );
}
