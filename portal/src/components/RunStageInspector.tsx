import { useEffect, useRef, useState } from "react";
import { useLiveData } from "../liveData";
import type {
  ArtifactContent,
  ArtifactMetadata,
  DaemonClient,
  RunEvent,
  StageAttempt,
  WorkflowGraphNode,
} from "../api/types";
import {
  eventHeading,
  eventSummary,
  formatDuration,
  isTranscriptEvent,
} from "../runDetailData";
import {
  isEmptyTurn,
  parseTranscript,
  transcriptSearchMatches,
  type TranscriptTurn,
  type TranscriptUsage,
} from "../transcript";
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

interface StageVisit {
  id: string;
  ordinal: number;
  attempts: StageAttempt[];
}

function groupAttemptsByVisit(attempts: StageAttempt[]): StageVisit[] {
  const visits: StageVisit[] = [];
  const byOrdinal = new Map<number, StageVisit>();
  for (const attempt of attempts) {
    let visit = byOrdinal.get(attempt.visit);
    if (!visit) {
      visit = { id: attempt.id, ordinal: attempt.visit, attempts: [] };
      visits.push(visit);
      byOrdinal.set(attempt.visit, visit);
    }
    visit.attempts.push(attempt);
  }
  return visits;
}

function attemptLabel(attempt: StageAttempt): string {
  const retry = attempt.class === "initial" ? "" : ` (${attempt.class} retry)`;
  return `Attempt ${attempt.number}${retry}`;
}

function repassDecision(
  events: RunEvent[],
  stageId: string,
  visit: StageVisit,
  previousVisit: StageVisit | undefined,
): RunEvent | undefined {
  const startedSeq = visit.attempts[0]?.startedSeq;
  if (visit.ordinal === 1 || startedSeq === undefined) {
    return undefined;
  }
  const previousFinishedSeq = Math.max(
    0,
    ...(previousVisit?.attempts.map(
      (attempt) => attempt.finishedSeq ?? attempt.startedSeq ?? 0,
    ) ?? []),
  );
  for (let index = events.length - 1; index >= 0; index -= 1) {
    const event = events[index];
    if (
      event.knownSchema &&
      event.type === "gate.evaluated" &&
      event.target === stageId &&
      event.seq > previousFinishedSeq &&
      event.seq < startedSeq
    ) {
      return event;
    }
  }
  return undefined;
}

