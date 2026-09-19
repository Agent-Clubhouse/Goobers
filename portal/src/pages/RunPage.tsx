import { useEffect, useRef, useState } from "react";
import type {
  DaemonClient,
  ExternalRef,
  RunDetail,
  RunEvent,
  WorkflowGraph,
} from "../api/types";
import { EscalationPanel } from "../components/EscalationPanel";
import { FailurePanel } from "../components/FailurePanel";
import { ReplayScrubber } from "../components/ReplayScrubber";
import {
  WorkflowTopologyGraph,
  type WorkflowGraphFullscreenMode,
} from "../components/WorkflowTopologyGraph";
import {
  deriveBranchStates,
  deriveNodeStates,
  deriveTraversedEdges,
  evidenceDecision,
  eventHeading,
  eventNodeAtSequence,
  eventNodeId,
  eventSummary,
  formatDuration,
  formatElapsed,
  formatTimestamp,
  humanize as humanizeLedgerValue,
  isFailureJournalEvent,
  isMajorJournalEvent,
  keyMoments,
  eventStage,
  journalEntries,
  nodeOwner,
  orderRunEvents,
  runFailure,
  semanticStageVisits,
  type JournalEntry,
  runEventStages,
  type SemanticStageVisit,
  UNSCOPED_EVENT_STAGE,
  type JournalEventGroup,
  type RunNodeState,
  useRunDetail,
} from "../runDetailData";
import {
  routeHash,
  type Navigate,
  type RunDetailTab,
} from "../routing";
import { GraphFrame } from "../ui/GraphFrame";
import { Icon } from "../ui/Icon";
import { StatusBadge } from "../ui/StatusBadge";
import { useCobrand } from "../cobrand";

export function RunPage({
  client,
  eventDetail,
  navigate,
  nodeId,
  revealRun,
  runId,
  sequence,
  standalone,
  tab,
}: {
  client: DaemonClient;
  eventDetail?: boolean;
  navigate: Navigate;
  nodeId?: string;
  revealRun: (runId: string) => Promise<void>;
  runId: string;
  sequence?: number;
  standalone: boolean;
  tab?: RunDetailTab;
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
      {/*
       * Only the stale+error case renders anything (matches WorkflowPage,
       * ErrorsPage, InsightPage, GagglePage): every live invalidation makes
       * useLiveData's connection freshness dip through "stale" for the
       * refresh's round-trip (liveData.tsx's drainInvalidations), which
       * flows into this query's status on every single live event for an
       * active run — not just on genuine disconnects. A no-error "stale"
       * banner here previously popped in and out above the graph/journal on
       * every event, reflowing them each time (#2530, recurrence of the
       * #2307/#2304/#2308 background-refresh-must-not-disrupt-the-view
       * class). Real connection health is already surfaced globally by
       * PortalShell's persistent freshness indicator.
       */}
      {query.state.status === "stale" && query.state.error && (
        <div className="run-stale-state run-stale-state-error" role="alert">
          <span>
            <strong>Run detail may be stale</strong>
            <small>{query.state.error.message}</small>
          </span>
          <button className="text-button" onClick={query.retry} type="button">
            Retry
          </button>
        </div>
      )}
      <RunDetailWorkspace
        events={query.state.data.events}
        eventDetail={eventDetail}
        key={query.state.data.run.id}
        navigate={navigate}
        routeNodeId={nodeId}
        routeSequence={sequence}
        routeTab={tab}
        revealRun={revealRun}
        run={query.state.data.run}
        runId={runId}
      />
    </>
  );
}

