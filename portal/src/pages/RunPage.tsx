import { useEffect, useRef, useState } from "react";
import type { DaemonClient, RunDetail, RunEvent } from "../api/types";
import { EscalationPanel } from "../components/EscalationPanel";
import { ReplayScrubber } from "../components/ReplayScrubber";
import { RunStageInspector } from "../components/RunStageInspector";
import {
  WorkflowTopologyGraph,
  type WorkflowGraphFullscreenMode,
} from "../components/WorkflowTopologyGraph";
import {
  deriveNodeStates,
  evidenceVisit,
  evidenceDecision,
  eventHeading,
  eventNodeAtSequence,
  eventSummary,
  formatDuration,
  formatElapsed,
  formatTimestamp,
  isMajorJournalEvent,
  isInspectableEvidenceEvent,
  journalEntries,
  orderRunEvents,
  type JournalEntry,
  type JournalEventGroup,
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
  if (query.state.status !== "ready" && query.state.status !== "stale") {
    return null;
  }

  return (
    <>
      {query.state.status === "stale" &&
        (query.state.error ? (
          <div className="run-stale-state run-stale-state-error" role="alert">
            <span>
              <strong>Run detail may be stale</strong>
              <small>{query.state.error.message}</small>
            </span>
            <button className="text-button" onClick={query.retry} type="button">
              Try again
            </button>
          </div>
        ) : (
          <div aria-live="polite" className="run-stale-state" role="status">
            <span aria-hidden="true" className="loading-mark" />
            <span>Refreshing run detail…</span>
          </div>
        ))}
      <RunDetailWorkspace
        client={client}
        events={query.state.data.events}
        key={query.state.data.run.id}
        navigate={navigate}
        run={query.state.data.run}
        runId={runId}
      />
    </>
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
    eventNodeAtSequence(events, initialSeq, {
      branch: latestEvent?.branch,
      runId,
    }) ?? run.currentStage,
  );
  const [followingLatest, setFollowingLatest] = useState(true);
  const [selectedEvidenceSeq, setSelectedEvidenceSeq] = useState<number>();
  const inspectorRef = useRef<HTMLElement>(null);
  const fullscreenRootRef = useRef<HTMLDivElement>(null);
  const [fullscreenMode, setFullscreenMode] =
    useState<WorkflowGraphFullscreenMode>("none");
  const nodeStates = run.graph
    ? deriveNodeStates(run.graph, events, selectedSeq, runId)
    : {};
  const selectedNode = run.graph?.nodes.find((node) => node.id === selectedNodeId);
  const selectedEvidence = events.find((event) => event.seq === selectedEvidenceSeq);
  const selectedEvidenceVisit = selectedEvidence
    ? evidenceVisit(events, selectedEvidence, runId)
    : undefined;

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
    setSelectedNodeId(
      eventNodeAtSequence(events, initialSeq, {
        branch: latestEvent?.branch,
        runId,
      }) ?? run.currentStage,
    );
    setSelectedEvidenceSeq(
      latestEvent && isInspectableEvidenceEvent(latestEvent) ? latestEvent.seq : undefined,
    );
  }, [events, followingLatest, initialSeq, latestEvent, run.currentStage, runId]);

  const selectNode = (nodeId: string, shouldRevealInspector = false) => {
    setSelectedNodeId(nodeId);
    setSelectedEvidenceSeq(undefined);
    if (shouldRevealInspector) {
      revealInspector();
    }
  };

  const selectEvent = (event: RunEvent, shouldRevealInspector = false) => {
    setSelectedSeq(event.seq);
    setSelectedNodeId(
      eventNodeAtSequence(events, event.seq, { branch: event.branch, runId }),
    );
    setSelectedEvidenceSeq(isInspectableEvidenceEvent(event) ? event.seq : undefined);
    setFollowingLatest(event.seq === initialSeq);
    if (shouldRevealInspector) {
      revealInspector();
    }
  };

  const replaySeek = (seq: number) => {
    const event = events.find((candidate) => candidate.seq === seq);
    setSelectedSeq(seq);
    setSelectedNodeId(
      eventNodeAtSequence(events, seq, { branch: event?.branch, runId }),
    );
    setSelectedEvidenceSeq(undefined);
    setFollowingLatest(seq === initialSeq);
  };

  const causalEventSeq = run.escalation?.causalEventSeq;
  const causalEvent =
    causalEventSeq === undefined ? undefined : events.find((event) => event.seq === causalEventSeq);
  const causalNodeId =
    causalEventSeq === undefined
      ? undefined
      : eventNodeAtSequence(events, causalEventSeq, {
          branch: causalEvent?.branch,
          runId,
        });
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
        <div
          aria-label={
            fullscreenMode === "fallback" ? "Run graph fullscreen view" : undefined
          }
          aria-modal={fullscreenMode === "fallback" ? "true" : undefined}
          className={[
            "run-graph-fullscreen-root",
            "workflow-graph-fullscreen-target",
            fullscreenMode === "fallback" ? "workflow-graph-shell-expanded" : "",
          ]
            .filter(Boolean)
            .join(" ")}
          data-fullscreen={fullscreenMode}
          ref={fullscreenRootRef}
          role={fullscreenMode === "fallback" ? "dialog" : undefined}
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
                fullscreenTargetRef={fullscreenRootRef}
                graph={run.graph}
                nodeStates={nodeStates}
                onFullscreenModeChange={setFullscreenMode}
                onSelectStage={selectNode}
                selectedStageId={selectedNodeId}
                stateSeq={selectedSeq}
              />
            ) : (
              <div className="empty-detail" role="status">
                <strong>Pinned graph unavailable</strong>
                <span>
                  This historic run predates graph snapshots. Its event ledger remains
                  available.
                </span>
              </div>
            )}
          </GraphFrame>

          {events.length > 0 && (
            <ReplayScrubber
              events={events}
              onSeek={replaySeek}
              runId={runId}
              selectedSeq={selectedSeq}
              terminal={run.finishedAt != null}
            />
          )}

          {run.graphStatus === "pinned" && run.graph && (
            <RunStageInspector
              client={client}
              events={events}
              inspectorRef={inspectorRef}
              node={selectedNode}
              runId={runId}
              selectedEvidence={selectedEvidence}
              selectedEvidenceVisit={selectedEvidenceVisit}
              selectedSeq={selectedSeq}
            />
          )}
        </div>

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
  const [view, setView] = useState<"major" | "all">("major");
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(() => new Set());
  const rowRefs = useRef(new Map<string, HTMLButtonElement>());
  const grouped = journalEntries(events, run.id);
  const rows: JournalEntry[] =
    view === "all"
      ? orderRunEvents(events).map((event) => ({ kind: "event", event }))
      : grouped.flatMap((entry) =>
          entry.kind === "group" && expandedGroups.has(entry.id)
            ? [entry, ...entry.events.map((event) => ({ kind: "event" as const, event }))]
            : [entry],
        );

  const rowKey = (entry: JournalEntry) =>
    entry.kind === "group"
      ? entry.id
      : `event-${entry.event.branch}-${entry.event.seq}`;

  const moveSelection = (targetIndex: number) => {
    const entry = rows[targetIndex];
    if (!entry) {
      return;
    }
    if (entry.kind === "event") {
      onSelect(entry.event);
    }
    rowRefs.current.get(rowKey(entry))?.focus();
  };

  const handleRowKeyDown = (
    keyboardEvent: React.KeyboardEvent<HTMLButtonElement>,
    index: number,
  ) => {
    let targetIndex: number | undefined;
    if (keyboardEvent.key === "ArrowDown" || keyboardEvent.key === "ArrowRight") {
      targetIndex = Math.min(index + 1, rows.length - 1);
    } else if (keyboardEvent.key === "ArrowUp" || keyboardEvent.key === "ArrowLeft") {
      targetIndex = Math.max(index - 1, 0);
    } else if (keyboardEvent.key === "Home") {
      targetIndex = 0;
    } else if (keyboardEvent.key === "End") {
      targetIndex = rows.length - 1;
    }
    if (targetIndex !== undefined) {
      keyboardEvent.preventDefault();
      moveSelection(targetIndex);
    }
  };

  const toggleGroup = (group: JournalEventGroup) => {
    setExpandedGroups((current) => {
      const next = new Set(current);
      if (next.has(group.id)) {
        next.delete(group.id);
      } else {
        next.add(group.id);
      }
      return next;
    });
  };

  return (
    <section aria-labelledby="event-ledger-title" className="event-ledger">
      <div className="panel-heading-row">
        <div>
          <p className="section-kicker">Journal</p>
          <h2 id="event-ledger-title">Event ledger</h2>
        </div>
        <div className="journal-heading-actions">
          <span className="graph-legend">Ordered by durable sequence</span>
          <div aria-label="Journal event view" className="journal-view-control" role="group">
            <button
              aria-pressed={view === "major"}
              className={view === "major" ? "journal-view-button journal-view-button-active" : "journal-view-button"}
              onClick={() => setView("major")}
              type="button"
            >
              Major events
            </button>
            <button
              aria-pressed={view === "all"}
              className={view === "all" ? "journal-view-button journal-view-button-active" : "journal-view-button"}
              onClick={() => setView("all")}
              type="button"
            >
              All events ({events.length})
            </button>
          </div>
        </div>
      </div>
      {events.length === 0 ? (
        <div className="empty-detail" role="status">
          <strong>No durable events recorded</strong>
        </div>
      ) : (
        <ol>
          {rows.map((entry, index) => {
            if (entry.kind === "group") {
              const expanded = expandedGroups.has(entry.id);
              const selected = entry.events.some((event) => event.seq === selectedSeq);
              const first = entry.events[0];
              const last = entry.events.at(-1) ?? first;
              const scope = ledgerGroupScope(entry);
              return (
                <li
                  className={`ledger-item ledger-support-group ${selected ? "ledger-item-active" : ""}`}
                  key={entry.id}
                >
                  <button
                    aria-current={selected ? "true" : undefined}
                    aria-expanded={expanded}
                    aria-label={`${expanded ? "Collapse" : "Expand"} ${entry.events.length} supporting ${entry.events.length === 1 ? "event" : "events"} for ${scope}, sequences ${first.seq} through ${last.seq}`}
                    className="run-ledger-button"
                    onClick={() => toggleGroup(entry)}
                    onKeyDown={(event) => handleRowKeyDown(event, index)}
                    ref={(element) => {
                      if (element) {
                        rowRefs.current.set(rowKey(entry), element);
                      } else {
                        rowRefs.current.delete(rowKey(entry));
                      }
                    }}
                    type="button"
                  >
                    <span className="ledger-seq mono">
                      Seq {first.seq}
                      {last.seq === first.seq ? "" : `–${last.seq}`}
                    </span>
                    <span className="ledger-type mono">Supporting</span>
                    <span className="ledger-time mono">{entry.events.length} records</span>
                    <span className="ledger-attempt">{scope}</span>
                    <span className="ledger-copy">
                      <strong>
                        {expanded ? "Hide" : "Show"} supporting journal records
                      </strong>
                      <span>{ledgerGroupCategories(entry)}</span>
                      {selected && <span className="selected-event-label">Contains selected event</span>}
                    </span>
                  </button>
                </li>
              );
            }

            const event = entry.event;
            const selected = event.seq === selectedSeq;
            const heading = eventHeading(event);
            const summary = eventSummary(
              event,
              evidenceDecision(events, event, run.id),
              run.id,
            );
            const major = isMajorJournalEvent(event);
            return (
              <li
                className={[
                  "ledger-item",
                  major ? "ledger-item-major" : "ledger-item-supporting",
                  selected ? "ledger-item-active" : "",
                ]
                  .filter(Boolean)
                  .join(" ")}
                data-category={event.category ?? "unknown"}
                key={`${event.branch}-${event.seq}`}
              >
                <button
                  aria-current={selected ? "true" : undefined}
                  aria-label={`Select sequence ${event.seq}: ${heading}. ${summary}`}
                  className="run-ledger-button"
                  onClick={() => onSelect(event, true)}
                  onKeyDown={(keyboardEvent) => handleRowKeyDown(keyboardEvent, index)}
                  ref={(element) => {
                    if (element) {
                      rowRefs.current.set(rowKey(entry), element);
                    } else {
                      rowRefs.current.delete(rowKey(entry));
                    }
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
                    <span className="ledger-category">{ledgerCategoryLabel(event)}</span>
                    {!event.knownSchema && (
                      <span className="ledger-unknown">Unsupported schema {event.schema}</span>
                    )}
                    {selected && <span className="selected-event-label">Selected event</span>}
                  </span>
                </button>
                {event.externalRef?.url && (
                  <a
                    className="ledger-event-link"
                    href={event.externalRef.url}
                    rel="noreferrer"
                    target="_blank"
                  >
                    Open linked {externalRefLabel(event.externalRef.kind)}
                  </a>
                )}
              </li>
            );
          })}
        </ol>
      )}
    </section>
  );
}

function ledgerGroupScope(group: JournalEventGroup): string {
  if (!group.nodeId) {
    return "unscoped records";
  }
  const node = humanizeLedgerValue(group.nodeId);
  return group.visit ? `${node} · Visit ${group.visit}` : node;
}

function ledgerGroupCategories(group: JournalEventGroup): string {
  const counts = new Map<string, number>();
  for (const event of group.events) {
    const label = ledgerCategoryLabel(event);
    counts.set(label, (counts.get(label) ?? 0) + 1);
  }
  return [...counts.entries()]
    .map(([category, count]) => `${category} ${count}`)
    .join(" · ");
}

function ledgerCategoryLabel(event: RunEvent): string {
  return humanizeLedgerValue(event.category ?? "unknown");
}

function externalRefLabel(kind: string): string {
  return kind.toLowerCase() === "pr" ? "pull request" : humanizeLedgerValue(kind).toLowerCase();
}

function humanizeLedgerValue(value: string): string {
  const words = value.replace(/[._-]+/g, " ").trim();
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : "Event";
}