// RunStageInspector drills into a selected graph node's attempts or the
// transcript/artifact evidence selected from the journal.
export function RunStageInspector({
  client,
  runId,
  node,
  workflow,
  selectedSeq,
  events = [],
  inspectorRef,
  onSelectAttempt,
  selectedEvidence,
  selectedEvidenceVisit,
}: {
  client: DaemonClient;
  runId: string;
  node: WorkflowGraphNode | undefined;
  /** The run's own workflow, shown alongside the stage id for context (#2538). This is a run-level constant, not a phase tier — #2538's phase/stage nesting criterion needs a backend concept that doesn't exist yet (tracked in #2599). */
  workflow?: string;
  selectedSeq: number;
  events?: RunEvent[];
  inspectorRef?: React.Ref<HTMLElement>;
  onSelectAttempt?: (isLatest: boolean) => void;
  selectedEvidence?: RunEvent;
  selectedEvidenceVisit?: number;
}) {
  const [attempts, setAttempts] = useState<StageAttempt[]>([]);
  const [loadState, setLoadState] = useState<"idle" | "loading" | "error">("idle");
  const [error, setError] = useState<string>();
  const [selectedId, setSelectedId] = useState<string>();
  const visitButtons = useRef<Array<HTMLButtonElement | null>>([]);
  const attemptButtons = useRef<Array<HTMLButtonElement | null>>([]);

  const stageId = node?.id;
  const selectedEvidenceKey = selectedEvidence
    ? `${selectedEvidence.branch}-${selectedEvidence.seq}`
    : undefined;
  useEffect(() => {
    setAttempts([]);
    setSelectedId(undefined);
    setLoadState("idle");
    setError(undefined);
  }, [runId, selectedEvidenceKey, stageId]);

  // Bumped by a live event for this run, which re-runs the fetch below.
  //
  // #1714: the attempt list was fetched in a plain effect keyed on
  // [client, runId, stageId] and never subscribed to live data. RunPage keys
  // RunDetailWorkspace on run.id, which is stable across refreshes, so the
  // inspector was never remounted either. A user watching a running stage retry
  // three times saw the graph and event timeline update while the attempt list
  // stayed frozen at whatever it was when the node was first clicked — on the
  // portal's primary live-debugging surface. They had to click away and back.
  const [liveRevision, setLiveRevision] = useState(0);
  const { subscribe } = useLiveData();
  useEffect(() => {
    if (!runId) {
      return;
    }
    // Scoped to this run: an event for a different run must not refetch this
    // one's attempts. On a busy instance that would be a request per event.
    return subscribe(
      ["run"],
      (_models, reason) => {
        // "initial" is the subscription handshake, not a change. The fetch
        // effect below already runs on mount, so acting on it would double
        // every load.
        if (reason !== "initial") {
          setLiveRevision((revision) => revision + 1);
        }
        return true;
      },
      { runId },
    );
  }, [runId, subscribe]);

  useEffect(() => {
    if (!stageId) {
      setAttempts([]);
      return;
    }
    if (selectedEvidence) {
      setAttempts([]);
      setLoadState("idle");
      setError(undefined);
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
      })
      .catch((err: unknown) => {
        if (controller.signal.aborted) {
          return;
        }
        setError(err instanceof Error ? err.message : "Unknown error");
        setLoadState("error");
      });
    return () => controller.abort();
  }, [client, liveRevision, runId, selectedEvidence, stageId]);

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
  const visits = groupAttemptsByVisit(visible);
  const selected =
    visible.find((attempt) => attempt.id === selectedId) ?? visible[visible.length - 1];
  const selectedVisitIndex = visits.findIndex((visit) => visit.ordinal === selected?.visit);
  const selectedVisit = visits[selectedVisitIndex];
  const decision = selectedVisit
    ? repassDecision(events, node.id, selectedVisit, visits[selectedVisitIndex - 1])
    : undefined;

  const selectAttempt = (attempt: StageAttempt) => {
    setSelectedId(attempt.id);
    onSelectAttempt?.(attempt.id === visible[visible.length - 1]?.id);
  };

  const selectVisit = (visit: StageVisit) => {
    const attempt = visit.attempts[visit.attempts.length - 1];
    if (!attempt) {
      return;
    }
    selectAttempt(attempt);
  };

  const moveVisitSelection = (index: number) => {
    const visit = visits[index];
    if (!visit) {
      return;
    }
    selectVisit(visit);
    visitButtons.current[index]?.focus();
  };
  const onVisitKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    if (event.key === "ArrowDown" || event.key === "ArrowRight") {
      event.preventDefault();
      moveVisitSelection((index + 1) % visits.length);
    } else if (event.key === "ArrowUp" || event.key === "ArrowLeft") {
      event.preventDefault();
      moveVisitSelection((index - 1 + visits.length) % visits.length);
    }
  };

  const moveAttemptSelection = (index: number) => {
    const attempt = selectedVisit?.attempts[index];
    if (!attempt) {
      return;
    }
    selectAttempt(attempt);
    attemptButtons.current[index]?.focus();
  };
  const onAttemptKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    const count = selectedVisit?.attempts.length ?? 0;
    if (event.key === "ArrowDown" || event.key === "ArrowRight") {
      event.preventDefault();
      moveAttemptSelection((index + 1) % count);
    } else if (event.key === "ArrowUp" || event.key === "ArrowLeft") {
      event.preventDefault();
      moveAttemptSelection((index - 1 + count) % count);
    }
  };

  return (
    <Inspector
      className="run-inspector"
      label={
        workflow ? `${workflow} · ${node.id} attempt inspector` : `${node.id} attempt inspector`
      }
      rootRef={inspectorRef}
    >
      <div className="inspector-heading">
        <span className={`primitive-icon primitive-${node.kind}`}>
          <Icon name={nodeIcon(node.kind)} size={17} />
        </span>
        <div>
          <span className="inspector-scope">
            {workflow ? `${workflow} · ${node.kind}` : node.kind}
          </span>
          <h3>{node.id}</h3>
          {node.owner && <span className="inspector-owner">Owned by {node.owner}</span>}
        </div>
      </div>

      {selectedEvidence ? (
        <EvidenceDetail
          client={client}
          event={selectedEvidence}
          key={`${selectedEvidence.branch}-${selectedEvidence.seq}`}
          node={node}
          runId={runId}
          visit={selectedEvidenceVisit}
        />
      ) : (
        <>
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

          {(loadState === "idle" || visible.length > 0) &&
            (visible.length === 0 ? (
              <div className="not-reached">
                <span>Not reached at this point</span>
                <small>Move the playhead forward to inspect this stage.</small>
              </div>
            ) : (
              <>
                {visible.length > 1 && (
                  <>
                    <div aria-label="Stage visits" className="attempt-switcher" role="group">
                      {visits.map((visit, index) => (
                        <button
                          aria-pressed={selectedVisit?.ordinal === visit.ordinal}
                          className={
                            selectedVisit?.ordinal === visit.ordinal
                              ? "attempt-button attempt-button-active"
                              : "attempt-button"
                          }
                          key={visit.id}
                          onClick={() => selectVisit(visit)}
                          onKeyDown={(event) => onVisitKeyDown(event, index)}
                          ref={(element) => {
                            visitButtons.current[index] = element;
                          }}
                          tabIndex={selectedVisit?.ordinal === visit.ordinal ? 0 : -1}
                          type="button"
                        >
                          Visit {visit.ordinal}
                        </button>
                      ))}
                    </div>
                    {selectedVisit && (
                      <div
                        aria-label={`Visit ${selectedVisit.ordinal} attempts`}
                        className="retry-switcher"
                        role="group"
                      >
                        {selectedVisit.attempts.map((attempt, index) => (
                          <button
                            aria-label={`Visit ${selectedVisit.ordinal} · ${attemptLabel(attempt)}`}
                            aria-pressed={selected?.id === attempt.id}
                            className={
                              selected?.id === attempt.id
                                ? "attempt-button attempt-button-active"
                                : "attempt-button"
                            }
                            key={attempt.id}
                            onClick={() => selectAttempt(attempt)}
                            onKeyDown={(event) => onAttemptKeyDown(event, index)}
                            ref={(element) => {
                              attemptButtons.current[index] = element;
                            }}
                            tabIndex={selected?.id === attempt.id ? 0 : -1}
                            type="button"
                          >
                            {attemptLabel(attempt)}
                          </button>
                        ))}
                      </div>
                    )}
                  </>
                )}
                {decision && (
                  <div className="repass-context">
                    <span>
                      Repass decision · Sequence {decision.seq}
                    </span>
                    <strong>{eventSummary(decision)}</strong>
                  </div>
                )}
                {selected && (
                  <AttemptDetail attempt={selected} client={client} runId={runId} />
                )}
              </>
            ))}
        </>
      )}
    </Inspector>
  );
}

