import { useEffect, useRef, useState } from "react";
import type {
  ArtifactContent,
  ArtifactMetadata,
  DaemonClient,
  StageAttempt,
  WorkflowGraphNode,
} from "../api/types";
import { formatDuration } from "../runDetailData";
import { Icon } from "../ui/Icon";
import { Inspector } from "../ui/Inspector";

// Only safe, textual artifact bodies are previewed inline.
const previewableMediaTypes = new Set([
  "application/json",
  "application/yaml",
  "text/markdown",
  "text/plain",
  "text/yaml",
]);

function canPreview(media: string): boolean {
  return previewableMediaTypes.has(media.toLowerCase());
}

function attemptStatusLabel(status: StageAttempt["status"]): string {
  return status === "" ? "pending" : status;
}

function nodeIcon(kind: WorkflowGraphNode["kind"]): "gate" | "code" | "workflow" {
  return kind === "gate" ? "gate" : kind === "agentic" ? "code" : "workflow";
}

// RunStageInspector drills into a selected graph node's stage: it loads that
// stage's live attempts (DASH-20), shows the current attempt as of the selected
// sequence — status, outputs, artifacts with digest/provenance — and previews
// textual artifact bodies on demand. The daemon already returns all of this;
// the run page previously never read it.
export function RunStageInspector({
  client,
  runId,
  node,
  selectedSeq,
  inspectorRef,
}: {
  client: DaemonClient;
  runId: string;
  node: WorkflowGraphNode | undefined;
  selectedSeq: number;
  inspectorRef?: React.Ref<HTMLElement>;
}) {
  const [attempts, setAttempts] = useState<StageAttempt[]>([]);
  const [loadState, setLoadState] = useState<"idle" | "loading" | "error">("idle");
  const [error, setError] = useState<string>();
  const [selectedNumber, setSelectedNumber] = useState<number>();
  const attemptButtons = useRef<Array<HTMLButtonElement | null>>([]);

  const stageId = node?.id;
  useEffect(() => {
    if (!stageId) {
      setAttempts([]);
      return;
    }
    const controller = new AbortController();
    setLoadState("loading");
    setError(undefined);
    client
      .listStageAttempts(runId, stageId, { signal: controller.signal })
      .then((list) => {
        setAttempts(list.attempts);
        setLoadState("idle");
        setSelectedNumber(undefined);
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) {
          return;
        }
        setError(err instanceof Error ? err.message : "Unknown error");
        setLoadState("error");
      });
    return () => controller.abort();
  }, [client, runId, stageId]);

  if (!node) {
    return (
      <Inspector className="run-inspector" label="Stage inspector" rootRef={inspectorRef}>
        <div className="not-reached">
          <span>Select a node</span>
          <small>Choose a stage in the graph to inspect its attempts.</small>
        </div>
      </Inspector>
    );
  }

  // Only attempts started by the selected sequence are visible on the playhead.
  const visible = attempts.filter(
    (attempt) => attempt.startedSeq === undefined || attempt.startedSeq <= selectedSeq,
  );
  const selected =
    visible.find((attempt) => attempt.number === selectedNumber) ?? visible[visible.length - 1];

  const moveSelection = (index: number) => {
    const attempt = visible[index];
    if (!attempt) {
      return;
    }
    setSelectedNumber(attempt.number);
    attemptButtons.current[index]?.focus();
  };
  const onAttemptKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    if (event.key === "ArrowDown" || event.key === "ArrowRight") {
      event.preventDefault();
      moveSelection((index + 1) % visible.length);
    } else if (event.key === "ArrowUp" || event.key === "ArrowLeft") {
      event.preventDefault();
      moveSelection((index - 1 + visible.length) % visible.length);
    }
  };

  return (
    <Inspector
      className="run-inspector"
      label={`${node.id} attempt inspector`}
      rootRef={inspectorRef}
    >
      <div className="inspector-heading">
        <span className={`primitive-icon primitive-${node.kind}`}>
          <Icon name={nodeIcon(node.kind)} size={17} />
        </span>
        <div>
          <span>{node.kind}</span>
          <h3>{node.id}</h3>
        </div>
      </div>

      {loadState === "loading" && (
        <div className="artifact-load-state" role="status">
          <span aria-hidden="true" className="loading-mark" />
          <span>Loading attempts…</span>
        </div>
      )}
      {loadState === "error" && (
        <div className="artifact-load-error" role="alert">
          Could not load attempts: {error}
        </div>
      )}

      {loadState === "idle" &&
        (visible.length === 0 ? (
          <div className="not-reached">
            <span>Not reached at this point</span>
            <small>Move the playhead forward to inspect this stage.</small>
          </div>
        ) : (
          <>
            {visible.length > 1 && (
              <div aria-label="Stage attempts" className="attempt-switcher" role="group">
                {visible.map((attempt, index) => (
                  <button
                    aria-pressed={selected?.number === attempt.number}
                    className={
                      selected?.number === attempt.number
                        ? "attempt-button attempt-button-active"
                        : "attempt-button"
                    }
                    key={attempt.number}
                    onClick={() => setSelectedNumber(attempt.number)}
                    onKeyDown={(event) => onAttemptKeyDown(event, index)}
                    ref={(element) => {
                      attemptButtons.current[index] = element;
                    }}
                    tabIndex={selected?.number === attempt.number ? 0 : -1}
                    type="button"
                  >
                    Attempt {attempt.number}
                  </button>
                ))}
              </div>
            )}
            {selected && (
              <AttemptDetail attempt={selected} client={client} runId={runId} />
            )}
          </>
        ))}
    </Inspector>
  );
}

