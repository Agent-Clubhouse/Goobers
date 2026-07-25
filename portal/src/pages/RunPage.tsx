import { useEffect, useRef, useState } from "react";
import type { DaemonClient, RunDetail, RunEvent } from "../api/types";
import { EscalationPanel } from "../components/EscalationPanel";
import { ReplayScrubber } from "../components/ReplayScrubber";
import { RunStageInspector } from "../components/RunStageInspector";
import { WorkflowTopologyGraph } from "../components/WorkflowTopologyGraph";
import {
  deriveNodeStates,
  eventHeading,
  eventNodeAtSequence,
  eventSummary,
  formatDuration,
  formatElapsed,
  formatTimestamp,
  type RunNodeState,
  useRunDetail,
} from "../runDetailData";
import type { Navigate } from "../routing";
import { GraphFrame } from "../ui/GraphFrame";
import { Icon } from "../ui/Icon";
import { StatusBadge } from "../ui/StatusBadge";

export function RunPage({
  client,
  navigate,
  runId,
  standalone,
}: {
  client: DaemonClient;
  navigate: Navigate;
  runId: string;
  standalone: boolean;
}) {
  const query = useRunDetail(client, runId);

  if (query.state.status === "loading") {
    return (
      <section aria-live="polite" className="daemon-state" role="status">
        <span aria-hidden="true" className="loading-mark" />
        <div>
          <h1>Loading run</h1>
          <p>
            {standalone
              ? "Reading pinned identity, graph, and durable events from local instance files."
              : "Reading pinned identity, graph, and durable events from the daemon."}
          </p>
        </div>
      </section>
    );
  }
  if (query.state.status === "error") {
    return (
      <section className="daemon-state daemon-state-error" role="alert">
        <div>
          <h1>Run unavailable</h1>
          <p>{query.state.error.message}</p>
        </div>
        <button className="reconnect-button" onClick={query.retry} type="button">
          Retry
        </button>
      </section>
    );
  }
  if (query.state.status !== "ready") {
    return null;
  }

  return (
    <RunDetailWorkspace
      client={client}
      events={query.state.data.events}
      key={query.state.data.run.id}
      navigate={navigate}
      run={query.state.data.run}
      runId={runId}
    />
  );
}

function RunDetailWorkspace({
  client,
  events,
  navigate,
  run,
  runId,
}: {
  client: DaemonClient;
  events: RunEvent[];
  navigate: Navigate;
  run: RunDetail;
  runId: string;
}) {
  const latestEvent = events.at(-1);
  const initialSeq = latestEvent?.seq ?? 0;
  const [selectedSeq, setSelectedSeq] = useState(initialSeq);
  const [selectedNodeId, setSelectedNodeId] = useState<string | undefined>(
    eventNodeAtSequence(events, initialSeq) ?? run.currentStage,
  );
  const [followingLatest, setFollowingLatest] = useState(true);
  const inspectorRef = useRef<HTMLElement>(null);
  const nodeStates = run.graph
    ? deriveNodeStates(run.graph, events, selectedSeq)
    : {};
  const selectedNode = run.graph?.nodes.find((node) => node.id === selectedNodeId);

  const revealInspector = () => {
    const inspector = inspectorRef.current;
    if (!inspector) {
      return;
    }
    inspector.scrollIntoView?.({ block: "start", inline: "nearest" });
    inspector.focus({ preventScroll: true });
  };

  useEffect(() => {
    if (!followingLatest) {
      return;
    }
    setSelectedSeq(initialSeq);
    setSelectedNodeId(eventNodeAtSequence(events, initialSeq) ?? run.currentStage);
  }, [events, followingLatest, initialSeq, run.currentStage]);

  const selectNode = (nodeId: string, shouldRevealInspector = false) => {
    setSelectedNodeId(nodeId);
    if (shouldRevealInspector) {
      revealInspector();
    }
  };

  const selectEvent = (event: RunEvent, shouldRevealInspector = false) => {
    setSelectedSeq(event.seq);
    setSelectedNodeId(eventNodeAtSequence(events, event.seq));
    setFollowingLatest(event.seq === initialSeq);
    if (shouldRevealInspector) {
      revealInspector();
    }
  };

  const replaySeek = (seq: number) => {
    setSelectedSeq(seq);
    setSelectedNodeId(eventNodeAtSequence(events, seq));
    setFollowingLatest(seq === initialSeq);
  };

  const causalEventSeq = run.escalation?.causalEventSeq;
  const causalEvent =
    causalEventSeq === undefined ? undefined : events.find((event) => event.seq === causalEventSeq);
  const causalNodeId =
    causalEventSeq === undefined ? undefined : eventNodeAtSequence(events, causalEventSeq);
  const focusCausalEvent =
    causalEventSeq === undefined ? undefined : () => replaySeek(causalEventSeq);

  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumbs">
        <button onClick={() => navigate({ page: "runs" })} type="button">
          Runs
        </button>
        <Icon name="chevron" size={14} />
        <span className="mono">{run.id}</span>
      </nav>

      <header className="run-heading">
        <div>
          <div className="run-title-line">
            <StatusBadge status={run.phase} />
            <span className="mono run-id">{run.id}</span>
          </div>
          <h1>Run {run.id}</h1>
          <p>
            {run.gaggle} / {run.workflow} · Workflow version {run.workflowVersion}
          </p>
        </div>
        <dl className="run-meta">
          <div>
            <dt>Trigger</dt>
            <dd>
              {run.trigger.kind}
              {run.trigger.ref ? ` · ${run.trigger.ref}` : ""}
            </dd>
          </div>
          <div>
            <dt>Started</dt>
            <dd>
              <time dateTime={run.startedAt}>{formatTimestamp(run.startedAt)}</time>
            </dd>
          </div>
          <div>
            <dt>Finished</dt>
            <dd>
              {run.finishedAt ? (
                <time dateTime={run.finishedAt}>{formatTimestamp(run.finishedAt)}</time>
              ) : (
                "In progress"
              )}
            </dd>
          </div>
          <div>
            <dt>Duration</dt>
            <dd>{formatDuration(run.durationMillis)}</dd>
          </div>
          <div>
            <dt>Workflow pin</dt>
            <dd className="mono run-pin">
              v{run.graph?.version ?? run.workflowVersion} ·{" "}
              {run.graph?.digest ?? run.workflowDigest ?? "Unavailable"}
            </dd>
          </div>
        </dl>
      </header>

      {run.escalation && (
        <EscalationPanel
          causalEvent={causalEvent}
          escalation={run.escalation}
          onFocusCausalEvent={focusCausalEvent}
        />
      )}

      <section
        className="run-detail-workspace"
        data-scroll-owner="page"
        data-responsive-layout="stack-under-820"
      >
        <GraphFrame
          action={
            <span aria-live="polite" className="graph-legend">
              State at sequence {selectedSeq || "—"}
            </span>
          }
          className="run-graph-panel"
        >
          {run.graphStatus === "pinned" && run.graph ? (
            <WorkflowTopologyGraph
              causalNodeId={causalNodeId}
              graph={run.graph}
              nodeStates={nodeStates}
              onSelectStage={selectNode}
              selectedStageId={selectedNodeId}
              stateSeq={selectedSeq}
            />
          ) : (
            <div className="empty-detail" role="status">
              <strong>Pinned graph unavailable</strong>
              <span>This historic run predates graph snapshots. Its event ledger remains available.</span>
            </div>
          )}
        </GraphFrame>

        {events.length > 0 && (
          <ReplayScrubber
            events={events}
            onSeek={replaySeek}
            selectedSeq={selectedSeq}
            terminal={run.finishedAt != null}
          />
        )}

        {run.graphStatus === "pinned" && run.graph && (
          <RunStageInspector
            client={client}
            inspectorRef={inspectorRef}
            node={selectedNode}
            runId={runId}
            selectedSeq={selectedSeq}
          />
        )}

        <EventLedger
          events={events}
          onSelect={selectEvent}
          run={run}
          selectedSeq={selectedSeq}
        />
      </section>
    </>
  );
}