function EvidenceDetail({
  client,
  event,
  node,
  runId,
  visit,
}: {
  client: DaemonClient;
  event: RunEvent;
  node: WorkflowGraphNode;
  runId: string;
  visit?: number;
}) {
  return (
    <div className="attempt-content">
      <div className="repass-context">
        <span>
          {node.id} evidence · Visit {visit ?? "unknown"} · Sequence {event.seq}
        </span>
        <strong>{eventHeading(event)}</strong>
      </div>
      <EvidencePayload client={client} event={event} runId={runId} visit={visit} />
    </div>
  );
}

// EvidencePayload renders whatever an inspectable evidence event (transcript
// or artifact) carries, without assuming a graph node context — the shared
// core EvidenceDetail wraps for the stage inspector and KeyMomentsDigest
// reuses directly for its inline "state change and payload" preview (#2537),
// so the two views never grow separate rendering logic for the same evidence.
export function EvidencePayload({
  client,
  event,
  runId,
  visit,
}: {
  client: DaemonClient;
  event: RunEvent;
  runId: string;
  visit?: number;
}) {
  if (isTranscriptEvent(event)) {
    return <TranscriptRow client={client} event={event} runId={runId} />;
  }
  if (event.artifact) {
    return (
      <div className="artifact-list">
        <ArtifactRow
          artifact={{
            ...event.artifact,
            recordedSeq: event.artifact.recordedSeq ?? event.seq,
          }}
          attemptNumber={event.artifact.attempt ?? event.attempt}
          attemptVisit={visit}
          client={client}
          runId={runId}
        />
      </div>
    );
  }
  return (
    <p className="artifact-load-error" role="alert">
      Evidence content is unavailable.
    </p>
  );
}

