import { useEffect, useMemo, useState } from "react";
import { DataList, DataRow } from "./components/DataList";
import { GraphFrame, TopologyGraph } from "./components/GraphFrame";
import { Icon } from "./components/Icon";
import { AttemptInspector, StageDefinitionInspector } from "./components/Inspector";
import { PortalShell } from "./components/PortalShell";
import { StatusBadge } from "./components/Status";
import { usePrefersReducedMotion } from "./hooks/usePrefersReducedMotion";
import {
  instanceWarnings,
  runs,
  workflowForRun,
  workflows,
  type NodeState,
  type Run,
  type RunEvent,
  type Workflow,
} from "./prototypeData";
import {
  compressedIdleDelayMs,
  formatReplayDuration,
  idleCompressionThresholdMs,
  orderedReplayEvents,
  replaySpeeds,
  replayTransition,
  type ReplaySpeed,
} from "./replay";
import { activeArea, parseRoute, routeHash, type Route } from "./routes";
import { ThemeProvider } from "./theme";

function OverviewPage({ navigate }: { navigate: (route: Route) => void }) {
  const [warningVisible, setWarningVisible] = useState(true);
  const activeRuns = runs.filter((run) => run.status === "running");
  const recentRuns = runs.filter((run) => run.status !== "running");
  const attentionRun = runs.find((run) => run.status === "escalated");

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Local instance</p>
        <h1>One run needs attention.</h1>
        <p>Everything else is moving normally across the goobers gaggle.</p>
      </header>

      <section aria-label="Instance status" className="instance-strip">
        <div>
          <span className="live-mark" />
          <strong>Daemon connected</strong>
          <span>updated just now</span>
        </div>
        <dl>
          <div>
            <dt>Workflows</dt>
            <dd>{workflows.length}</dd>
          </div>
          <div>
            <dt>Active runs</dt>
            <dd>{activeRuns.length}</dd>
          </div>
          <div>
            <dt>Gaggles</dt>
            <dd>1</dd>
          </div>
        </dl>
      </section>

      {attentionRun && (
        <section className="content-section">
          <div className="section-heading">
            <div>
              <p className="section-kicker section-kicker-danger">Attention</p>
              <h2>Needs a decision</h2>
            </div>
            <span className="section-count">1 run</span>
          </div>
          <button
            className="attention-row"
            onClick={() => navigate({ page: "run", id: attentionRun.id })}
            type="button"
          >
            <span className="attention-icon">
              <Icon name="alert" />
            </span>
            <span className="attention-copy">
              <strong>{attentionRun.title}</strong>
              <span>{attentionRun.escalation?.title}</span>
            </span>
            <span className="attention-meta">
              <span>{attentionRun.issue}</span>
              <span>{attentionRun.repasses} of 3 repasses</span>
            </span>
            <Icon name="arrow" />
          </button>
        </section>
      )}

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">Live</p>
            <h2>Active runs</h2>
          </div>
          <button className="text-button" onClick={() => navigate({ page: "runs" })} type="button">
            View all runs <Icon name="arrow" size={15} />
          </button>
        </div>
        <DataList headers={["Run", "Workflow", "Current stage", "Elapsed", ""]} layout="run-grid">
          {activeRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              layout="run-grid"
              onClick={() => navigate({ page: "run", id: run.id })}
            >
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">
                  {run.issue} · {run.shortId}
                </span>
              </span>
              <span>{workflowForRun(run).name}</span>
              <span className="stage-progress">
                <span className="stage-progress-mark" />
                {run.currentStage}
              </span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </DataList>
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent outcomes</h2>
          </div>
        </div>
        <DataList headers={["Run", "Outcome", "Workflow", "Duration", ""]} layout="outcome-grid">
          {recentRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              layout="outcome-grid"
              onClick={() => navigate({ page: "run", id: run.id })}
            >
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">{run.issue}</span>
              </span>
              <StatusBadge status={run.status} />
              <span>{workflowForRun(run).name}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </DataList>
      </section>

      {warningVisible && instanceWarnings.map((warning) => (
        <section className="warning-strip" key={warning.code}>
          <span className="warning-code">{warning.code}</span>
          <span>
            <strong>{warning.title}</strong>
            <small>{warning.detail}</small>
          </span>
          <button
            className="icon-button"
            aria-label="Dismiss warning preview"
            onClick={() => setWarningVisible(false)}
            type="button"
          >
            <Icon name="close" size={16} />
          </button>
        </section>
      ))}
    </>
  );
}

function WorkflowsPage({ navigate }: { navigate: (route: Route) => void }) {
  return (
    <>
      <header className="page-heading page-heading-row">
        <div>
          <p className="page-kicker">Definitions</p>
          <h1>Workflows</h1>
          <p>Versioned processes currently provisioned in the local instance.</p>
        </div>
        <div className="scope-chip">
          <span className="scope-mark">G</span>
          goobers gaggle
        </div>
      </header>

      <section className="content-section">
        <DataList headers={["Workflow", "Trigger", "Concurrency", "Last outcome", ""]} layout="workflow-grid">
          {workflows.map((workflow) => (
            <DataRow
              key={workflow.id}
              label={`Open workflow ${workflow.name}`}
              layout="workflow-grid"
              onClick={() => navigate({ page: "workflow", id: workflow.id })}
            >
              <span className="row-primary">
                <span className="row-title">{workflow.name}</span>
                <span className="row-subtitle">{workflow.description}</span>
              </span>
              <span>{workflow.trigger}</span>
              <span>
                {workflow.activeRuns} active / {workflow.maxConcurrency} max
              </span>
              <span className="outcome-cell">
                <StatusBadge status={workflow.lastOutcome} />
                <small>{workflow.lastRunAt}</small>
              </span>
            </DataRow>
          ))}
        </DataList>
      </section>
    </>
  );
}

function RunsPage({ navigate }: { navigate: (route: Route) => void }) {
  const [filter, setFilter] = useState<"all" | "active" | "attention" | "complete">("all");
  const filteredRuns = runs.filter((run) => {
    if (filter === "active") {
      return run.status === "running";
    }
    if (filter === "attention") {
      return run.status === "escalated" || run.status === "failed";
    }
    if (filter === "complete") {
      return run.status === "completed";
    }
    return true;
  });

  return (
    <>
      <header className="page-heading">
        <p className="page-kicker">Journal</p>
        <h1>Runs</h1>
        <p>Every execution, ordered by the most recent journal activity.</p>
      </header>

      <div className="filter-bar" aria-label="Filter runs" role="group">
        {(["all", "active", "attention", "complete"] as const).map((option) => (
          <button
            aria-pressed={filter === option}
            className={filter === option ? "filter-button filter-button-active" : "filter-button"}
            key={option}
            onClick={() => setFilter(option)}
            type="button"
          >
            {option === "all" ? "All runs" : option}
          </button>
        ))}
      </div>

      <section className="content-section">
        <DataList
          headers={["Run", "Status", "Current stage", "Started", "Duration", ""]}
          layout="all-runs-grid"
        >
          {filteredRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              layout="all-runs-grid"
              onClick={() => navigate({ page: "run", id: run.id })}
            >
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">
                  {run.issue} · {workflowForRun(run).name}
                </span>
              </span>
              <StatusBadge status={run.status} />
              <span>{run.currentStage}</span>
              <span>{run.startedAt}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </DataList>
      </section>
    </>
  );
}

function WorkflowPage({ workflow, navigate }: { workflow: Workflow; navigate: (route: Route) => void }) {
  const [selectedStageId, setSelectedStageId] = useState(workflow.stages[0]?.id ?? "");
  const selectedStage = workflow.stages.find((stage) => stage.id === selectedStageId) ?? workflow.stages[0];
  const workflowRuns = runs.filter((run) => run.workflowId === workflow.id);

  return (
    <>
      <nav className="breadcrumbs" aria-label="Breadcrumb">
        <button onClick={() => navigate({ page: "workflows" })} type="button">
          Workflows
        </button>
        <Icon name="chevron" size={14} />
        <span>{workflow.name}</span>
      </nav>
      <header className="detail-heading">
        <div>
          <span className="definition-label">Workflow definition</span>
          <h1>{workflow.name}</h1>
          <p>{workflow.description}</p>
        </div>
        <dl className="detail-meta">
          <div>
            <dt>Trigger</dt>
            <dd>{workflow.trigger}</dd>
          </div>
          <div>
            <dt>Concurrency</dt>
            <dd>{workflow.activeRuns} / {workflow.maxConcurrency}</dd>
          </div>
          <div>
            <dt>Gaggle</dt>
            <dd>{workflow.gaggle}</dd>
          </div>
        </dl>
      </header>

      <section className="graph-layout">
        <GraphFrame hint="Select a stage to inspect it">
          <TopologyGraph
            onSelectStage={setSelectedStageId}
            selectedStageId={selectedStageId}
            workflow={workflow}
          />
        </GraphFrame>
        {selectedStage && <StageDefinitionInspector stage={selectedStage} />}
      </section>

      <section className="content-section">
        <div className="section-heading">
          <div>
            <p className="section-kicker">History</p>
            <h2>Recent runs</h2>
          </div>
        </div>
        <DataList layout="history-grid">
          {workflowRuns.map((run) => (
            <DataRow
              key={run.id}
              label={`Open run ${run.title}`}
              layout="history-grid"
              onClick={() => navigate({ page: "run", id: run.id })}
            >
              <span className="row-primary">
                <span className="row-title">{run.title}</span>
                <span className="row-subtitle">{run.issue} · {run.startedAt}</span>
              </span>
              <StatusBadge status={run.status} />
              <span>{run.currentStage}</span>
              <span className="mono">{run.duration}</span>
            </DataRow>
          ))}
        </DataList>
      </section>
    </>
  );
}

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

type ReplayMode = "live-follow" | "replay";

function RunPage({ run, navigate }: { run: Run; navigate: (route: Route) => void }) {
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
                <span className={`event-mark event-${currentEvent?.tone ?? "neutral"}`} />
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
                    <span className={`event-mark event-${event.tone}`} />
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

function PortalApplication() {
  const [route, setRoute] = useState<Route>(() => parseRoute());

  useEffect(() => {
    const onHashChange = () => setRoute(parseRoute());
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  const navigate = (nextRoute: Route) => {
    const nextHash = routeHash(nextRoute);
    if (window.location.hash === nextHash) {
      setRoute(nextRoute);
    } else {
      window.location.hash = nextHash;
    }
  };

  const area = activeArea(route);
  const run = route.page === "run" ? runs.find((candidate) => candidate.id === route.id) : undefined;
  const workflow =
    route.page === "workflow" ? workflows.find((candidate) => candidate.id === route.id) : undefined;

  return (
    <PortalShell
      activeArea={area}
      navigate={navigate}
      runCount={runs.length}
      workflowCount={workflows.length}
    >
      {route.page === "overview" && <OverviewPage navigate={navigate} />}
      {route.page === "workflows" && <WorkflowsPage navigate={navigate} />}
      {route.page === "runs" && <RunsPage navigate={navigate} />}
      {route.page === "workflow" && workflow && <WorkflowPage navigate={navigate} workflow={workflow} />}
      {route.page === "run" && run && <RunPage key={run.id} navigate={navigate} run={run} />}
      {route.page === "workflow" && !workflow && <p>Workflow not found.</p>}
      {route.page === "run" && !run && <p>Run not found.</p>}
    </PortalShell>
  );
}

export function App() {
  return (
    <ThemeProvider>
      <PortalApplication />
    </ThemeProvider>
  );
}