function EventLedger({
  events,
  onSelect,
  run,
  selectedSeq,
}: {
  events: RunEvent[];
  onSelect: (event: RunEvent, revealInspector?: boolean) => void;
  run: RunDetail;
  selectedSeq: number;
}) {
  const eventRefs = useRef<Array<HTMLButtonElement | null>>([]);

  const moveSelection = (index: number, targetIndex: number) => {
    const event = events[targetIndex];
    if (!event) {
      return;
    }
    onSelect(event);
    eventRefs.current[targetIndex]?.focus();
  };

  return (
    <section aria-labelledby="event-ledger-title" className="event-ledger">
      <div className="panel-heading-row">
        <div>
          <p className="section-kicker">Journal</p>
          <h2 id="event-ledger-title">Event ledger</h2>
        </div>
        <span className="graph-legend">Ordered by durable sequence</span>
      </div>
      {events.length === 0 ? (
        <div className="empty-detail" role="status">
          <strong>No durable events recorded</strong>
        </div>
      ) : (
        <ol>
          {events.map((event, index) => {
            const selected = event.seq === selectedSeq;
            const heading = eventHeading(event);
            const summary = eventSummary(event);
            return (
              <li className={`ledger-item ${selected ? "ledger-item-active" : ""}`} key={`${event.branch}-${event.seq}`}>
                <button
                  aria-current={selected ? "true" : undefined}
                  aria-label={`Select sequence ${event.seq}: ${heading}. ${summary}`}
                  className="run-ledger-button"
                  onClick={() => onSelect(event, true)}
                  onKeyDown={(keyboardEvent) => {
                    let targetIndex: number | undefined;
                    if (keyboardEvent.key === "ArrowDown" || keyboardEvent.key === "ArrowRight") {
                      targetIndex = Math.min(index + 1, events.length - 1);
                    } else if (keyboardEvent.key === "ArrowUp" || keyboardEvent.key === "ArrowLeft") {
                      targetIndex = Math.max(index - 1, 0);
                    } else if (keyboardEvent.key === "Home") {
                      targetIndex = 0;
                    } else if (keyboardEvent.key === "End") {
                      targetIndex = events.length - 1;
                    }
                    if (targetIndex !== undefined) {
                      keyboardEvent.preventDefault();
                      moveSelection(index, targetIndex);
                    }
                  }}
                  ref={(element) => {
                    eventRefs.current[index] = element;
                  }}
                  type="button"
                >
                  <span className="ledger-seq mono">Seq {event.seq}</span>
                  <span className="ledger-type mono">Type {event.type}</span>
                  <span className="ledger-time mono">
                    Elapsed {formatElapsed(run.startedAt, event.time)}
                  </span>
                  <span className="ledger-attempt">
                    {event.attempt ? `Attempt ${event.attempt}` : "No attempt"}
                  </span>
                  <span className="ledger-copy">
                    <strong>{heading}</strong>
                    <span>{summary}</span>
                    {!event.knownSchema && (
                      <span className="ledger-unknown">Unsupported schema {event.schema}</span>
                    )}
                    {selected && <span className="selected-event-label">Selected event</span>}
                  </span>
                </button>
              </li>
            );
          })}
        </ol>
      )}
    </section>
  );
}
