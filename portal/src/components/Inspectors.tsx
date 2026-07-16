import { useEffect, useState } from "react";
import { Icon } from "../foundation/Icon";
import type { Run, StageAttempt, WorkflowStage } from "../prototypeData";

export function StageDefinition({ stage }: { stage: WorkflowStage }) {
  return (
    <aside className="definition-panel" aria-label={`${stage.name} definition`}>
      <InspectorHeading stage={stage} />
      <p className="inspector-description">{stage.description}</p>
      <dl className="property-list">
        <div>
          <dt>{stage.kind === "gate" ? "Evaluator" : "Goober"}</dt>
          <dd>{stage.evaluator ?? stage.goober}</dd>
        </div>
        <div>
          <dt>Policy</dt>
          <dd>{stage.retry}</dd>
        </div>
      </dl>
      <div className="code-heading">
        <span>Definition</span>
        <span>YAML</span>
      </div>
      <pre className="code-block">{stage.yaml}</pre>
    </aside>
  );
}

function InspectorHeading({ stage }: { stage: WorkflowStage }) {
  return (
    <div className="inspector-heading">
      <span className={`primitive-icon primitive-${stage.kind}`}>
        <Icon name={stage.kind === "gate" ? "gate" : stage.kind === "agentic" ? "code" : "workflow"} size={17} />
      </span>
      <div>
        <span>{stage.kind}</span>
        <h3>{stage.name}</h3>
      </div>
    </div>
  );
}

export function visibleAttempts(run: Run, stageId: string, eventSeq: number): StageAttempt[] {
  return run.attempts
    .filter((attempt) => attempt.stageId === stageId && attempt.startedSeq <= eventSeq)
    .map((attempt) => {
      if (attempt.endedSeq !== undefined && attempt.endedSeq <= eventSeq) {
        return attempt;
      }
      return {
        ...attempt,
        status: "running",
        duration: "In progress",
        output: undefined,
        artifacts: [],
      };
    });
}

export function AttemptInspector({
  run,
  stage,
  eventSeq,
}: {
  run: Run;
  stage: WorkflowStage;
  eventSeq: number;
}) {
  const attempts = visibleAttempts(run, stage.id, eventSeq);
  const [selectedAttemptNumber, setSelectedAttemptNumber] = useState<number>();
  const selectedAttempt =
    attempts.find((attempt) => attempt.number === selectedAttemptNumber) ?? attempts[attempts.length - 1];

  useEffect(() => {
    setSelectedAttemptNumber(undefined);
  }, [stage.id, eventSeq]);

  return (
    <aside className="run-inspector" aria-label={`${stage.name} attempt inspector`}>
      <InspectorHeading stage={stage} />

      {attempts.length === 0 ? (
        <div className="not-reached">
          <span>Not reached at this point</span>
          <small>Move the playhead forward to inspect this stage.</small>
        </div>
      ) : (
        <>
          <div className="attempt-switcher" aria-label="Stage attempts">
            {attempts.map((attempt) => (
              <button
                aria-pressed={selectedAttempt?.id === attempt.id}
                className={selectedAttempt?.id === attempt.id ? "attempt-button attempt-button-active" : "attempt-button"}
                key={attempt.id}
                onClick={() => setSelectedAttemptNumber(attempt.number)}
                type="button"
              >
                Attempt {attempt.number}
              </button>
            ))}
          </div>
          {selectedAttempt && (
            <div className="attempt-content">
              <div className="attempt-summary-row">
                <span className={`attempt-state attempt-${selectedAttempt.status}`}>{selectedAttempt.status}</span>
                <span className="mono">{selectedAttempt.duration}</span>
                <span>{selectedAttempt.kind}</span>
              </div>
              <p>{selectedAttempt.summary}</p>
              {selectedAttempt.output && (
                <div className="output-line">
                  <span>Output</span>
                  <code>{selectedAttempt.output}</code>
                </div>
              )}
              <div className="artifact-heading">
                <span>Artifacts</span>
                <span>{selectedAttempt.artifacts.length}</span>
              </div>
              {selectedAttempt.artifacts.length === 0 ? (
                <p className="empty-detail">No artifacts recorded yet.</p>
              ) : (
                <div className="artifact-list">
                  {selectedAttempt.artifacts.map((artifact) => (
                    <div className="artifact-row" key={artifact.name}>
                      <Icon name="artifact" size={17} />
                      <span>
                        <strong>{artifact.name}</strong>
                        <small>{artifact.summary}</small>
                      </span>
                      <span className="artifact-size">{artifact.size}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
          )}
        </>
      )}

      <details className="definition-disclosure">
        <summary>Stage definition</summary>
        <p>{stage.description}</p>
        <pre className="code-block">{stage.yaml}</pre>
      </details>
    </aside>
  );
}
