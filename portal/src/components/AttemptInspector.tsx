import { useEffect, useState } from "react";
import type { Run, WorkflowStage } from "../prototypeData";
import { visibleAttempts } from "../runState";
import { Icon } from "../ui/Icon";
import { Inspector, InspectorHeading } from "../ui/Inspector";

interface AttemptInspectorProps {
  eventSeq: number;
  run: Run;
  stage: WorkflowStage;
}

export function AttemptInspector({ run, stage, eventSeq }: AttemptInspectorProps) {
  const attempts = visibleAttempts(run, stage.id, eventSeq);
  const [selectedAttemptNumber, setSelectedAttemptNumber] = useState<number>();
  const selectedAttempt =
    attempts.find((attempt) => attempt.number === selectedAttemptNumber) ??
    attempts[attempts.length - 1];

  useEffect(() => {
    setSelectedAttemptNumber(undefined);
  }, [stage.id, eventSeq]);

  return (
    <Inspector className="run-inspector" label={`${stage.name} attempt inspector`}>
      <InspectorHeading stage={stage} />

      {attempts.length === 0 ? (
        <div className="not-reached">
          <span>Not reached at this point</span>
          <small>Move the playhead forward to inspect this stage.</small>
        </div>
      ) : (
        <>
          <div className="attempt-switcher" aria-label="Stage attempts" role="group">
            {attempts.map((attempt) => (
              <button
                aria-pressed={selectedAttempt?.id === attempt.id}
                className={
                  selectedAttempt?.id === attempt.id
                    ? "attempt-button attempt-button-active"
                    : "attempt-button"
                }
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
                <span className={`attempt-state attempt-${selectedAttempt.status}`}>
                  {selectedAttempt.status}
                </span>
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
    </Inspector>
  );
}
