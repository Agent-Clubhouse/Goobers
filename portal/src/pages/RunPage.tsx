import { useEffect, useMemo, useState } from "react";
import { GraphFrame } from "../foundation/GraphFrame";
import { Icon } from "../foundation/Icon";
import { Inspector } from "../foundation/Inspector";
import { StatusBadge } from "../foundation/StatusBadge";
import { usePrefersReducedMotion } from "../foundation/useMediaQuery";
import {
  type NodeState,
  type Run,
  type RunEvent,
  type StageAttempt,
  type Workflow,
  type WorkflowStage,
  workflowForRun,
} from "../fixtures";
import {
  compressedIdleDelayMs,
  formatReplayDuration,
  idleCompressionThresholdMs,
  orderedReplayEvents,
  replaySpeeds,
  replayTransition,
  type ReplaySpeed,
} from "../replay";
import type { Route } from "../routes";

function deriveNodeStates(workflow: Workflow, events: RunEvent[], index: number): Record<string, NodeState> {
  const states = Object.fromEntries(workflow.stages.map((stage) => [stage.id, "pending" as NodeState]));
  let activeStageId: string | undefined;
  for (const event of events.slice(0, index + 1)) {
    if (!event.stageId) {
      continue;
    }
    if (event.type === "stage.started") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = "active";
      activeStageId = event.stageId;
    } else if (event.type === "stage.finished" || event.type === "artifact.recorded") {
      if (event.type === "stage.finished") {
        states[event.stageId] = event.tone === "danger" ? "failed" : "complete";
        if (activeStageId === event.stageId) {
          activeStageId = undefined;
        }
      }
    } else if (event.type === "gate.evaluated") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = event.tone === "danger" ? "escalated" : "complete";
      activeStageId = undefined;
    } else if (event.type === "run.finished") {
      if (activeStageId && activeStageId !== event.stageId && states[activeStageId] === "active") {
        states[activeStageId] = "complete";
      }
      states[event.stageId] = event.tone === "danger" ? "escalated" : "complete";
      activeStageId = undefined;
    }
  }
  return states;
}

function deriveTraversedEdges(events: RunEvent[], index: number): Set<string> {
  const traversed = new Set<string>();
  let currentStageId: string | undefined;
  for (const event of events.slice(0, index + 1)) {
    if (!event.stageId) {
      continue;
    }
    const entersStage =
      event.type === "stage.started" ||
      event.type === "stage.finished" ||
      event.type === "gate.evaluated" ||
      event.type === "run.finished";
    if (!entersStage) {
      continue;
    }
    if (currentStageId && currentStageId !== event.stageId) {
      traversed.add(`${currentStageId}->${event.stageId}`);
    }
    currentStageId = event.stageId;
  }
  return traversed;
}

function traversalEdgeAtEvent(events: RunEvent[], index: number): string | undefined {
  const current = events[index];
  if (!current?.stageId || !["stage.started", "gate.evaluated", "run.finished"].includes(current.type)) {
    return undefined;
  }
  for (let previousIndex = index - 1; previousIndex >= 0; previousIndex -= 1) {
    const previous = events[previousIndex];
    if (!previous.stageId || !["stage.started", "gate.evaluated", "run.finished"].includes(previous.type)) {
      continue;
    }
    return previous.stageId === current.stageId
      ? undefined
      : `${previous.stageId}->${current.stageId}`;
  }
  return undefined;
}

