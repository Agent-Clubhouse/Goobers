import type { RunEvent, RunPhase } from "../api/types";
import { eventHeading, type RunFailure } from "../runDetailData";
import { Icon } from "../ui/Icon";

// FailurePanel is the unsuccessfully-terminated-run counterpart to
// EscalationPanel: the single authoritative "why this run ended", surfaced at
// the top of the run page so an operator reads the coded reason (and jumps to
// the failing event) in seconds rather than scrolling the ledger to
// reconstruct it. It is deliberately shaped like EscalationPanel — same danger
// banner, same causal-event affordance — so failure, abort, and escalation read
// one consistent way.
export function FailurePanel({
  failure,
  phase,
  causalEvent,
  onFocusCausalEvent,
  errorsHref,
}: {
  failure: RunFailure;
  phase?: RunPhase;
  causalEvent?: RunEvent;
  onFocusCausalEvent?: () => void;
  errorsHref?: string;
}) {
  const aborted = phase === "aborted";
  return (
    <section aria-labelledby="failure-title" className="failure-panel" tabIndex={0}>
      <span className="escalation-icon">
        <Icon name="alert" />
      </span>
      <div className="escalation-content">
        <h2 id="failure-title">{aborted ? "Run aborted" : "Run failed"}</h2>
        <dl className="failure-facts">
          {failure.code && (
            <div className="failure-fact-inline">
              <dt>Error Code:</dt>
              <dd className="mono">{failure.code}</dd>
            </div>
          )}
          {failure.stage && (
            <div>
              <dt>{aborted ? "Last stage" : "Failed stage"}</dt>
              <dd className="mono">{failure.stage}</dd>
            </div>
          )}
          {failure.attempt !== undefined && (
            <div>
              <dt>Attempt</dt>
              <dd>{failure.attempt}</dd>
            </div>
          )}
          <div className="failure-reason">
            <dt>Reason</dt>
            <dd>
              <p>{failure.message}</p>
            </dd>
          </div>
        </dl>
        {failure.causalEventSeq !== undefined &&
          (causalEvent && onFocusCausalEvent ? (
            <button className="causal-event-link" onClick={onFocusCausalEvent} type="button">
              <span>Failing event</span>
              <strong>
                Seq {failure.causalEventSeq} · {eventHeading(causalEvent)}
              </strong>
              <Icon name="arrow" size={14} />
            </button>
          ) : (
            <div className="causal-event-link causal-event-unavailable">
              <span>Failing event</span>
              <strong>Seq {failure.causalEventSeq} · Unavailable</strong>
            </div>
          ))}
        {errorsHref && (
          <a className="failure-errors-link" href={errorsHref}>
            <span>View matching errors</span>
            <Icon name="arrow" size={14} />
          </a>
        )}
      </div>
    </section>
  );
}