function RunDetailWorkspace({
  events,
  eventDetail,
  navigate,
  routeNodeId,
  routeSequence,
  routeTab,
  revealRun,
  run,
  runId,
}: {
  events: RunEvent[];
  eventDetail?: boolean;
  navigate: Navigate;
  routeNodeId?: string;
  routeSequence?: number;
  routeTab?: RunDetailTab;
  revealRun: (runId: string) => Promise<void>;
  run: RunDetail;
  runId: string;
}) {
  const latestEvent = events.at(-1);
  const initialSeq = latestEvent?.seq ?? 0;
  const latestNodeId =
    eventNodeAtSequence(events, initialSeq, {
      branch: latestEvent?.branch,
      runId,
    }) ?? run.currentStage;
  const routedEvent = events.find((event) => event.seq === routeSequence);
  const startingSeq = routedEvent?.seq ?? initialSeq;
  const startingNodeId =
    routeNodeId ??
    eventNodeAtSequence(events, startingSeq, {
      branch: routedEvent?.branch ?? latestEvent?.branch,
      runId,
    }) ??
    latestNodeId;
  const [selectedSeq, setSelectedSeq] = useState(startingSeq);
  const [selectedNodeId, setSelectedNodeId] = useState<string | undefined>(startingNodeId);
  const [followingLatest, setFollowingLatest] = useState(routedEvent === undefined);
  const [revealPending, setRevealPending] = useState(false);
  const [revealError, setRevealError] = useState<string>();
  const [runIdCopied, setRunIdCopied] = useState(false);
  const [activeTab, setActiveTab] = useState<RunDetailTab>(routeTab ?? "overview");
  const { config: portalConfig, loading: portalConfigLoading } = useCobrand();
  const fullscreenRootRef = useRef<HTMLDivElement>(null);
  const [fullscreenMode, setFullscreenMode] =
    useState<WorkflowGraphFullscreenMode>("none");
  const nodeStates = run.graph
    ? deriveNodeStates(run.graph, events, selectedSeq, runId)
    : {};
  const traversedEdges = deriveTraversedEdges(run.transitions, selectedSeq);
  const branchStates = deriveBranchStates(events, selectedSeq);
  const orderedEvents = orderRunEvents(events);
  const selectedEventIndex = orderedEvents.findIndex(
    (event) => event.seq === selectedSeq,
  );
  const selectedEvent =
    selectedEventIndex >= 0 ? orderedEvents[selectedEventIndex] : undefined;
  const previousEvent =
    selectedEventIndex > 0 ? orderedEvents[selectedEventIndex - 1] : undefined;
  const nextEvent =
    selectedEventIndex >= 0 && selectedEventIndex < orderedEvents.length - 1
      ? orderedEvents[selectedEventIndex + 1]
      : undefined;

  useEffect(() => {
    const event =
      routeSequence === undefined
        ? latestEvent
        : events.find((candidate) => candidate.seq === routeSequence);
    const nextSeq = event?.seq ?? initialSeq;
    setActiveTab(routeTab ?? "overview");
    setSelectedSeq(nextSeq);
    setSelectedNodeId(
      routeNodeId ??
        eventNodeAtSequence(events, nextSeq, {
          branch: event?.branch ?? latestEvent?.branch,
          runId,
        }) ??
        latestNodeId,
    );
    setFollowingLatest(routeSequence === undefined);
  }, [
    events,
    initialSeq,
    latestEvent?.branch,
    latestNodeId,
    routeNodeId,
    routeSequence,
    routeTab,
    runId,
  ]);

  const navigateRun = (
    nextTab: RunDetailTab = "overview",
    seq?: number,
    node?: string,
    showEventDetail = false,
  ) => {
    const event = events.find((candidate) => candidate.seq === seq);
    setActiveTab(nextTab);
    if (event) {
      setSelectedSeq(event.seq);
      setSelectedNodeId(
        node ??
          eventNodeAtSequence(events, event.seq, {
            branch: event.branch,
            runId,
          }),
      );
      setFollowingLatest(false);
    } else if (node) {
      setSelectedNodeId(node);
      const followsLatest = node === latestNodeId;
      if (followsLatest) {
        setSelectedSeq(initialSeq);
      }
      setFollowingLatest(followsLatest);
    } else if (nextTab === "overview") {
      setSelectedSeq(initialSeq);
      setSelectedNodeId(latestNodeId);
      setFollowingLatest(true);
    }
    navigate({
      page: "run",
      id: runId,
      tab: nextTab === "overview" ? undefined : nextTab,
      seq,
      node,
      event: showEventDetail || undefined,
    });
  };

  const selectNode = (nodeId: string) => {
    const nodeEvent =
      [...orderedEvents]
        .reverse()
        .find(
          (event) =>
            event.seq <= selectedSeq && eventNodeId(event, runId) === nodeId,
        ) ??
      [...orderedEvents]
        .reverse()
        .find((event) => eventNodeId(event, runId) === nodeId);
    navigateRun(
      "diagnostics",
      nodeId === latestNodeId ? undefined : nodeEvent?.seq ?? selectedSeq,
      nodeId,
      nodeEvent !== undefined,
    );
  };

  const selectEvent = (event: RunEvent) => {
    navigateRun(activeTab, event.seq, undefined, true);
  };

  const replaySeek = (seq: number, showEventDetail = false) => {
    navigateRun(activeTab, seq, undefined, showEventDetail);
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
    causalEventSeq === undefined
      ? undefined
      : () => navigateRun(activeTab, causalEventSeq, undefined, true);

  const failure = runFailure(run, events);
  const failureCausalEvent =
    failure?.causalEventSeq === undefined
      ? undefined
      : events.find((event) => event.seq === failure.causalEventSeq);

  const revealFiles = async () => {
    setRevealPending(true);
    setRevealError(undefined);
    try {
      await revealRun(runId);
    } catch (error) {
      setRevealError(error instanceof Error ? error.message : "The run directory could not be opened.");
    } finally {
      setRevealPending(false);
    }
  };
  const copyRunId = async () => {
    try {
      await navigator.clipboard.writeText(run.id);
      setRunIdCopied(true);
    } catch {
      setRunIdCopied(false);
    }
  };
  const displayedRunId = shortenIdentifier(run.id);
  const relatedReferences = collectRelatedReferences(run, events);
  const stageVisits = semanticStageVisits(events, runId);
  const inspectSequence = (seq: number) => {
    navigateRun(activeTab, seq, undefined, true);
  };
  const closeEventDetail = () => {
    const nextHash = routeHash({
      page: "run",
      id: runId,
      tab: activeTab === "overview" ? undefined : activeTab,
      seq: routeSequence,
      node: routeNodeId,
    });
    window.history.replaceState(window.history.state, "", nextHash);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  };

  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumbs">
        <button onClick={() => navigate({ page: "runs" })} type="button">
          Runs
        </button>
        <Icon name="chevron" size={14} />
        <span className="mono breadcrumb-run-id" title={run.id}>
          {displayedRunId}
        </span>
      </nav>

      <header className="run-heading">
        <div className="run-heading-main">
          <div className="run-heading-title">
            <h1 aria-label={`Run ${run.id}`}>Run <span aria-hidden="true">{displayedRunId}</span></h1>
            <button
              aria-label={runIdCopied ? "Run ID copied" : "Copy full run ID"}
              className="run-id-copy"
              onClick={() => void copyRunId()}
              title={runIdCopied ? "Copied" : `Copy ${run.id}`}
              type="button"
            >
              <Icon name={runIdCopied ? "check" : "copy"} size={16} />
            </button>
            <StatusBadge stale={run.stale} status={run.phase} />
          </div>
          <p className="run-identity-line">
            <span>
              {run.gaggle} / {run.workflow} · Pinned v
              {run.graph?.version ?? run.workflowVersion} ·{" "}
              <span className="mono">
                {run.graph?.digest ?? run.workflowDigest ?? "Digest unavailable"}
              </span>
            </span>
          </p>
          {((!portalConfigLoading && portalConfig.capabilities.revealRun) ||
            relatedReferences.length > 0) && (
            <div className="run-heading-actions">
              {!portalConfigLoading && portalConfig.capabilities.revealRun && (
                <button
                  className="scope-pivot-link run-heading-action"
                  disabled={revealPending}
                  onClick={() => void revealFiles()}
                  type="button"
                >
                  <Icon name="artifact" size={14} />
                {revealPending ? "Opening…" : "Reveal run files"}
                </button>
              )}
              {relatedReferences.map((reference) => (
                <a
                  className="scope-pivot-link run-heading-action"
                  href={reference.url}
                  key={`${reference.provider}/${reference.kind}/${reference.id}`}
                  rel="noreferrer"
                  target="_blank"
                >
                  <Icon name="arrow" size={14} />
                  Open related {externalRefLabel(reference.kind)} #{reference.id}
                </a>
              ))}
              {revealError && <span role="alert">{revealError}</span>}
            </div>
          )}
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
        </dl>
      </header>

      {run.stale && (
        <div className="run-stale-state run-stale-run" role="status">
          <span>
            <strong>Stale / unmonitored</strong>
            <small>No recent run activity is available and the daemon heartbeat is stale.</small>
          </span>
        </div>
      )}

      {run.escalation && (
        <EscalationPanel
          causalEvent={causalEvent}
          escalation={run.escalation}
          onFocusCausalEvent={focusCausalEvent}
        />
      )}

      {failure && (
        <FailurePanel
          causalEvent={failureCausalEvent}
          errorsHref={routeHash({
            page: "errors",
            filters: {
              gaggle: run.gaggle,
              workflow: run.workflow,
              stage: failure.stage,
              code: failure.code,
            },
          })}
          failure={failure}
          onFocusCausalEvent={
            failure.causalEventSeq === undefined
              ? undefined
              : () => navigateRun(activeTab, failure.causalEventSeq, undefined, true)
          }
          phase={run.phase}
        />
      )}

      <RunDetailTabs
        activeTab={activeTab}
        onSelect={(nextTab) =>
          navigateRun(
            nextTab,
            nextTab === "overview" || followingLatest
              ? undefined
              : selectedSeq,
            nextTab === "diagnostics" ? routeNodeId : undefined,
          )
        }
      />

      {activeTab === "overview" && (
        <RunOverview
          events={events}
          onInspectSequence={inspectSequence}
          run={run}
          visits={stageVisits}
        />
      )}

      {activeTab === "diagnostics" && (
        <section
          aria-labelledby="run-tab-diagnostics"
          className="run-detail-workspace"
          data-scroll-owner="page"
          data-responsive-layout="stack-under-820"
          id="run-panel-diagnostics"
          role="tabpanel"
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
            eyebrow=""
          >
            {run.graphStatus === "pinned" && run.graph ? (
              <WorkflowTopologyGraph
                branchStates={branchStates}
                causalNodeId={causalNodeId}
                fullscreenTargetRef={fullscreenRootRef}
                graph={run.graph}
                initialZoom={0.9}
                nodeStates={nodeStates}
                onFullscreenModeChange={setFullscreenMode}
                onSelectStage={selectNode}
                selectedStageId={selectedNodeId}
                stateSeq={selectedSeq}
                traversedEdges={traversedEdges}
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
              graph={run.graph}
              onInspect={(seq) => replaySeek(seq, true)}
              onSeek={replaySeek}
              runId={runId}
              selectedSeq={selectedSeq}
              terminal={run.finishedAt != null}
            />
          )}
        </div>
        </section>
      )}

      {activeTab === "journal" && (
        <section
          aria-labelledby="run-tab-journal"
          id="run-panel-journal"
          role="tabpanel"
        >
          <div className="run-journal-column">
            <EventLedger
              events={events}
              onSelect={selectEvent}
              run={run}
              selectedSeq={selectedSeq}
            />
          </div>
        </section>
      )}

      {eventDetail && selectedEvent && (
        <EventDetailDialog
          associatedDecision={evidenceDecision(events, selectedEvent, runId)}
          event={selectedEvent}
          graph={run.graph}
          nextEvent={nextEvent}
          onClose={closeEventDetail}
          onNext={() => nextEvent && navigateRun(activeTab, nextEvent.seq, undefined, true)}
          onPrevious={() =>
            previousEvent && navigateRun(activeTab, previousEvent.seq, undefined, true)
          }
          previousEvent={previousEvent}
          runId={runId}
          workflow={run.workflow}
        />
      )}
    </>
  );
}

