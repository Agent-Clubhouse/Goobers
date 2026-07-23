import type { EscalationCause, RunEvent } from "../api/types";
import { eventHeading } from "../runDetailData";
import { Icon } from "../ui/Icon";

// EscalationPanel is the single authoritative "why" for an escalated run: the
// structured cause the daemon returns (gate/condition, selected branch, budget
// consumed, terminal reason) and a link to the durable causal event (DASH-21).
// It is the source of truth the header badge and the node-state overlay both
// defer to, so escalation reads one way rather than three.
export function EscalationPanel({
  escalation,
  causalEvent,
  onFocusCausalEvent,
}: {
  escalation: EscalationCause;
  causalEvent?: RunEvent;
  onFocusCausalEvent?: () => void;
}) {
  const selectorLabel = escalation.selector.kind === "gate" ? "Gate" : "Condition";
  const summary = escalation.terminalReason?.trim()
    ? escalation.terminalReason
    : `${selectorLabel} ${escalation.selector.name} escalated the run${
        escalation.selectedBranch ? ` via ${escalation.selectedBranch}` : ""
      }.`;

  return (
    <section aria-labelledby="escalation-title" className="escalation-panel" tabIndex={0}>
      <span className="escalation-icon">
        <Icon name="alert" />
      </span>
      <div className="escalation-content">
        <span className="escalation-label">Attention · Escalation · authoritative cause</span>
        <h2 id="escalation-title">{summary}</h2>
        <dl className="escalation-facts">
          <div>
            <dt>{selectorLabel}</dt>
            <dd>{escalation.selector.name}</dd>
          </div>
          {escalation.selectedBranch && (
            <div>
              <dt>Selected branch</dt>
              <dd className="mono">{escalation.selectedBranch}</dd>
            </div>
          )}
          <div>
            <dt>Budget consumed</dt>
            <dd>
              {escalation.repassCount} repass · {escalation.retryCount} retry
            </dd>
          </div>
          {escalation.terminalReason && (
            <div>
              <dt>Terminal reason</dt>
              <dd>{escalation.terminalReason}</dd>
            </div>
          )}
        </dl>
        {escalation.causalEventSeq !== undefined &&
          (causalEvent && onFocusCausalEvent ? (
            <button className="causal-event-link" onClick={onFocusCausalEvent} type="button">
              <span>Causal event</span>
              <strong>
                Seq {escalation.causalEventSeq} · {eventHeading(causalEvent)}
              </strong>
              <Icon name="arrow" size={14} />
            </button>
          ) : (
            <div className="causal-event-link causal-event-unavailable">
              <span>Causal event</span>
              <strong>Seq {escalation.causalEventSeq} · Unavailable</strong>
            </div>
          ))}
      </div>
    </section>
  );
}
