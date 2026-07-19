import { useEffect, useRef, useState } from "react";
import type {
  DaemonClient,
  RunDetail,
  RunEvent,
  WorkflowGraph,
  WorkflowGraphNode,
} from "../api/types";
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
      events={query.state.data.events}
      key={query.state.data.run.id}
      navigate={navigate}
      run={query.state.data.run}
    />
  );
}

function RunDetailWorkspace({
  events,
  navigate,
  run,
}: {
  events: RunEvent[];
  navigate: Navigate;
  run: RunDetail;
}) {
  const latestEvent = events.at(-1);
  const initialSeq = latestEvent?.seq ?? 0;
  const [selectedSeq, setSelectedSeq] = useState(initialSeq);
  const [selectedNodeId, setSelectedNodeId] = useState<string | undefined>(
    eventNodeAtSequence(events, initialSeq) ?? run.currentStage,
  );
  const [followingLatest, setFollowingLatest] = useState(true);
  const nodeStates = run.graph
    ? deriveNodeStates(run.graph, events, selectedSeq)
    : {};

  useEffect(() => {
    if (!followingLatest) {
      return;
    }
    setSelectedSeq(initialSeq);
    setSelectedNodeId(eventNodeAtSequence(events, initialSeq) ?? run.currentStage);
  }, [events, followingLatest, initialSeq, run.currentStage]);

  const selectEvent = (event: RunEvent) => {
    setSelectedSeq(event.seq);
    setSelectedNodeId(eventNodeAtSequence(events, event.seq));
    setFollowingLatest(event.seq === initialSeq);
  };

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

      <section
        className="run-detail-workspace"
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
            <RunTopologyGraph
              graph={run.graph}
              nodeStates={nodeStates}
              onSelectNode={setSelectedNodeId}
              selectedNodeId={selectedNodeId}
              selectedSeq={selectedSeq}
            />
          ) : (
            <div className="empty-detail" role="status">
              <strong>Pinned graph unavailable</strong>
              <span>This historic run predates graph snapshots. Its event ledger remains available.</span>
            </div>
          )}
        </GraphFrame>

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

function RunTopologyGraph({
  graph,
  nodeStates,
  onSelectNode,
  selectedNodeId,
  selectedSeq,
}: {
  graph: WorkflowGraph;
  nodeStates: Record<string, RunNodeState>;
  onSelectNode: (nodeId: string) => void;
  selectedNodeId?: string;
  selectedSeq: number;
}) {
  const nodeRefs = useRef<Array<HTMLButtonElement | null>>([]);

  const moveSelection = (targetIndex: number) => {
    const node = graph.nodes[targetIndex];
    if (!node) {
      return;
    }
    onSelectNode(node.id);
    nodeRefs.current[targetIndex]?.focus();
  };

  const onNodeKeyDown = (
    event: React.KeyboardEvent<HTMLButtonElement>,
    node: WorkflowGraphNode,
    index: number,
  ) => {
    let targetIndex: number | undefined;
    if (event.key === "ArrowRight" || event.key === "ArrowDown") {
      targetIndex = (index + 1) % graph.nodes.length;
    } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
      targetIndex = (index - 1 + graph.nodes.length) % graph.nodes.length;
    } else if (event.key === "Home") {
      targetIndex = 0;
    } else if (event.key === "End") {
      targetIndex = graph.nodes.length - 1;
    }
    if (targetIndex !== undefined) {
      event.preventDefault();
      moveSelection(targetIndex);
    } else if (event.key === "Enter" || event.key === " ") {
      onSelectNode(node.id);
    }
  };

  return (
    <div
      aria-label={`${graph.name} pinned execution graph`}
      className="run-topology"
      data-responsive-layout="compact-under-820"
      role="group"
    >
      <p className="run-graph-pin">
        Pinned v{graph.version} · <span className="mono">{graph.digest}</span>
      </p>
      <ol className="run-topology-list">
        {graph.nodes.map((node, index) => {
          const state = nodeStates[node.id] ?? "pending";
          const outgoing = graph.edges.filter((edge) => edge.source === node.id);
          return (
            <li className="run-topology-step" key={node.id}>
              <button
                aria-label={`${node.id}, ${node.kind}, ${nodeStateLabel(state)} at sequence ${selectedSeq}`}
                aria-pressed={selectedNodeId === node.id}
                className={`run-topology-node run-node-${node.kind} run-node-state-${state}`}
                onClick={() => onSelectNode(node.id)}
                onKeyDown={(event) => onNodeKeyDown(event, node, index)}
                ref={(element) => {
                  nodeRefs.current[index] = element;
                }}
                type="button"
              >
                <span className="graph-node-kind">{node.kind}</span>
                <strong>{node.id}</strong>
                <span className="run-node-state">{nodeStateLabel(state)}</span>
                {(node.owner || node.evaluator) && (
                  <span className="run-node-owner">{node.owner || `${node.evaluator} evaluator`}</span>
                )}
              </button>
              {outgoing.length > 0 && (
                <ul aria-label={`Outgoing branches from ${node.id}`} className="run-graph-edges">
                  {outgoing.map((edge, edgeIndex) => (
                    <li key={`${edge.source}-${edge.target}-${edge.outcome ?? edgeIndex}`}>
                      <span>{edge.outcome || "next"}</span>
                      <span aria-hidden="true">→</span>
                      <strong>{edge.target || edge.terminal}</strong>
                    </li>
                  ))}
                </ul>
              )}
            </li>
          );
        })}
      </ol>
    </div>
  );
}

function EventLedger({
  events,
  onSelect,
  run,
  selectedSeq,
}: {
  events: RunEvent[];
  onSelect: (event: RunEvent) => void;
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
                  onClick={() => onSelect(event)}
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

function nodeStateLabel(state: RunNodeState): string {
  return state.charAt(0).toUpperCase() + state.slice(1);
}