const RUN_DETAIL_TABS: Array<{ id: RunDetailTab; label: string }> = [
  { id: "overview", label: "Overview" },
  { id: "diagnostics", label: "Graph" },
  { id: "journal", label: "Journal" },
];

function RunDetailTabs({
  activeTab,
  onSelect,
}: {
  activeTab: RunDetailTab;
  onSelect: (tab: RunDetailTab) => void;
}) {
  const move = (event: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    let nextIndex: number | undefined;
    if (event.key === "ArrowRight" || event.key === "ArrowDown") {
      nextIndex = (index + 1) % RUN_DETAIL_TABS.length;
    } else if (event.key === "ArrowLeft" || event.key === "ArrowUp") {
      nextIndex = (index - 1 + RUN_DETAIL_TABS.length) % RUN_DETAIL_TABS.length;
    } else if (event.key === "Home") {
      nextIndex = 0;
    } else if (event.key === "End") {
      nextIndex = RUN_DETAIL_TABS.length - 1;
    }
    if (nextIndex === undefined) {
      return;
    }
    event.preventDefault();
    const tab = RUN_DETAIL_TABS[nextIndex];
    onSelect(tab.id);
    document.getElementById(`run-tab-${tab.id}`)?.focus();
  };

  return (
    <div aria-label="Run detail views" className="run-detail-tabs" role="tablist">
      {RUN_DETAIL_TABS.map((tab, index) => (
        <button
          aria-controls={`run-panel-${tab.id}`}
          aria-selected={activeTab === tab.id}
          className={activeTab === tab.id ? "run-detail-tab run-detail-tab-active" : "run-detail-tab"}
          id={`run-tab-${tab.id}`}
          key={tab.id}
          onClick={() => onSelect(tab.id)}
          onKeyDown={(event) => move(event, index)}
          role="tab"
          tabIndex={activeTab === tab.id ? 0 : -1}
          type="button"
        >
          {tab.label}
        </button>
      ))}
    </div>
  );
}

