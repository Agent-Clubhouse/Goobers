import type { RunEvent, RunPhase } from "../api/types";
import { eventHeading, type RunFailure } from "../runDetailData";
import { CopyCommand } from "../ui/CopyCommand";
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
  const reasonParts = splitFailureReason(failure.message);
  return (
    <section aria-labelledby="failure-title" className="failure-panel" tabIndex={0}>
      <span className="escalation-icon">
        <Icon name="alert" />
      </span>
      <div className="escalation-content">
        <span className="escalation-label">
          {aborted
            ? "Attention · Aborted · why this run was aborted"
            : "Attention · Failure · why this run failed"}
        </span>
        <h2 id="failure-title">{aborted ? "Run aborted" : "Run failed"}</h2>
        <dl className="failure-facts">
          {failure.code && (
            <div>
              <dt>Error code</dt>
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
            <dt>Exit reason</dt>
            <dd>
              {reasonParts.length === 1 ? (
                <p>{reasonParts[0]}</p>
              ) : (
                <ol aria-label="Failure cause chain" className="failure-reason-chain">
                  {reasonParts.map((reason, index) => (
                    <li key={`${index}-${reason}`}>{reason}</li>
                  ))}
                </ol>
              )}
            </dd>
          </div>
        </dl>
        <details className="failure-raw-details">
          <summary>Show raw failure details</summary>
          <div className="failure-raw-toolbar">
            <span>Complete error context</span>
            <CopyCommand
              command={failure.message}
              compact
              failureLabel="Could not copy the raw failure details."
              idleLabel="Copy raw details"
              successLabel="Raw failure details copied."
            />
          </div>
          <pre>{failure.message}</pre>
        </details>
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

function splitFailureReason(message: string): string[] {
  const parts: string[] = [];
  let start = 0;
  let quoted = false;
  let escaped = false;

  for (let index = 0; index < message.length; index += 1) {
    const character = message[index];
    if (quoted && character === "\\" && !escaped) {
      escaped = true;
      continue;
    }
    if (character === '"' && !escaped) {
      quoted = !quoted;
    }
    escaped = false;

    if (!quoted && character === ":" && /\s/.test(message[index + 1] ?? "")) {
      const part = message.slice(start, index).trim();
      if (part) {
        parts.push(part);
      }
      start = index + 1;
      while (/\s/.test(message[start] ?? "")) {
        start += 1;
      }
      index = start - 1;
    }
  }

  const finalPart = message.slice(start).trim();
  if (finalPart) {
    parts.push(finalPart);
  }
  return parts.length > 0 ? parts : [message];
}
