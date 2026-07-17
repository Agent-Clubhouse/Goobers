import type { Run, RunEvent, Workflow } from "../prototypeData";
import { visibleAttempts } from "../runState";
import { Icon } from "../ui/Icon";

interface EscalationPanelProps {
  causalEvent?: RunEvent;
  evidenceEventSeq?: number;
  onFocusCausalEvent?: () => void;
  onSelectStage: (stageId: string) => void;
  run: Run;
  workflow: Workflow;
}

/**
 * Explains why an escalated run stopped: the structured cause (gate or
 * condition, selected branch, budget consumed, terminal reason), a link to
 * the durable event that caused it, and the point-in-time evidence (stage
 * attempts and artifacts) as of that event.
 */
export function EscalationPanel({
  run,
  workflow,
  evidenceEventSeq,
  causalEvent,
  onFocusCausalEvent,
  onSelectStage,
}: EscalationPanelProps) {
  const evidence =
    evidenceEventSeq === undefined
      ? []
      : workflow.stages
          .map((stage) => ({
            stage,
            attempts: visibleAttempts(run, stage.id, evidenceEventSeq),
          }))
          .filter(({ attempts }) => attempts.length > 0);
  const evidenceUnavailable = run.escalation !== undefined && evidenceEventSeq === undefined;
  const attemptCount = evidence.reduce((total, item) => total + item.attempts.length, 0);
  const artifactCount = evidence.reduce(
    (total, item) =>
      total + item.attempts.reduce((attemptTotal, attempt) => attemptTotal + attempt.artifacts.length, 0),
    0,
  );

  return (
    <section aria-labelledby="escalation-title" className="escalation-panel" tabIndex={0}>
      <span className="escalation-icon">
        <Icon name="alert" />
      </span>
      <div className="escalation-content">
        <span className="escalation-label">Attention · Escalation</span>
        {run.escalation ? (
          <>
            <h2 id="escalation-title">{run.escalation.summary}</h2>
            <dl className="escalation-facts">
              <div>
                <dt>{run.escalation.selector.kind === "gate" ? "Gate" : "Condition"}</dt>
                <dd>{run.escalation.selector.name}</dd>
              </div>
              <div>
                <dt>Selected branch</dt>
                <dd className="mono">{run.escalation.selectedBranch}</dd>
              </div>
              <div>
                <dt>Budget consumed</dt>
                <dd>
                  {run.escalation.budget.consumed} / {run.escalation.budget.limit}{" "}
                  {run.escalation.budget.kind} attempts
                </dd>
              </div>
              <div>
                <dt>Terminal reason</dt>
                <dd>{run.escalation.terminalReason}</dd>
              </div>
            </dl>
            {causalEvent && onFocusCausalEvent ? (
              <button className="causal-event-link" onClick={onFocusCausalEvent} type="button">
                <span>Causal event</span>
                <strong>
                  Seq {run.escalation.causalEventSeq} · {causalEvent.title}
                </strong>
                <Icon name="arrow" size={14} />
              </button>
            ) : (
              <div className="causal-event-link causal-event-unavailable">
                <span>Causal event</span>
                <strong>Seq {run.escalation.causalEventSeq} · Unavailable</strong>
              </div>
            )}
          </>
        ) : (
          <div className="escalation-unavailable">
            <h2 id="escalation-title">Escalation cause unavailable</h2>
            <p>
              This legacy run has no structured cause record. The gate or condition, selected
              branch, budget, terminal reason, and causal event are unavailable.
            </p>
          </div>
        )}

        <details className="escalation-evidence" open>
          <summary>
            Evidence at escalation
            <span>
              {evidenceUnavailable
                ? "Unavailable"
                : `${attemptCount} ${attemptCount === 1 ? "attempt" : "attempts"} · ${artifactCount} ${
                    artifactCount === 1 ? "artifact" : "artifacts"
                  }`}
            </span>
          </summary>
          {evidenceUnavailable && (
            <p className="empty-detail">
              Point-in-time evidence is unavailable because the causal event could not be resolved.
            </p>
          )}
          <div className="evidence-stage-list">
            {evidence.map(({ stage, attempts }) => {
              const stageArtifactCount = attempts.reduce(
                (total, attempt) => total + attempt.artifacts.length,
                0,
              );
              return (
                <section className="evidence-stage" key={stage.id}>
                  <button
                    aria-label={`Inspect ${stage.name} evidence: ${attempts.length} attempts, ${stageArtifactCount} artifacts`}
                    onClick={() => onSelectStage(stage.id)}
                    type="button"
                  >
                    <strong>{stage.name}</strong>
                    <span>
                      {attempts.length} {attempts.length === 1 ? "attempt" : "attempts"} ·{" "}
                      {stageArtifactCount} {stageArtifactCount === 1 ? "artifact" : "artifacts"}
                    </span>
                  </button>
                  <ol>
                    {attempts.map((attempt) => (
                      <li key={attempt.id}>
                        <div className="evidence-attempt">
                          <strong>
                            Attempt {attempt.number} · {attempt.kind}
                          </strong>
                          <span>
                            {attempt.status} · {attempt.duration}
                          </span>
                        </div>
                        <p>{attempt.summary}</p>
                        {attempt.artifacts.length > 0 && (
                          <ul aria-label={`${stage.name} attempt ${attempt.number} artifacts`}>
                            {attempt.artifacts.map((artifact) => (
                              <li key={artifact.name}>
                                <Icon name="artifact" size={14} />
                                <span>
                                  <strong>{artifact.name}</strong>
                                  <small className="mono">
                                    {artifact.mediaType} · {artifact.size}
                                    {artifact.digest ? ` · ${artifact.digest}` : ""}
                                  </small>
                                </span>
                              </li>
                            ))}
                          </ul>
                        )}
                      </li>
                    ))}
                  </ol>
                </section>
              );
            })}
          </div>
        </details>
      </div>
    </section>
  );
}