function RunOverview({
  events,
  onInspectSequence,
  run,
  visits,
}: {
  events: RunEvent[];
  onInspectSequence: (seq: number) => void;
  run: RunDetail;
  visits: SemanticStageVisit[];
}) {
  const current =
    [...visits].reverse().find((visit) => visit.status === "running") ?? visits.at(-1);
  const latestFailure = [...visits]
    .reverse()
    .find((visit) => ["failed", "blocked", "aborted", "escalated"].includes(visit.status));
  const currentReason =
    current?.repass?.reason ||
    (current?.status === "running" ? run.operator?.nextTransition : current?.result);
  const lastActivity = events.at(-1)?.time ?? run.lastActivityAt;

  return (
    <section
      aria-labelledby="run-tab-overview"
      className="run-overview"
      id="run-panel-overview"
      role="tabpanel"
    >
      <article className={`run-current-state run-current-state-${current?.status ?? run.phase}`}>
        <div className="run-current-state-heading">
          <div>
            <p>Current state</p>
            <h2>
              {current ? humanizeLedgerValue(current.stage) : "Run"} —{" "}
              {semanticStatusLabel(current?.status ?? run.phase)}
            </h2>
          </div>
        </div>
        <p className="run-current-state-context">
          {current
            ? `Visit ${current.visit}${current.repass ? ` · ${repassKindLabel(current.repass.kind)} after ${humanizeLedgerValue(current.repass.sourceStage)}` : ""}`
            : "No stage activity has been recorded yet."}
        </p>
        {currentReason && <p className="run-current-state-reason">{currentReason}</p>}
        <p className="run-current-state-activity">
          Last activity {formatActivityAge(lastActivity)}
        </p>
        <div className="run-current-state-actions">
          {current && (
            <button
              className="scope-pivot-link run-heading-action"
              onClick={() => onInspectSequence(current.startedSeq)}
              type="button"
            >
              View stage details
            </button>
          )}
          {latestFailure && latestFailure !== current && (
            <button
              className="scope-pivot-link run-heading-action"
              onClick={() => onInspectSequence(latestFailure.finishedSeq ?? latestFailure.startedSeq)}
              type="button"
            >
              View latest failure
            </button>
          )}
        </div>
      </article>

      <section className="run-stage-history" aria-labelledby="run-stage-history-title">
        <div className="panel-heading-row">
          <h2 id="run-stage-history-title">Progress &amp; Transitions</h2>
        </div>
        <ol className="run-stage-list">
          {visits.map((visit) => (
            <li className={`run-stage-row run-stage-row-${visit.status}`} key={visit.id}>
              <button
                aria-label={`Open ${visit.stage}, visit ${visit.visit}, ${semanticStatusLabel(visit.status)}`}
                onClick={() => onInspectSequence(visit.finishedSeq ?? visit.startedSeq)}
                type="button"
              >
                <span aria-hidden="true" className="run-stage-status-mark" />
                <span className="run-stage-name">
                  <strong>{humanizeLedgerValue(visit.stage)}</strong>
                  <small>
                    Visit {visit.visit}
                    {visit.attempt ? ` · Attempt ${visit.attempt}` : ""}
                  </small>
                </span>
                <span className="run-stage-result">
                  <strong>{visit.repass ? `Returned from ${humanizeLedgerValue(visit.repass.sourceStage)}` : visit.result}</strong>
                  {visit.repass && <small>{visit.repass.reason}</small>}
                </span>
                <span className="run-stage-duration">
                  {visit.durationMillis === undefined ? "In progress" : formatDuration(visit.durationMillis)}
                </span>
                <span className="run-stage-action">Details</span>
              </button>
            </li>
          ))}
        </ol>
      </section>
    </section>
  );
}