function AttemptDetail({
  attempt,
  client,
  runId,
}: {
  attempt: StageAttempt;
  client: DaemonClient;
  runId: string;
}) {
  const outputs = Object.entries(attempt.outputs ?? {});
  const status = attemptStatusLabel(attempt.status);
  return (
    <div className="attempt-content">
      <div className="attempt-summary-row">
        <span className={`attempt-state attempt-${status}`}>{status}</span>
        <span className="mono">{formatDuration(attempt.durationMillis)}</span>
        <span>{attempt.class}</span>
      </div>
      {attempt.error && (
        <p className="artifact-load-error">
          {attempt.error.code}
          {attempt.error.message ? `: ${attempt.error.message}` : ""}
        </p>
      )}
      {outputs.length > 0 && (
        <details className="definition-disclosure" open>
          <summary>Outputs</summary>
          {outputs.map(([key, value]) => (
            <div className="output-line" key={key}>
              <span>{key}</span>
              <code>{typeof value === "string" ? value : JSON.stringify(value)}</code>
            </div>
          ))}
        </details>
      )}
      <div className="artifact-heading">
        <span>Artifacts</span>
        <span>{attempt.artifacts.length}</span>
      </div>
      {attempt.artifacts.length === 0 ? (
        <p className="empty-detail">No artifacts recorded.</p>
      ) : (
        <div className="artifact-list">
          {attempt.artifacts.map((artifact) => (
            <ArtifactRow
              artifact={artifact}
              attemptNumber={attempt.number}
              client={client}
              key={artifact.digest}
              runId={runId}
            />
          ))}
        </div>
      )}
    </div>
  );
}

function ArtifactRow({
  artifact,
  attemptNumber,
  client,
  runId,
}: {
  artifact: ArtifactMetadata;
  attemptNumber: number;
  client: DaemonClient;
  runId: string;
}) {
  const [content, setContent] = useState<ArtifactContent>();
  const [state, setState] = useState<"idle" | "loading" | "error">("idle");
  const [error, setError] = useState<string>();

  const load = () => {
    setState("loading");
    setError(undefined);
    client
      .getArtifact(runId, artifact.digest)
      .then((value) => {
        setContent(value);
        setState("idle");
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : "Unknown error");
        setState("error");
      });
  };

  return (
    <article className="artifact-row">
      <div className="artifact-row-heading">
        <Icon name="artifact" size={17} />
        <span>
          <strong>{artifact.name ?? artifact.digest}</strong>
          <small>{artifact.mediaType}</small>
        </span>
      </div>
      <dl className="artifact-metadata">
        <div>
          <dt>Size</dt>
          <dd className="artifact-size">{artifact.size}</dd>
        </div>
        <div>
          <dt>Provenance</dt>
          <dd>
            Attempt {attemptNumber}
            {artifact.recordedSeq !== undefined ? ` · Seq ${artifact.recordedSeq}` : ""}
          </dd>
        </div>
        <div className="artifact-digest">
          <dt>Digest</dt>
          <dd>
            <code>{artifact.digest}</code>
          </dd>
        </div>
      </dl>
      {canPreview(artifact.mediaType) ? (
        content ? (
          <pre className="artifact-content code-block">
            {new TextDecoder().decode(content.bytes)}
          </pre>
        ) : (
          <button
            className="artifact-action"
            disabled={state === "loading"}
            onClick={load}
            type="button"
          >
            {state === "loading" ? "Loading…" : "View content"}
          </button>
        )
      ) : (
        <span className="artifact-access-note">Metadata only</span>
      )}
      {state === "error" && (
        <p className="artifact-load-error" role="alert">
          Could not load artifact: {error}
        </p>
      )}
    </article>
  );
}