function TranscriptRow({
  client,
  event,
  runId,
}: {
  client: DaemonClient;
  event: RunEvent;
  runId: string;
}) {
  const [content, setContent] = useState<string>();
  const [state, setState] = useState<"idle" | "loading" | "error">("idle");
  const [error, setError] = useState<string>();

  const load = () => {
    setState("loading");
    setError(undefined);
    client
      .getTranscript(runId, event.seq)
      .then((value) => {
        setContent(new TextDecoder().decode(value.bytes));
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
          <strong>Transcript</strong>
          <small>{event.name ?? "transcript"}</small>
        </span>
      </div>
      {content === undefined ? (
        <button
          className="artifact-action"
          disabled={state === "loading"}
          onClick={load}
          type="button"
        >
          {state === "loading" ? "Loading…" : "View transcript"}
        </button>
      ) : (
        <TranscriptView
          attempt={event.attempt}
          stage={event.stage}
          text={content}
        />
      )}
      {state === "error" && (
        <p className="artifact-load-error" role="alert">
          Could not load transcript: {error}
        </p>
      )}
    </article>
  );
}

// How many turns render before the reader asks for more. A real transcript runs
// to 160+ turns and hundreds of kilobytes; mounting all of it stalls a run page
// that is already under load-time pressure.
const TRANSCRIPT_WINDOW = 40;

const ROLE_LABELS: Record<TranscriptTurn["role"], string> = {
  system: "System",
  user: "User",
  assistant: "Assistant",
  tool: "Tool",
  unknown: "Unrecognized",
};

function formatTokens(value: number): string {
  return value.toLocaleString("en-US");
}

function usageSummary(usage: TranscriptUsage): string {
  const parts: string[] = [];
  if (usage.inputTokens !== undefined) parts.push(`${formatTokens(usage.inputTokens)} in`);
  if (usage.outputTokens !== undefined) parts.push(`${formatTokens(usage.outputTokens)} out`);
  if (usage.reasoningTokens !== undefined) parts.push(`${formatTokens(usage.reasoningTokens)} reasoning`);
  if (usage.requests !== undefined) parts.push(`${formatTokens(usage.requests)} requests`);
  return parts.join(" · ");
}

/**
 * TranscriptView renders a captured transcript as a conversation.
 *
 * The transcript is the only record of why an agentic stage did what it did,
 * so diagnosing a stage that "succeeded" while changing nothing means reading
 * it. As raw JSONL that was possible but not practical. Raw bytes stay one
 * click away — this replaces the default view, not the evidence.
 */
export function TranscriptView({
  text,
  stage,
  attempt,
}: {
  text: string;
  stage?: string;
  attempt?: number;
}) {
  const [query, setQuery] = useState("");
  const [role, setRole] = useState<TranscriptTurn["role"] | "">("");
  const [expanded, setExpanded] = useState(false);
  const [raw, setRaw] = useState(false);

  const parsed = parseTranscript(text);
  const turns = parsed.turns.filter(
    (turn) => !isEmptyTurn(turn) && (!role || turn.role === role),
  );
  const matches = transcriptSearchMatches(turns, query);
  const matching = query.trim() === "" ? turns : turns.filter((turn) => matches.includes(turn.index));
  const visible = expanded ? matching : matching.slice(0, TRANSCRIPT_WINDOW);
  const hidden = matching.length - visible.length;

  if (raw) {
    return (
      <div className="transcript">
        <div className="transcript-toolbar">
          <button className="artifact-action" onClick={() => setRaw(false)} type="button">
            Show conversation
          </button>
        </div>
        <pre className="artifact-content code-block">{text}</pre>
      </div>
    );
  }

  return (
    <div className="transcript">
      <div className="transcript-toolbar">
        <label className="transcript-search">
          <span className="sr-only">Search transcript</span>
          <input
            onChange={(changeEvent) => setQuery(changeEvent.target.value)}
            placeholder="Search transcript"
            type="search"
            value={query}
          />
        </label>
        <span className="transcript-count">
          {query.trim() === ""
            ? `${turns.length} ${turns.length === 1 ? "turn" : "turns"}`
            : `${matching.length} of ${turns.length} matching`}
        </span>
        <label className="transcript-role-filter">
          <span className="sr-only">Filter transcript by role</span>
          <select
            aria-label="Filter transcript by role"
            onChange={(event) => setRole(event.target.value as TranscriptTurn["role"] | "")}
            value={role}
          >
            <option value="">All roles</option>
            {(["system", "user", "assistant", "tool", "unknown"] as const).map((value) => (
              <option key={value} value={value}>
                {value}
              </option>
            ))}
          </select>
        </label>
        <button className="artifact-action" onClick={() => setRaw(true)} type="button">
          Show raw JSONL
        </button>
      </div>

      {/* The concluding message and the usage record are the two most-wanted
          points in a transcript and the hardest to reach by scrolling. */}
      {parsed.usage && (
        <p className="transcript-usage">
          <strong>Usage</strong> {usageSummary(parsed.usage)}
        </p>
      )}
      {parsed.malformedLines > 0 && (
        <p className="transcript-malformed" role="status">
          {parsed.malformedLines} line{parsed.malformedLines === 1 ? "" : "s"} could not be parsed and
          {parsed.malformedLines === 1 ? " is" : " are"} shown verbatim.
        </p>
      )}

      {matching.length === 0 ? (
        <p className="transcript-empty" role="status">
          No turns match “{query.trim()}”.
        </p>
      ) : (
        <ol className="transcript-turns">
          {visible.map((turn) => (
            <TranscriptTurnRow key={turn.index} turn={turn} />
          ))}
        </ol>
      )}

      {hidden > 0 && (
        <button className="artifact-action" onClick={() => setExpanded(true)} type="button">
          Show {hidden} more turn{hidden === 1 ? "" : "s"}
        </button>
      )}
      {(stage !== undefined || attempt !== undefined) && (
        <p className="transcript-scope">
          Stage {stage ?? "unavailable"} · Attempt {attempt ?? "unavailable"}
        </p>
      )}
    </div>
  );
}

function TranscriptTurnRow({ turn }: { turn: TranscriptTurn }) {
  // Tool results are collapsed by default: they are the bulkiest part of a
  // transcript and only sometimes the interesting part.
  const [showResult, setShowResult] = useState(false);
  const label = ROLE_LABELS[turn.role];

  return (
    <li className={`transcript-turn transcript-turn-${turn.role}`}>
      <div className="transcript-turn-heading">
        <span className="transcript-role">{label}</span>
        {turn.toolCall?.name && <span className="transcript-tool-name mono">{turn.toolCall.name}</span>}
        {turn.toolCall?.success === false && (
          <span className="transcript-tool-failed">failed</span>
        )}
        {turn.truncated && <span className="transcript-tool-failed">truncated</span>}
      </div>

      {turn.content && <p className="transcript-content">{turn.content}</p>}

      {turn.toolCall?.arguments && (
        <pre className="transcript-code code-block">{turn.toolCall.arguments}</pre>
      )}

      {turn.toolResult !== undefined && (
        <>
          <button
            aria-expanded={showResult}
            className="transcript-result-toggle"
            onClick={() => setShowResult((value) => !value)}
            type="button"
          >
            {showResult ? "Hide result" : "Show result"}
          </button>
          {showResult && <pre className="transcript-code code-block">{turn.toolResult}</pre>}
        </>
      )}
    </li>
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
        <span>{attemptLabel(attempt)}</span>
        {attempt.model && <span className="mono">model: {attempt.model}</span>}
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
              attemptVisit={attempt.visit}
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
  attemptVisit,
  client,
  runId,
}: {
  artifact: ArtifactMetadata;
  attemptNumber?: number;
  attemptVisit?: number;
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
            {attemptVisit !== undefined ? `Visit ${attemptVisit}` : "Visit unavailable"}
            {attemptNumber !== undefined ? ` · Attempt ${attemptNumber}` : ""}
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