function semanticStatusLabel(status: string): string {
  return humanizeLedgerValue(status);
}

function EventDetailDialog({
  associatedDecision,
  event,
  graph,
  nextEvent,
  onClose,
  onNext,
  onPrevious,
  previousEvent,
  runId,
  workflow,
}: {
  associatedDecision?: RunEvent;
  event: RunEvent;
  graph?: WorkflowGraph;
  nextEvent?: RunEvent;
  onClose: () => void;
  onNext: () => void;
  onPrevious: () => void;
  previousEvent?: RunEvent;
  runId: string;
  workflow?: string;
}) {
  const dialogRef = useRef<HTMLElement>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const stageId = eventNodeId(event, runId);
  const node = graph?.nodes.find((candidate) => candidate.id === stageId);
  const owner = nodeOwner(graph, stageId);
  const summary = eventSummary(event, associatedDecision, runId).replace(
    / Select this event to inspect (?:the artifact|the evidence)\.$/,
    "",
  );

  useEffect(() => {
    dialogRef.current?.focus();
    const onKeyDown = (keyEvent: KeyboardEvent) => {
      if (keyEvent.key === "Escape") {
        keyEvent.preventDefault();
        onCloseRef.current();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  return (
    <div
      className="event-detail-backdrop"
      onMouseDown={(mouseEvent) => {
        if (mouseEvent.target === mouseEvent.currentTarget) {
          onClose();
        }
      }}
    >
      <section
        aria-labelledby="event-detail-title"
        aria-modal="true"
        className="event-detail-dialog"
        ref={dialogRef}
        role="dialog"
        tabIndex={-1}
      >
        <header>
          <div>
            <p className="section-kicker">Sequence {event.seq}</p>
            <h2 id="event-detail-title">Event detail</h2>
          </div>
          <button
            aria-label="Close event detail"
            className="dialog-close"
            onClick={onClose}
            type="button"
          >
            <Icon name="close" size={16} />
          </button>
        </header>
        <dl className="event-detail-meta">
          <div>
            <dt>Time</dt>
            <dd>{formatTimestamp(event.time)}</dd>
          </div>
          <div>
            <dt>Type</dt>
            <dd><code>{event.type}</code></dd>
          </div>
          {workflow && (
            <div>
              <dt>Workflow</dt>
              <dd>{workflow}</dd>
            </div>
          )}
          {stageId && (
            <div>
              <dt>Stage</dt>
              <dd>{humanizeLedgerValue(stageId)}</dd>
            </div>
          )}
          {node && (
            <div>
              <dt>Kind</dt>
              <dd>{humanizeLedgerValue(node.kind)}</dd>
            </div>
          )}
          {owner && (
            <div>
              <dt>Goober</dt>
              <dd>{owner}</dd>
            </div>
          )}
          {event.attempt !== undefined && (
            <div>
              <dt>Attempt</dt>
              <dd>{event.attempt}</dd>
            </div>
          )}
        </dl>
        <div className="event-detail-summary">
          <strong>{eventHeading(event)}</strong>
          <p>{summary}</p>
        </div>
        <footer>
          <button
            className="scope-pivot-link run-heading-action"
            disabled={!previousEvent}
            onClick={onPrevious}
            type="button"
          >
            Previous event
          </button>
          <button
            className="scope-pivot-link run-heading-action"
            disabled={!nextEvent}
            onClick={onNext}
            type="button"
          >
            Next event
          </button>
        </footer>
      </section>
    </div>
  );
}

function repassKindLabel(kind: "correction" | "infrastructure" | "retry"): string {
  if (kind === "correction") {
    return "corrective repass";
  }
  if (kind === "infrastructure") {
    return "infrastructure retry";
  }
  return "retry";
}

function formatActivityAge(value: string | undefined): string {
  if (!value) {
    return "unavailable";
  }
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) {
    return "unavailable";
  }
  const seconds = Math.max(0, Math.floor((Date.now() - timestamp) / 1_000));
  if (seconds < 60) {
    return `${seconds}s ago`;
  }
  if (seconds < 3_600) {
    return `${Math.floor(seconds / 60)}m ago`;
  }
  if (seconds < 86_400) {
    return `${Math.floor(seconds / 3_600)}h ago`;
  }
  return formatTimestamp(value);
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
  const [view, setView] = useState<"key" | "major" | "all">("major");
  const [stageFilter, setStageFilter] = useState<string>("");
  const [searchQuery, setSearchQuery] = useState("");
  const [expandedGroups, setExpandedGroups] = useState<Set<string>>(() => new Set());
  const rowRefs = useRef(new Map<string, HTMLButtonElement>());
  const stages = runEventStages(events, run.id);
  // A filter naming a stage this run never visited would silently empty the
  // ledger; treat it as unset instead.
  const activeStage = stageFilter && stages.includes(stageFilter) ? stageFilter : "";
  const stageFiltered = activeStage
    ? events.filter((event) => eventStage(event, run.id) === activeStage)
    : events;
  const query = searchQuery.trim().toLowerCase();
  const visible = query
    ? stageFiltered.filter((event) => eventMatchesQuery(event, events, run.id, query))
    : stageFiltered;
  const keyMomentIds = new Set(
    keyMoments(visible).map(({ event }) => `${event.branch}-${event.seq}`),
  );
  const grouped = journalEntries(visible, run.id);
  const rows: JournalEntry[] =
    view === "all"
      ? orderRunEvents(visible).map((event) => ({ kind: "event", event }))
      : view === "key"
        ? orderRunEvents(visible)
            .filter((event) => keyMomentIds.has(`${event.branch}-${event.seq}`))
            .map((event) => ({ kind: "event", event }))
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
      <div className="panel-heading-row event-ledger-heading">
        <h2 id="event-ledger-title">Event ledger</h2>
        <span className="graph-legend">Ordered by durable sequence</span>
      </div>
      <div aria-label="Event ledger filters" className="filter-bar event-ledger-filter-bar">
        <button
          aria-describedby="journal-view-key-hint"
          aria-pressed={view === "key"}
          className={view === "key" ? "filter-button filter-button-active" : "filter-button"}
          onClick={() => setView("key")}
          title="Show decisions, escalations, and branch handoffs"
          type="button"
        >
          Key moments
        </button>
        <span className="sr-only" id="journal-view-key-hint">
          Shows decisions, escalations, and branch handoffs in durable sequence order
        </span>
        <button
          aria-describedby="journal-view-major-hint"
          aria-pressed={view === "major"}
          className={view === "major" ? "filter-button filter-button-active" : "filter-button"}
          onClick={() => setView("major")}
          title="Show only stage/gate landmarks, hiding evidence and liveness noise"
          type="button"
        >
          Major events
        </button>
        <span className="sr-only" id="journal-view-major-hint">
          Shows only stage/gate landmarks, hiding evidence and liveness noise
        </span>
        <button
          aria-describedby="journal-view-all-hint"
          aria-pressed={view === "all"}
          className={view === "all" ? "filter-button filter-button-active" : "filter-button"}
          onClick={() => setView("all")}
          title="Show every durable event of every kind"
          type="button"
        >
          All events ({events.length})
        </button>
        <span className="sr-only" id="journal-view-all-hint">
          Shows every durable event of every kind, independent of the stage filter
        </span>
        <div className="event-ledger-filter-fields">
          <label className="filter-search event-ledger-filter-field">
            <span>Search</span>
            <input
              onChange={(changeEvent) => setSearchQuery(changeEvent.target.value)}
              placeholder="Search events"
              type="search"
              value={searchQuery}
            />
          </label>
          {stages.length > 1 && (
            <label className="filter-select event-ledger-filter-field">
              <span>Stage</span>
              <select
                aria-label="Narrow the journal to one stage, independent of the event-kind toggle above"
                onChange={(changeEvent) => setStageFilter(changeEvent.target.value)}
                value={activeStage}
              >
                <option value="">All stages</option>
                {stages.map((stage) => {
                  const owner =
                    stage === UNSCOPED_EVENT_STAGE ? undefined : nodeOwner(run.graph, stage);
                  const label = stage === UNSCOPED_EVENT_STAGE ? "Run-level" : stage;
                  return (
                    <option key={stage} value={stage}>
                      {owner ? `${label} — ${owner}` : label}
                    </option>
                  );
                })}
              </select>
            </label>
          )}
        </div>
      </div>
      {events.length === 0 ? (
        <div className="empty-detail" role="status">
          <strong>No durable events recorded</strong>
        </div>
      ) : rows.length === 0 ? (
        <div className="empty-detail" role="status">
          <strong>No events match</strong>
          {query && <span>No events match “{searchQuery.trim()}”.</span>}
        </div>
      ) : (
        <div className="data-table-shell event-ledger-table">
          <div aria-hidden="true" className="data-table-header event-ledger-table-header">
            <span>Sequence</span>
            <span>Stage</span>
            <span>Type</span>
            <span>Elapsed</span>
            <span>Attempt #</span>
            <span>Event</span>
          </div>
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
                    <span className="ledger-seq">
                      {first.seq}
                      {last.seq === first.seq ? "" : `–${last.seq}`}
                    </span>
                    <span className="ledger-stage">{entry.nodeId ?? UNSCOPED_EVENT_STAGE}</span>
                    <span className="ledger-type">Supporting</span>
                    <span className="ledger-time">{entry.events.length} records</span>
                    <span className="ledger-attempt">N/A</span>
                    <span className="ledger-copy">
                      <strong className="ledger-group-toggle-label">
                        <span aria-hidden="true" className="ledger-support-chevron">
                          <Icon name="chevron" size={14} />
                        </span>
                        {expanded ? "Collapse" : "Expand"} supporting journal records
                      </strong>
                      <span>{ledgerGroupCategories(entry)}</span>
                    </span>
                  </button>
                  <details className="ledger-mobile-detail">
                    <summary>More group details</summary>
                    <dl>
                      <div><dt>Stage</dt><dd>{entry.nodeId ?? UNSCOPED_EVENT_STAGE}</dd></div>
                      <div><dt>Sequences</dt><dd>{first.seq}–{last.seq}</dd></div>
                      <div><dt>Records</dt><dd>{entry.events.length}</dd></div>
                      <div><dt>Categories</dt><dd>{ledgerGroupCategories(entry)}</dd></div>
                    </dl>
                  </details>
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
            const failed = isFailureJournalEvent(event);
            return (
              <li
                className={[
                  "ledger-item",
                  major ? "ledger-item-major" : "ledger-item-supporting",
                  selected ? "ledger-item-active" : "",
                  failed ? "ledger-item-failure" : "",
                ]
                  .filter(Boolean)
                  .join(" ")}
                data-category={event.category ?? "unknown"}
                key={`${event.branch}-${event.seq}`}
              >
                <button
                  aria-current={selected ? "true" : undefined}
                  aria-label={`Select sequence ${event.seq}: ${eventStage(event, run.id)}. ${heading}. ${summary}${failed ? " Failed." : ""}`}
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
                  <span className="ledger-seq">{event.seq}</span>
                  <span className="ledger-stage">{eventStage(event, run.id)}</span>
                  <span className="ledger-type">{event.type}</span>
                  <span className="ledger-time">
                    {formatElapsed(run.startedAt, event.time)}
                  </span>
                  <span className="ledger-attempt">
                    {event.attempt ?? "N/A"}
                  </span>
                  <span className="ledger-copy">
                    <strong>{heading}</strong>
                    <span>{summary}</span>
                    <span
                      className={failed ? "ledger-labels ledger-labels-failure" : "ledger-labels"}
                    >
                      <span className="ledger-category">{ledgerCategoryLabel(event)}</span>
                      {failed && (
                        <span className="ledger-severity">
                          <Icon name="alert" size={9} />
                          Failed
                        </span>
                      )}
                    </span>
                    {!event.knownSchema && (
                      <span className="ledger-unknown">Unsupported schema {event.schema}</span>
                    )}
                  </span>
                </button>
                <details className="ledger-mobile-detail">
                  <summary>More event details</summary>
                  <dl>
                    <div><dt>Type</dt><dd>{event.type}</dd></div>
                    <div><dt>Attempt</dt><dd>{event.attempt ?? "N/A"}</dd></div>
                    <div><dt>Category</dt><dd>{ledgerCategoryLabel(event)}</dd></div>
                  </dl>
                </details>
                {event.externalRef?.url && (
                  <div className="ledger-event-action-row">
                    <a
                      className="ledger-event-link"
                      href={event.externalRef.url}
                      rel="noreferrer"
                      target="_blank"
                    >
                      <Icon name="arrow" size={14} />
                      Open linked {externalRefLabel(event.externalRef.kind)}
                    </a>
                  </div>
                )}
              </li>
            );
          })}
          </ol>
        </div>
      )}
    </section>
  );
}

