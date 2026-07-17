import { useEffect, useMemo, useState } from "react";
import { AttemptInspector } from "../components/AttemptInspector";
import { EscalationPanel } from "../components/EscalationPanel";
import { usePrefersReducedMotion } from "../hooks/usePrefersReducedMotion";
import { workflowForRun, type Run } from "../prototypeData";
import {
  compressedIdleDelayMs,
  formatReplayDuration,
  idleCompressionThresholdMs,
  orderedReplayEvents,
  replaySpeeds,
  replayTransition,
  type ReplaySpeed,
} from "../replay";
import type { Navigate } from "../routing";
import {
  causalEventIndex,
  deriveNodeStates,
  deriveTraversedEdges,
  traversalEdgeAtEvent,
} from "../runState";
import { GraphFrame, TopologyGraph } from "../ui/GraphFrame";
import { Icon } from "../ui/Icon";
import { StatusBadge } from "../ui/StatusBadge";

type ReplayMode = "live-follow" | "replay";

export function RunPage({ run, navigate }: { run: Run; navigate: Navigate }) {
  const workflow = workflowForRun(run);
  const events = useMemo(() => orderedReplayEvents(run.events), [run.events]);
  const lastEventIndex = Math.max(events.length - 1, 0);
  const escalationEventIndex = causalEventIndex(run, events);
  const initialReplayIndex =
    run.status === "escalated" && escalationEventIndex !== undefined
      ? escalationEventIndex
      : lastEventIndex;
  const activeRun = run.status === "running";
  const [mode, setMode] = useState<ReplayMode>(activeRun ? "live-follow" : "replay");
  const [replayIndex, setReplayIndex] = useState(initialReplayIndex);
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
  const causalEvent =
    escalationEventIndex === undefined ? undefined : events[escalationEventIndex];
  const escalationVisible =
    run.status === "escalated" &&
    (run.escalation && causalEvent
      ? (currentEvent?.seq ?? 0) >= causalEvent.seq
      : replayIndex === lastEventIndex);

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

  const selectReplayIndex = (index: number, enterReplay: boolean) => {
    setIsPlaying(false);
    if (enterReplay) {
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

      {escalationVisible && (
        <EscalationPanel
          causalEvent={causalEvent}
          evidenceEventSeq={run.escalation ? causalEvent?.seq : currentEvent?.seq}
          onFocusCausalEvent={
            escalationEventIndex === undefined
              ? undefined
              : () => selectReplayIndex(escalationEventIndex, true)
          }
          onSelectStage={setSelectedStageId}
          run={run}
          workflow={workflow}
        />
      )}

      <section className="run-workspace" data-replay-motion={reducedMotion ? "reduced" : "animated"}>
        <div className="run-primary">
          <GraphFrame
            action={
              <button
                className="workflow-link"
                onClick={() => navigate({ page: "workflow", id: workflow.id })}
                type="button"
              >
                View definition <Icon name="arrow" size={14} />
              </button>
            }
            className="run-graph-panel"
          >
            <TopologyGraph
              activeEdges={traversedEdges}
              causalStageId={escalationVisible ? causalEvent?.stageId : undefined}
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
          </GraphFrame>

          <section
            aria-label="Replay controls"
            className="playback-panel"
            onKeyDown={onReplayKeyDown}
            tabIndex={0}
          >
            <div className="playback-summary">
              <div className="playback-now">
                <span
                  aria-hidden="true"
                  className={`event-mark event-${currentEvent?.tone ?? "neutral"}`}
                />
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
              <div className="speed-control" aria-label="Replay speed" role="group">
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
                <li
                  className={[
                    "ledger-item",
                    index === replayIndex ? "ledger-item-active" : "",
                    escalationVisible && event.seq === run.escalation?.causalEventSeq
                      ? "ledger-item-causal"
                      : "",
                  ]
                    .filter(Boolean)
                    .join(" ")}
                  key={event.id}
                >
                  <button
                    aria-current={index === replayIndex ? "true" : undefined}
                    aria-label={`Select event ${event.seq}: ${event.title}${
                      escalationVisible && event.seq === run.escalation?.causalEventSeq
                        ? " (causal event)"
                        : ""
                    }`}
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
                      {escalationVisible && event.seq === run.escalation?.causalEventSeq && (
                        <span className="causal-event-label">Causal event</span>
                      )}
                    </span>
                    <span className="ledger-type mono">{event.type}</span>
                    <span className="ledger-time mono">{event.elapsed}</span>
                  </button>
                </li>
              ))}
            </ol>
          </div>
        </div>

        {selectedStage && (
          <AttemptInspector eventSeq={currentEvent?.seq ?? 0} run={run} stage={selectedStage} />
        )}
      </section>
    </>
  );
}
