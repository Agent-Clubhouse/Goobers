import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import type { Artifact, Run, StageAttempt, WorkflowStage } from "../prototypeData";
import { Icon } from "../ui/Icon";

const safeArtifactMediaTypes = new Set([
  "application/json",
  "application/yaml",
  "text/markdown",
  "text/plain",
  "text/yaml",
]);

export function canPreviewArtifact(artifact: Artifact): boolean {
  return (
    safeArtifactMediaTypes.has(artifact.mediaType.toLowerCase()) &&
    (artifact.content !== undefined || artifact.contentError !== undefined)
  );
}

function loadArtifactContent(artifact: Artifact): Promise<string> {
  return new Promise((resolve, reject) => {
    window.setTimeout(() => {
      if (artifact.contentError) {
        reject(new Error(artifact.contentError));
        return;
      }
      if (artifact.content === undefined) {
        reject(new Error("Artifact content is not available."));
        return;
      }
      if (artifact.mediaType.toLowerCase() === "application/json") {
        try {
          resolve(JSON.stringify(JSON.parse(artifact.content), null, 2));
        } catch {
          reject(new Error("Artifact content is not valid JSON."));
        }
        return;
      }
      resolve(artifact.content);
    }, 20);
  });
}

interface ArtifactViewerProps {
  artifact: Artifact;
  attempt: StageAttempt;
  run: Run;
  stage: WorkflowStage;
  onClose: () => void;
}

export function ArtifactViewer({ artifact, attempt, run, stage, onClose }: ArtifactViewerProps) {
  const [loadAttempt, setLoadAttempt] = useState(0);
  const [contentState, setContentState] = useState<
    { status: "loading" } | { status: "ready"; content: string } | { status: "error"; message: string }
  >({ status: "loading" });
  const backdrop = useRef<HTMLDivElement>(null);
  const dialog = useRef<HTMLElement>(null);

  useEffect(() => {
    let current = true;
    setContentState({ status: "loading" });
    loadArtifactContent(artifact).then(
      (content) => {
        if (current) {
          setContentState({ status: "ready", content });
        }
      },
      (error: unknown) => {
        if (current) {
          setContentState({
            status: "error",
            message: error instanceof Error ? error.message : "Artifact content could not be loaded.",
          });
        }
      },
    );
    return () => {
      current = false;
    };
  }, [artifact, loadAttempt]);

  useEffect(() => {
    const backgroundElements = Array.from(document.body.children).filter(
      (element): element is HTMLElement => element instanceof HTMLElement && element !== backdrop.current,
    );
    const inertStates = backgroundElements.map((element) => element.hasAttribute("inert"));
    backgroundElements.forEach((element) => element.setAttribute("inert", ""));

    return () => {
      backgroundElements.forEach((element, index) => {
        if (!inertStates[index]) {
          element.removeAttribute("inert");
        }
      });
    };
  }, []);

  const onDialogKeyDown = (event: React.KeyboardEvent<HTMLElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      onClose();
      return;
    }
    if (event.key !== "Tab") {
      return;
    }

    const focusable = Array.from(
      dialog.current?.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      ) ?? [],
    ).filter((element) => element.tabIndex >= 0);
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (!first || !last) {
      event.preventDefault();
      return;
    }

    if (event.shiftKey && (document.activeElement === first || !dialog.current?.contains(document.activeElement))) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && (document.activeElement === last || !dialog.current?.contains(document.activeElement))) {
      event.preventDefault();
      first.focus();
    }
  };

  return createPortal(
    <div className="artifact-dialog-backdrop" ref={backdrop}>
      <section
        aria-labelledby="artifact-dialog-title"
        aria-modal="true"
        className="artifact-dialog"
        onKeyDown={onDialogKeyDown}
        ref={dialog}
        role="dialog"
      >
        <header>
          <div>
            <span className="section-kicker">Artifact content</span>
            <h2 id="artifact-dialog-title">{artifact.name}</h2>
          </div>
          <button aria-label="Close artifact viewer" autoFocus className="dialog-close" onClick={onClose} type="button">
            <Icon name="close" size={16} />
          </button>
        </header>
        <dl className="artifact-dialog-meta">
          <div>
            <dt>Provenance</dt>
            <dd>
              {run.shortId} · {stage.name} · Attempt {attempt.number} · Seq {artifact.recordedSeq}
            </dd>
          </div>
          <div>
            <dt>Media</dt>
            <dd>
              {artifact.mediaType} · {artifact.size}
            </dd>
          </div>
          <div>
            <dt>Digest</dt>
            <dd>
              <code>{artifact.digest}</code> · {artifact.digestVerified ? "Verified" : "Unverified"}
            </dd>
          </div>
        </dl>
        {contentState.status === "loading" && (
          <div className="artifact-load-state" role="status">
            Loading artifact content…
          </div>
        )}
        {contentState.status === "error" && (
          <div className="artifact-load-state artifact-load-error" role="alert">
            <strong>Artifact unavailable</strong>
            <span>{contentState.message}</span>
            <button onClick={() => setLoadAttempt((attemptNumber) => attemptNumber + 1)} type="button">
              Retry
            </button>
          </div>
        )}
        {contentState.status === "ready" && (
          <pre aria-label={`${artifact.name} content`} className="artifact-content">
            {contentState.content}
          </pre>
        )}
      </section>
    </div>,
    document.body,
  );
}
