import { useEffect, useRef, useState } from "react";
import type { Artifact, Run, WorkflowStage } from "../prototypeData";
import { visibleAttempts } from "../runState";
import { Icon } from "../ui/Icon";
import { Inspector, InspectorHeading } from "../ui/Inspector";
import { ArtifactViewer, canPreviewArtifact } from "./ArtifactViewer";

interface AttemptInspectorProps {
  eventAttemptNumber?: number;
  eventSeq: number;
  run: Run;
  stage: WorkflowStage;
}

export function AttemptInspector({ run, stage, eventSeq, eventAttemptNumber }: AttemptInspectorProps) {
  const attempts = visibleAttempts(run, stage.id, eventSeq);
  const [selectedAttemptNumber, setSelectedAttemptNumber] = useState<number | undefined>(eventAttemptNumber);
  const [selectedArtifact, setSelectedArtifact] = useState<Artifact>();
  const artifactTrigger = useRef<HTMLButtonElement | null>(null);
  const attemptButtons = useRef<Array<HTMLButtonElement | null>>([]);
  const selectedAttempt =
    attempts.find((attempt) => attempt.number === selectedAttemptNumber) ?? attempts[attempts.length - 1];

  useEffect(() => {
    setSelectedAttemptNumber(eventAttemptNumber);
  }, [stage.id, eventSeq, eventAttemptNumber]);

  const selectAttemptAt = (index: number) => {
    const attempt = attempts[index];
    if (!attempt) {
      return;
    }
    setSelectedAttemptNumber(attempt.number);
    attemptButtons.current[index]?.focus();
  };

  const onAttemptKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    let nextIndex: number | undefined;
    if (event.key === "ArrowDown" || event.key === "ArrowRight") {
      nextIndex = (index + 1) % attempts.length;
    } else if (event.key === "ArrowUp" || event.key === "ArrowLeft") {
      nextIndex = (index - 1 + attempts.length) % attempts.length;
    } else if (event.key === "Home") {
      nextIndex = 0;
    } else if (event.key === "End") {
      nextIndex = attempts.length - 1;
    }
    if (nextIndex !== undefined) {
      event.preventDefault();
      selectAttemptAt(nextIndex);
    }
  };

  const openArtifact = (artifact: Artifact, trigger: HTMLButtonElement) => {
    artifactTrigger.current = trigger;
    setSelectedArtifact(artifact);
  };

  const closeArtifact = () => {
    setSelectedArtifact(undefined);
  };

  useEffect(() => {
    if (!selectedArtifact) {
      artifactTrigger.current?.focus();
    }
  }, [selectedArtifact]);

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
            {attempts.map((attempt, index) => (
              <button
                aria-pressed={selectedAttempt?.id === attempt.id}
                className={
                  selectedAttempt?.id === attempt.id
                    ? "attempt-button attempt-button-active"
                    : "attempt-button"
                }
                key={attempt.id}
                onClick={() => selectAttemptAt(index)}
                onKeyDown={(event) => onAttemptKeyDown(event, index)}
                ref={(element) => {
                  attemptButtons.current[index] = element;
                }}
                tabIndex={selectedAttempt?.id === attempt.id ? 0 : -1}
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
                    <article className="artifact-row" key={artifact.name}>
                      <div className="artifact-row-heading">
                        <Icon name="artifact" size={17} />
                        <span>
                          <strong>{artifact.name}</strong>
                          <small>{artifact.summary}</small>
                        </span>
                      </div>
                      <dl className="artifact-metadata">
                        <div>
                          <dt>Media</dt>
                          <dd>{artifact.mediaType}</dd>
                        </div>
                        <div>
                          <dt>Size</dt>
                          <dd>{artifact.size}</dd>
                        </div>
                        <div>
                          <dt>Provenance</dt>
                          <dd>
                            Attempt {selectedAttempt.number} · Seq {artifact.recordedSeq}
                          </dd>
                        </div>
                        <div className="artifact-digest">
                          <dt>Digest</dt>
                          <dd>
                            <code>{artifact.digest}</code>
                            <span className={artifact.digestVerified ? "digest-verified" : "digest-unverified"}>
                              {artifact.digestVerified ? "Verified" : "Unverified"}
                            </span>
                          </dd>
                        </div>
                      </dl>
                      {canPreviewArtifact(artifact) ? (
                        <button
                          className="artifact-action"
                          onClick={(event) => openArtifact(artifact, event.currentTarget)}
                          type="button"
                        >
                          View content
                        </button>
                      ) : artifact.downloadUrl ? (
                        <a className="artifact-action" download href={artifact.downloadUrl}>
                          Download
                        </a>
                      ) : (
                        <span className="artifact-access-note">Metadata only</span>
                      )}
                    </article>
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

      {selectedArtifact && selectedAttempt && (
        <ArtifactViewer
          artifact={selectedArtifact}
          attempt={selectedAttempt}
          onClose={closeArtifact}
          run={run}
          stage={stage}
        />
      )}
    </Inspector>
  );
}