/**
 * eventMatchesQuery decides whether an event's ledger row would show the
 * given lowercased query, matching against the same text a reader sees on
 * the row: its heading, summary, stage, and raw type.
 */
function eventMatchesQuery(
  event: RunEvent,
  allEvents: RunEvent[],
  runId: string,
  query: string,
): boolean {
  const summary = eventSummary(event, evidenceDecision(allEvents, event, runId), runId);
  const haystack = [eventHeading(event), summary, eventStage(event, runId), event.type]
    .filter((value): value is string => typeof value === "string")
    .join("\n")
    .toLowerCase();
  return haystack.includes(query);
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

function collectRelatedReferences(run: RunDetail, events: RunEvent[]): ExternalRef[] {
  const references = events
    .map((event) => event.externalRef)
    .filter((reference): reference is ExternalRef => Boolean(reference?.url));
  const pullRequest = run.operator?.pullRequest;
  if (pullRequest?.url) {
    references.push({
      provider: pullRequest.provider,
      kind: pullRequest.kind,
      id: pullRequest.id,
      url: pullRequest.url,
    });
  }
  return [...new Map(
    references.map((reference) => [
      `${reference.provider}/${reference.kind}/${reference.id}`,
      reference,
    ]),
  ).values()];
}

function shortenIdentifier(value: string): string {
  return value.length > 20 ? `${value.slice(0, 10)}…${value.slice(-6)}` : value;
}