function visibleAttempts(run: Run, stageId: string, eventSeq: number): StageAttempt[] {
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

function AttemptInspector({
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
    <Inspector className="run-inspector" kind={stage.kind} title={stage.name}>
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
    </Inspector>
  );
}

type ReplayMode = "live-follow" | "replay";

export function RunPage({ run, navigate }: { run: Run; navigate: (route: Route) => void }) {
  const workflow = workflowForRun(run);
  const events = useMemo(() => orderedReplayEvents(run.events), [run.events]);
  const lastEventIndex = Math.max(events.length - 1, 0);
  const activeRun = run.status === "running";
  const [mode, setMode] = useState<ReplayMode>(activeRun ? "live-follow" : "replay");
  const [replayIndex, setReplayIndex] = useState(lastEventIndex);
  const [isPlaying, setIsPlaying] = useState(false);
  const [speed, setSpeed] = useState<ReplaySpeed>(1);
  const reducedMotion = usePrefersReducedMotion();
  const currentEvent = events[replayIndex] ?? events[0];
  const [selectedStageId, setSelectedStageId] = useState(
    currentEvent?.stageId ?? workflow.stages[0]?.id ?? "",
  );
  const selectedStage =
    workflow.stages.find((stage) => stage.id === selectedStageId) ?? workflow.stages[0];
  const nodeStates = deriveNodeStates(workflow, events, replayIndex);
  const traversedEdges = deriveTraversedEdges(events, replayIndex);
  const transition = replayTransition(events, replayIndex, speed);
  const atEnd = replayIndex >= lastEventIndex;
  const replayEnded = mode === "replay" && atEnd && !isPlaying;
  const liveHistoryInspection = mode === "live-follow" && replayIndex < lastEventIndex;
  const latestSequence = events[events.length - 1]?.seq;

  useEffect(() => {
    if (!isPlaying) {
      return;
    }
    if (atEnd || !transition) {
      setIsPlaying(false);
      return;
    }
    const timeout = window.setTimeout(() => {
      setReplayIndex((current) => Math.min(current + 1, lastEventIndex));
    }, transition.playbackDelayMs);
    return () => window.clearTimeout(timeout);
  }, [atEnd, isPlaying, lastEventIndex, replayIndex, transition?.playbackDelayMs]);

  useEffect(() => {
    if (activeRun && mode === "live-follow" && latestSequence !== undefined) {
      setReplayIndex(lastEventIndex);
    }
  }, [activeRun, lastEventIndex, latestSequence, mode]);

  useEffect(() => {
    setSelectedStageId(currentEvent?.stageId ?? workflow.stages[0]?.id ?? "");
  }, [currentEvent, workflow.stages]);

  const selectReplayIndex = (index: number, enterReplayMode: boolean) => {
    setIsPlaying(false);
    if (enterReplayMode) {
      setMode("replay");
    }
    setReplayIndex(Math.max(0, Math.min(index, lastEventIndex)));
  };

  const togglePlayback = () => {
    if (isPlaying) {
      setIsPlaying(false);
      return;
    }
    setMode("replay");
    if (atEnd) {
      setReplayIndex(0);
    }
    setIsPlaying(true);
  };

  const enterReplay = () => {
    setIsPlaying(false);
    setMode("replay");
    setReplayIndex(0);
  };

  const returnToLive = () => {
    setIsPlaying(false);
    setMode("live-follow");
    setReplayIndex(lastEventIndex);
  };

  const onReplayKeyDown = (event: React.KeyboardEvent<HTMLElement>) => {
    if (event.currentTarget !== event.target) {
      return;
    }
    if (event.key === "ArrowLeft") {
      event.preventDefault();
      selectReplayIndex(replayIndex - 1, true);
    } else if (event.key === "ArrowRight") {
      event.preventDefault();
      selectReplayIndex(replayIndex + 1, true);
    } else if (event.key === " ") {
      event.preventDefault();
      togglePlayback();
    }
  };

  return (
    <>
      <nav className="breadcrumbs" aria-label="Breadcrumb">
        <button onClick={() => navigate({ page: "runs" })} type="button">
          Runs
        </button>
        <Icon name="chevron" size={14} />
        <span>{run.shortId}</span>
      </nav>
      <header className="run-heading">
        <div>
          <div className="run-title-line">
            <StatusBadge status={run.status} />
            <span className="mono run-id">{run.shortId}</span>
          </div>
          <h1>{run.title}</h1>
          <p>
            {run.issue} · {workflow.name} v{workflow.version} · {run.startedAt}
          </p>
        </div>
        <dl className="run-meta">
          <div>
            <dt>Duration</dt>
            <dd>{run.duration}</dd>
          </div>
          <div>
            <dt>Repasses</dt>
            <dd>{run.repasses} / 3</dd>
          </div>
          <div>
            <dt>Trigger</dt>
            <dd>{run.trigger}</dd>
          </div>
          <div>
            <dt>Workflow pin</dt>
            <dd className="mono">v{workflow.version} · {workflow.digest.slice(7, 15)}</dd>
          </div>
        </dl>
      </header>

      {run.escalation && replayIndex >= run.events.length - 2 && (
        <section className="escalation-panel">
          <span className="escalation-icon">
            <Icon name="alert" />
          </span>
          <div>
            <span className="escalation-label">Escalation cause</span>
            <h2>{run.escalation.title}</h2>
            <p>{run.escalation.cause}</p>
            <dl>
              <div>
                <dt>Gate</dt>
                <dd>{run.escalation.gate}</dd>
              </div>
              <div>
                <dt>Branch</dt>
                <dd className="mono">{run.escalation.branch}</dd>
              </div>
              <div>
                <dt>Budget</dt>
                <dd>{run.escalation.attemptsUsed} / {run.escalation.attemptsAllowed} repasses</dd>
              </div>
            </dl>
          </div>
        </section>
      )}

      <section className="run-workspace" data-replay-motion={reducedMotion ? "reduced" : "animated"}>
        <div className="run-primary">
          <div className="graph-panel run-graph-panel">
            <div className="panel-heading-row">
              <div>
                <p className="section-kicker">Structure</p>
                <h2>Execution graph</h2>
              </div>
              <button
                className="workflow-link"
                onClick={() => navigate({ page: "workflow", id: workflow.id })}
                type="button"
              >
                View definition <Icon name="arrow" size={14} />
              </button>
            </div>
            <GraphFrame
              activeEdges={traversedEdges}
              onSelectStage={setSelectedStageId}
              selectedStageId={selectedStageId}
              states={nodeStates}
              traversingEdge={
                isPlaying && !reducedMotion
                  ? traversalEdgeAtEvent(events, replayIndex)
                  : undefined
              }
              workflow={workflow}
            />
          </div>

          <section
            aria-label="Replay controls"
            className="playback-panel"
            onKeyDown={onReplayKeyDown}
            tabIndex={0}
          >
            <div className="playback-summary">
              <div className="playback-now">
                <span aria-hidden="true" className={`event-mark event-${currentEvent?.tone ?? "neutral"}`} />
                <div>
                  <span>
                    Event {events.length === 0 ? 0 : replayIndex + 1} of {events.length}
                    {" · "}Seq {currentEvent?.seq ?? "—"}
                    {" · "}Real elapsed {currentEvent?.elapsed ?? "—"}
                  </span>
                  <strong>{currentEvent?.title ?? "No durable events"}</strong>
                </div>
              </div>
              <div className="replay-mode-control">
                <span className={`replay-mode replay-mode-${mode}`}>
                  {mode === "live-follow"
                    ? liveHistoryInspection
                      ? "Live follow · inspecting history"
                      : "Live follow"
                    : "Replay mode"}
                </span>
                {activeRun && mode === "live-follow" && (
                  <button className="mode-button" onClick={enterReplay} type="button">
                    Enter replay
                  </button>
                )}
                {activeRun && mode === "replay" && (
                  <button className="mode-button" onClick={returnToLive} type="button">
                    Return to live
                  </button>
                )}
              </div>
            </div>
            <div className="playback-controls">
              <button
                aria-label="Previous event"
                className="step-button"
                disabled={replayIndex === 0 || events.length === 0}
                onClick={() => selectReplayIndex(replayIndex - 1, true)}
                type="button"
              >
                <Icon name="previous" size={16} />
              </button>
              <button
                aria-label={isPlaying ? "Pause replay" : "Play replay"}
                className="play-button"
                disabled={events.length === 0}
                onClick={togglePlayback}
                type="button"
              >
                <Icon name={isPlaying ? "pause" : "play"} size={17} />
              </button>
              <button
                aria-label="Next event"
                className="step-button"
                disabled={atEnd || events.length === 0}
                onClick={() => selectReplayIndex(replayIndex + 1, true)}
                type="button"
              >
                <Icon name="next" size={16} />
              </button>
              <input
                aria-label="Replay position"
                disabled={events.length <= 1}
                max={lastEventIndex}
                min={0}
                onChange={(event) => {
                  selectReplayIndex(Number(event.target.value), true);
                }}
                type="range"
                value={replayIndex}
              />
              <div className="speed-control" aria-label="Replay speed">
                {replaySpeeds.map((value) => (
                  <button
                    aria-pressed={speed === value}
                    className={speed === value ? "speed-button speed-button-active" : "speed-button"}
                    key={value}
                    onClick={() => setSpeed(value)}
                    type="button"
                  >
                    {value}x
                  </button>
                ))}
              </div>
            </div>
            <div className="playback-context">
              <span className="replay-state" aria-live="polite">
                {mode === "live-follow"
                  ? liveHistoryInspection
                    ? `Inspecting event ${replayIndex + 1}; new durable events remain live-followed.`
                    : `Following the latest durable event, sequence ${currentEvent?.seq ?? "—"}.`
                  : isPlaying
                    ? `Playing event ${replayIndex + 1} of ${events.length}.`
                    : replayEnded
                      ? `Replay ended at event ${events.length}. Play restarts from event 1.`
                      : `Replay paused at event ${replayIndex + 1} of ${events.length}.`}
              </span>
              <span>
                Idle compression: waits over {formatReplayDuration(idleCompressionThresholdMs)} play in at most{" "}
                {formatReplayDuration(compressedIdleDelayMs)} before speed; real elapsed is unchanged.
              </span>
              {transition?.idleCompressed && (
                <strong className="compressed-wait">
                  Next wait: {formatReplayDuration(transition.realDelayMs)} compressed to{" "}
                  {formatReplayDuration(transition.playbackDelayMs)} at {speed}x.
                </strong>
              )}
              <span>
                {reducedMotion
                  ? "Reduced motion: state changes are instant without graph traversal animation."
                  : "Graph traversal animates only after the durable event is selected."}
              </span>
              <span className="keyboard-hint">Keyboard: Space play/pause · Left/Right previous/next</span>
            </div>
          </section>

          <div className="event-ledger">
            <div className="panel-heading-row">
              <div>
                <p className="section-kicker">Journal</p>
                <h2>Event ledger</h2>
              </div>
              <span className="graph-legend">Ordered by durable sequence</span>
            </div>
            <ol>
              {events.map((event, index) => (
                <li className={index === replayIndex ? "ledger-item ledger-item-active" : "ledger-item"} key={event.id}>
                  <button
                    aria-current={index === replayIndex ? "true" : undefined}
                    aria-label={`Select event ${event.seq}: ${event.title}`}
                    onClick={() => {
                      selectReplayIndex(index, !activeRun || mode === "replay");
                    }}
                    type="button"
                  >
                    <span className="ledger-seq mono">{String(event.seq).padStart(2, "0")}</span>
                    <span aria-hidden="true" className={`event-mark event-${event.tone}`} />
                    <span className="ledger-copy">
                      <strong>{event.title}</strong>
                      <span>{event.detail}</span>
                    </span>
                    <span className="ledger-type mono">{event.type}</span>
                    <span className="ledger-time mono">{event.elapsed}</span>
                  </button>
                </li>
              ))}
            </ol>
          </div>
        </div>

        {selectedStage && <AttemptInspector eventSeq={currentEvent?.seq ?? 0} run={run} stage={selectedStage} />}
      </section>
    </>
  );
}
