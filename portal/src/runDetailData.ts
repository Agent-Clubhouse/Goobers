import { useCallback, useEffect, useRef, useState } from "react";
import { MalformedResponseError } from "./api/errors";
import type {
  DaemonClient,
  GraphTerminal,
  RunDetail,
  RunEvent,
  RunTransition,
  WorkflowGraph,
  WorkflowGraphEdge,
} from "./api/types";
import type { QueryState } from "./api/queryState";
import { dataCacheKey, type DataCacheDependency } from "./dataCache";
import { useLiveData } from "./liveData";

export type RunNodeState =
  | "pending"
  | "running"
  | "completed"
  | "failed"
  | "blocked"
  | "aborted"
  | "escalated"
  | "skipped";

export interface RunDetailSnapshot {
  run: RunDetail;
  events: RunEvent[];
}

export interface RunDetailQuery {
  retry: () => void;
  state: QueryState<RunDetailSnapshot>;
}

export interface JournalEventGroup {
  kind: "group";
  id: string;
  events: RunEvent[];
  nodeId?: string;
  visit?: number;
}

export interface JournalEventItem {
  kind: "event";
  event: RunEvent;
}

export type JournalEntry = JournalEventGroup | JournalEventItem;

interface JournalVisitState {
  ordinal: number;
  humanVisit: boolean;
  humanRequested: boolean;
}

export function useRunDetail(client: DaemonClient, runId: string): RunDetailQuery {
  const { cache, freshness, subscribe } = useLiveData();
  const cacheKey = dataCacheKey("run-detail", runId);
  const [state, setState] = useState<QueryState<RunDetailSnapshot>>(() => {
    const cached = cache.get<RunDetailSnapshot>(cacheKey);
    return cached ? { status: "ready", data: cached } : { status: "loading" };
  });
  const request = useRef<AbortController | undefined>(undefined);

  const refresh = useCallback((): Promise<boolean> => {
    request.current?.abort();
    const dependencies = runDetailDependencies(runId);
    const cacheRevision = cache.beginWrite(cacheKey, dependencies);
    const controller = new AbortController();
    request.current = controller;
    setState((current) =>
      current.status === "ready" || current.status === "stale"
        ? { status: "stale", data: current.data }
        : { status: "loading" },
    );

    return loadRunDetail(client, runId, controller.signal).then(
      (data) => {
        if (controller.signal.aborted) {
          return true;
        }
        if (request.current === controller) {
          request.current = undefined;
        }
        cache.set(cacheKey, data, dependencies, cacheRevision);
        setState({ status: "ready", data });
        return true;
      },
      (error: unknown) => {
        if (controller.signal.aborted) {
          return true;
        }
        if (request.current === controller) {
          request.current = undefined;
        }
        const queryError =
          error instanceof Error ? error : new Error("Unable to read run detail.");
        setState((current) =>
          current.status === "stale"
            ? { status: "stale", data: current.data, error: queryError }
            : { status: "error", error: queryError },
        );
        return false;
      },
    );
  }, [cache, cacheKey, client, runId]);

  useEffect(() => {
    const cached = cache.get<RunDetailSnapshot>(cacheKey);
    setState(cached ? { status: "ready", data: cached } : { status: "loading" });
    const unsubscribe = subscribe(
      ["run"],
      (_models, reason) => {
        const current = reason === "initial" ? cache.get<RunDetailSnapshot>(cacheKey) : undefined;
        if (current) {
          setState({ status: "ready", data: current });
          return true;
        }
        return refresh();
      },
      { runId },
    );
    return () => {
      unsubscribe();
      request.current?.abort();
      request.current = undefined;
    };
  }, [cache, cacheKey, refresh, subscribe]);

  // Freshness downgrade (#1714).
  //
  // Every other query hook has this; these two did not, so a detail page kept
  // reporting "ready" after the live stream dropped — the row looked current
  // while the stream behind it was gone. The status is what the page renders
  // its freshness indicator from, so without this the indicator lies.
  //
  // It moves ready -> stale on disconnect and back on reconnect, and never
  // clears an error: a stale-with-error state must stay visibly errored rather
  // than silently becoming "ready" because the socket came back.
  useEffect(() => {
    setState((current) => {
      if (freshness !== "connected" && current.status === "ready") {
        return { status: "stale", data: current.data };
      }
      if (freshness === "connected" && current.status === "stale" && !current.error) {
        return { status: "ready", data: current.data };
      }
      return current;
    });
  }, [freshness]);

  const retry = useCallback(() => {
    // Evict the cached snapshot so a retry refetches rather than re-serving the
    // entry that just failed — but do NOT reset to "loading". Blanking the page
    // to a full skeleton on retry is the regression #1684 fixed; refresh() already
    // moves ready/stale data to "stale" and keeps it visible while the refetch runs.
    cache.remove(cacheKey);
    void refresh();
  }, [cache, cacheKey, refresh]);
  return { retry, state };
}

function runDetailDependencies(runId: string): readonly DataCacheDependency[] {
  return [{ model: "run", runId }];
}

export async function loadRunDetail(
  client: DaemonClient,
  runId: string,
  signal?: AbortSignal,
): Promise<RunDetailSnapshot> {
  const options = { signal };
  const [run, eventList] = await Promise.all([
    client.getRun(runId, options),
    client.listRunEvents(runId, options),
  ]);
  if (run.id !== runId || eventList.runId !== runId) {
    throw new MalformedResponseError("The daemon returned mismatched run detail.");
  }
  if (run.graphStatus === "pinned" && !run.graph) {
    throw new MalformedResponseError("The daemon omitted the pinned run graph.");
  }
  return { run, events: orderRunEvents(eventList.events) };
}

export function orderRunEvents(events: RunEvent[]): RunEvent[] {
  return [...events].sort((left, right) => left.seq - right.seq || left.branch - right.branch);
}

/**
 * The single authoritative "why" for a failed run, mirroring what EscalationPanel
 * gives an escalated run and RunOutcome gives a completed one. A failed run's
 * RunDetail carries no escalation/outcome cause (readservice only populates those
 * for the escalated/completed phases), so — until the read model projects a
 * FailureCause of its own — the portal derives it here from the durable journal:
 * the failing stage attempt carries the coded ErrorDetail (the same field
 * readservice reads in stageEscalationReason). This is what turns the run page
 * from "it failed, go dig" into "failed: harness.crash — Harness exited before
 * producing a result envelope" at a glance.
 */
export interface RunFailure {
  /** Human-readable failure reason, always non-empty. */
  message: string;
  /** Coded error, when the failing event recorded one (e.g. "harness.crash"). */
  code?: string;
  /** Stage that failed, when the failure was attributable to one. */
  stage?: string;
  /** Attempt number of the failing stage visit, when recorded. */
  attempt?: number;
  /** Sequence of the durable event that carried the failure, for deep-linking. */
  causalEventSeq?: number;
}

export function runFailure(run: RunDetail, events: RunEvent[]): RunFailure | undefined {
  // Escalated runs get EscalationPanel and completed runs get RunOutcome; this is
  // strictly the failed axis, so the two banners never fight over the same run.
  if (run.phase !== "failed") {
    return undefined;
  }

  let errored: RunEvent | undefined;
  let failingStage: RunEvent | undefined;
  let finished: RunEvent | undefined;
  for (const event of orderRunEvents(events)) {
    if (event.error && (event.error.code || event.error.message)) {
      errored = event;
    }
    if (
      event.type === "stage.finished" &&
      (event.status === "failure" || event.status === "blocked")
    ) {
      failingStage = event;
    }
    if (event.type === "run.finished") {
      finished = event;
    }
  }

  const causal = errored ?? failingStage ?? finished;
  const stage = errored?.stage ?? failingStage?.stage;
  const attempt = errored?.attempt ?? failingStage?.attempt;
  const code = errored?.error?.code?.trim() || undefined;
  const errorText = errored?.error?.message?.trim() || errored?.error?.code?.trim();
  const message =
    errorText ||
    finished?.reason?.trim() ||
    (stage
      ? `Stage ${humanize(stage)} failed without a recorded reason.`
      : "The run failed without a recorded reason.");

  return { message, code, stage, attempt, causalEventSeq: causal?.seq };
}

export function eventNodeId(event: RunEvent, runId?: string): string | undefined {
  if (event.stage) {
    if (!isTranscriptEvent(event)) {
      return event.stage;
    }
    const runPrefix = runId ? `${runId}:` : undefined;
    return runPrefix && event.stage.startsWith(runPrefix)
      ? event.stage.slice(runPrefix.length)
      : event.stage;
  }
  return event.artifact?.stage || event.gate;
}

export function eventNodeAtSequence(
  events: RunEvent[],
  selectedSeq: number,
  options: { branch?: number; runId?: string } = {},
): string | undefined {
  const orderedEvents = orderRunEvents(events);
  const selectedBranch =
    options.branch ?? orderedEvents.find((event) => event.seq === selectedSeq)?.branch;
  let nodeId: string | undefined;
  for (const event of orderedEvents) {
    if (event.seq > selectedSeq) {
      break;
    }
    if (selectedBranch !== undefined && event.branch !== selectedBranch) {
      continue;
    }
    nodeId = eventNodeId(event, options.runId) ?? nodeId;
  }
  return nodeId;
}

export function journalEntries(events: RunEvent[], runId?: string): JournalEntry[] {
  const entries: JournalEntry[] = [];
  const visits = new Map<string, JournalVisitState>();
  const activeNodes = new Map<number, string>();

  for (const event of orderRunEvents(events)) {
    const directNodeId = eventNodeId(event, runId);
    if (directNodeId) {
      activeNodes.set(event.branch, directNodeId);
    }
    const nodeId = directNodeId ?? activeNodes.get(event.branch);
    const visitKey = nodeId ? `${event.branch}:${nodeId}` : undefined;
    let visit: number | undefined;
    if (visitKey) {
      const state = nextVisitState(
        event,
        visits.get(visitKey) ?? {
          ordinal: 0,
          humanVisit: false,
          humanRequested: false,
        },
      );
      visits.set(visitKey, state);
      visit = Math.max(state.ordinal, 1);
    }

    if (isMajorJournalEvent(event)) {
      entries.push({ kind: "event", event });
      continue;
    }

    const previous = entries.at(-1);
    if (
      previous?.kind === "group" &&
      previous.nodeId === nodeId &&
      previous.visit === visit &&
      previous.events.at(-1)?.branch === event.branch
    ) {
      previous.events.push(event);
      continue;
    }
    entries.push({
      kind: "group",
      id: `support-${event.branch}-${event.seq}`,
      events: [event],
      nodeId,
      visit,
    });
  }

  return entries;
}

export function evidenceVisit(
  events: RunEvent[],
  evidence: RunEvent,
  runId?: string,
): number | undefined {
  for (const entry of journalEntries(events, runId)) {
    if (
      entry.kind === "group" &&
      entry.events.some(
        (event) => event.branch === evidence.branch && event.seq === evidence.seq,
      )
    ) {
      return entry.visit;
    }
  }
  return undefined;
}

export function isInspectableEvidenceEvent(event: RunEvent): boolean {
  return isTranscriptEvent(event) || (event.type === "artifact.recorded" && !!event.artifact);
}

export function isMajorJournalEvent(event: RunEvent): boolean {
  if (!event.knownSchema || !event.category) {
    return true;
  }
  return (
    event.category === "transition" ||
    event.category === "decision" ||
    event.category === "result" ||
    event.category === "unknown"
  );
}

export function evidenceDecision(
  events: RunEvent[],
  evidence: RunEvent,
  runId?: string,
): RunEvent | undefined {
  if (!isVerdictArtifact(evidence)) {
    return undefined;
  }
  const nodeId =
    eventNodeId(evidence, runId) ??
    eventNodeAtSequence(events, evidence.seq, { branch: evidence.branch, runId });
  for (const event of orderRunEvents(events)) {
    if (event.seq <= evidence.seq) {
      continue;
    }
    if (event.branch !== evidence.branch) {
      continue;
    }
    if (
      event.type === "gate.evaluated" &&
      (!nodeId || eventNodeId(event, runId) === nodeId)
    ) {
      return event;
    }
    if (event.type === "gate.started" || event.type === "stage.started") {
      return undefined;
    }
  }
  return undefined;
}

export function deriveNodeStates(
  graph: WorkflowGraph,
  events: RunEvent[],
  selectedSeq: number,
  runId?: string,
): Record<string, RunNodeState> {
  const states = Object.fromEntries(
    graph.nodes.map((node) => [node.id, "pending" as RunNodeState]),
  );
  let activeNodeId: string | undefined;
  let terminal = false;

  for (const event of orderRunEvents(events)) {
    if (event.seq > selectedSeq) {
      break;
    }
    if (event.type === "run.finished") {
      if (activeNodeId) {
        states[activeNodeId] = stateFromStatus(event.status);
      }
      terminal = true;
      continue;
    }
    const nodeId = eventNodeId(event, runId);
    if (!nodeId || !Object.hasOwn(states, nodeId)) {
      continue;
    }
    activeNodeId = nodeId;

    switch (event.type) {
      case "stage.started":
      case "gate.started":
        states[nodeId] = "running";
        break;
      case "stage.finished":
        states[nodeId] = stateFromStatus(event.status);
        break;
      case "gate.evaluated":
        states[nodeId] = stateFromGate(event);
        break;
    }
  }

  // Once the run is terminal (as of the selected sequence), a node that was
  // never entered is not still "pending" — it is a no-work node the run ended
  // without visiting, i.e. "skipped". Deriving this at the end (rather than
  // per-event) is what keeps skipped nodes from reverting to pending when the
  // run.finished event is processed — the DASH-19 regression.
  if (terminal) {
    for (const id of Object.keys(states)) {
      if (states[id] === "pending") {
        states[id] = "skipped";
      }
    }
  }

  return states;
}

function traversedEdgeKey(source: string, target: string): string {
  return `${source}->${target}`;
}

function traversedTerminalKey(source: string, terminal: GraphTerminal): string {
  return `${source}=>${terminal}`;
}

// A terminal transition's TerminalStatus is the run's/branch's actual outcome
// (journal.RunPhase or journal.BranchStatus on the Go side); this maps it to
// the declared graph's own terminal vocabulary. "failed" has no entry: a bare
// stage failure with no declared gate route (#1427's task-sourced terminal
// case) never corresponds to a declared edge, and #1427/#1430 both require
// never fabricating one.
const TERMINAL_STATUS_TO_GRAPH_TERMINAL: Partial<Record<string, GraphTerminal>> = {
  completed: "complete",
  aborted: "abort",
  escalated: "escalate",
};

// TraversedEdges maps an edge key (see traversedEdgeKey/traversedTerminalKey)
// to the causal sequence of its LAST crossing at or before the playhead — the
// edge-level analogue of deriveNodeStates' state map. The value, not just
// membership, is what lets a per-edge description read "Traversed at
// sequence 23" (#1431) rather than a bare yes/no.
export type TraversedEdges = Map<string, number>;

// deriveTraversedEdges is deriveNodeStates' edge-level counterpart: it answers
// "which DECLARED edges has the run actually crossed, as of selectedSeq, and
// at what sequence" from the run's exact transition history (#1427) — never
// by inferring an edge was taken merely because both endpoints were visited,
// which false-positives on any cyclic workflow (#1430): a forward path
// visits both endpoints of an untaken repass edge for unrelated reasons.
export function deriveTraversedEdges(
  transitions: RunTransition[] | undefined,
  selectedSeq: number,
): TraversedEdges {
  const traversed: TraversedEdges = new Map();
  if (!transitions) {
    return traversed;
  }
  // #1427's ProjectTransitions sorts rows by causal sequence before
  // returning, so the same "break once past the playhead" idiom
  // deriveNodeStates uses above applies here unchanged — and a repeatedly
  // traversed edge's key is overwritten with its LATEST occurrence's
  // sequence, so repeated repasses never create phantom/duplicate edges.
  for (const transition of transitions) {
    if (transition.seq > selectedSeq) {
      break;
    }
    if (transition.terminal) {
      const terminal = transition.status
        ? TERMINAL_STATUS_TO_GRAPH_TERMINAL[transition.status]
        : undefined;
      if (terminal) {
        traversed.set(traversedTerminalKey(transition.source, terminal), transition.seq);
      }
      continue;
    }
    if (transition.target) {
      traversed.set(traversedEdgeKey(transition.source, transition.target), transition.seq);
    }
  }
  return traversed;
}

// edgeTraversed reports whether a declared graph edge is on the executed path,
// per the map deriveTraversedEdges produced.
export function edgeTraversed(
  traversedEdges: TraversedEdges | undefined,
  edge: WorkflowGraphEdge,
): boolean {
  return traversedEdgeSeq(traversedEdges, edge) !== undefined;
}

// traversedEdgeSeq returns the causal sequence a declared graph edge was last
// crossed at, or undefined when it was never crossed as of the playhead.
export function traversedEdgeSeq(
  traversedEdges: TraversedEdges | undefined,
  edge: WorkflowGraphEdge,
): number | undefined {
  if (!traversedEdges) {
    return undefined;
  }
  return edge.terminal
    ? traversedEdges.get(traversedTerminalKey(edge.source, edge.terminal))
    : traversedEdges.get(traversedEdgeKey(edge.source, edge.target));
}

export function eventHeading(event: RunEvent): string {
  if (!event.knownSchema) {
    return "Unsupported event";
  }
  if (isTranscriptEvent(event)) {
    return "Transcript recorded";
  }
  if (isVerdictArtifact(event)) {
    return "Structured verdict recorded";
  }
  const headings: Record<string, string> = {
    "run.started": "Run started",
    "run.finished": "Run finished",
    "stage.started": "Stage started",
    "stage.heartbeat": "Stage heartbeat",
    "stage.finished": "Stage finished",
    "gate.started": "Gate started",
    "gate.evaluated": "Gate evaluated",
    "artifact.recorded": "Artifact recorded",
    error: "Error recorded",
    "input.snapshot": "Input snapshotted",
    "ref.touched": "External reference touched",
    redaction: "Journal content redacted",
    repaired: "Journal repaired",
    "runner.annotation": "Runner annotation",
    "span.recorded": "Span recorded",
  };
  return headings[event.type] ?? humanize(event.type);
}

export function eventSummary(
  event: RunEvent,
  associatedDecision?: RunEvent,
  runId?: string,
): string {
  if (!event.knownSchema) {
    return `Schema ${event.schema} is not supported; ${event.type} is retained with generic fields.`;
  }

  const node = eventNodeId(event, runId);
  switch (event.type) {
    case "run.started":
      return event.workflow ? `${event.workflow} began execution.` : "The run began execution.";
    case "run.finished":
      return `The run finished as ${event.status || "terminal"}.`;
    case "stage.started":
      return `${humanize(node || "stage")} began execution.`;
    case "stage.finished":
      return `${humanize(node || "stage")} finished with ${event.status || "an outcome"}.`;
    case "gate.started":
      return `${humanize(node || "gate")} began evaluation.`;
    case "gate.evaluated": {
      const target = event.target ? ` and selected ${event.target}` : "";
      return `${humanize(node || "gate")} returned ${event.verdict || "a verdict"}${target}.`;
    }
    case "artifact.recorded": {
      const name = event.artifact?.name || event.name || "An artifact";
      const stage = node ? ` for ${humanize(node)}` : "";
      const access = event.artifact ? " Select this event to inspect the artifact." : "";
      if (isVerdictArtifact(event) && associatedDecision?.type === "gate.evaluated") {
        const gate = humanize(eventNodeId(associatedDecision, runId) || node || "review");
        const verdict = associatedDecision.verdict || "a verdict";
        const target = associatedDecision.target
          ? ` selecting ${associatedDecision.target}`
          : "";
        return `${name} captured the ${gate} decision: ${verdict}${target}.${access}`;
      }
      return `${name} was recorded${stage}.${access}`;
    }
    case "span.recorded": {
      const stage = node ? ` for ${humanize(node)}` : "";
      const kind = isTranscriptEvent(event) ? "Transcript" : "Trace evidence";
      return `${kind}${stage} was recorded. Select this event to inspect the evidence.`;
    }
    case "stage.heartbeat": {
      const attempt = event.attempt ? ` attempt ${event.attempt}` : "";
      return `${humanize(node || "stage")}${attempt} reported liveness; workflow state did not change.`;
    }
    case "ref.touched": {
      const reference = event.externalRef;
      if (!reference) {
        return "An external reference was recorded.";
      }
      const kind = externalRefKind(reference.kind);
      const id = reference.kind === "pr" || reference.kind === "issue"
        ? `#${reference.id}`
        : reference.id;
      const operation = externalRefOperation(event);
      return `${providerName(reference.provider)} ${operation} ${kind} ${id}.`;
    }
    case "error":
      return event.error?.message || event.error?.code || "An error was recorded.";
    default:
      return event.reason || event.name || event.target || "Durable journal event.";
  }
}

export function formatElapsed(startedAt: string, eventTime: string): string {
  const elapsed = Date.parse(eventTime) - Date.parse(startedAt);
  if (!Number.isFinite(elapsed) || elapsed < 0) {
    return "Unavailable";
  }
  const totalSeconds = Math.floor(elapsed / 1_000);
  const hours = Math.floor(totalSeconds / 3_600);
  const minutes = Math.floor((totalSeconds % 3_600) / 60);
  const seconds = totalSeconds % 60;
  return hours > 0
    ? `${hours}:${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`
    : `${minutes}:${String(seconds).padStart(2, "0")}`;
}

export function formatDuration(milliseconds: number): string {
  const totalSeconds = Math.max(0, Math.floor(milliseconds / 1_000));
  const hours = Math.floor(totalSeconds / 3_600);
  const minutes = Math.floor((totalSeconds % 3_600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) {
    return `${hours}h ${minutes}m ${seconds}s`;
  }
  if (minutes > 0) {
    return `${minutes}m ${seconds}s`;
  }
  return `${seconds}s`;
}

export function formatTimestamp(value: string | undefined): string {
  if (!value) {
    return "In progress";
  }
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) {
    return "Unavailable";
  }
  return new Intl.DateTimeFormat("en", {
    year: "numeric",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZone: "UTC",
    timeZoneName: "short",
  }).format(timestamp);
}

function stateFromGate(event: RunEvent): RunNodeState {
  const target = event.target?.toLowerCase() ?? "";
  if (target.includes("escalate")) {
    return "escalated";
  }
  if (target.includes("abort")) {
    return "aborted";
  }
  return stateFromStatus(event.status, "completed");
}

function nextVisitState(event: RunEvent, current: JournalVisitState): JournalVisitState {
  const next = { ...current };
  if (event.type === "stage.rerun.requested") {
    next.humanRequested = true;
    return next;
  }
  if (event.type !== "stage.started" && event.type !== "gate.started") {
    return next;
  }
  switch (event.attemptClass) {
    case undefined:
    case "initial":
      next.ordinal += 1;
      next.humanVisit = false;
      next.humanRequested = false;
      break;
    case "human":
      // A retry of an interrupted human rerun remains in the same visit.
      if (next.humanRequested || !next.humanVisit || next.ordinal === 0) {
        next.ordinal += 1;
      }
      next.humanVisit = true;
      next.humanRequested = false;
      break;
    default:
      if (next.ordinal === 0) {
        next.ordinal = 1;
      }
  }
  return next;
}

export function isTranscriptEvent(event: RunEvent): boolean {
  const name = event.name?.toLowerCase() ?? "";
  return event.type === "span.recorded" && (name === "transcript" || name.endsWith(".transcript"));
}

export function isVerdictArtifact(event: RunEvent): boolean {
  const name = (event.artifact?.name || event.name || "").toLowerCase();
  return event.type === "artifact.recorded" && name.includes("verdict");
}

function externalRefKind(kind: string): string {
  switch (kind.toLowerCase()) {
    case "pr":
      return "pull request";
    case "issue":
      return "issue";
    default:
      return kind.replace(/[._-]+/g, " ");
  }
}

function externalRefOperation(event: RunEvent): string {
  const operation = event.runner?.operation;
  if (typeof operation !== "string") {
    return "touched";
  }
  const operations: Record<string, string> = {
    claim: "claimed",
    close: "closed",
    create: "created",
    delete: "deleted",
    merge: "merged",
    open: "opened",
    push: "pushed",
    release: "released",
    update: "updated",
  };
  return operations[operation.toLowerCase()] ?? operation.replace(/[._-]+/g, " ");
}

function providerName(provider: string): string {
  return provider.toLowerCase() === "github" ? "GitHub" : humanize(provider);
}

function stateFromStatus(
  status: RunEvent["status"],
  fallback: RunNodeState = "completed",
): RunNodeState {
  switch (status) {
    case "running":
      return "running";
    case "failure":
    case "failed":
      return "failed";
    case "blocked":
      return "blocked";
    case "aborted":
      return "aborted";
    case "escalated":
      return "escalated";
    case "success":
    case "no-work":
    case "completed":
      return "completed";
    default:
      return fallback;
  }
}

function humanize(value: string): string {
  const words = value.replace(/[._-]+/g, " ").trim();
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : "Event";
}

// UNSCOPED_EVENT_STAGE labels a journal event that belongs to the run itself
// rather than to any one stage or gate — run.started, run.finished, and the
// bare artifact records the runner writes between stages.
export const UNSCOPED_EVENT_STAGE = "—";

// eventStage is the scope a journal row is scanned by: the stage that produced
// the event, or the gate that evaluated it. Both exist on RunEvent and only one
// is ever set, so a single column can carry either — which is what makes the
// ledger scannable at all. Reading a run means finding "the second implement
// attempt", and until this was a column that meant reading every row's prose.
export function eventStage(event: RunEvent): string {
  return event.stage ?? event.gate ?? UNSCOPED_EVENT_STAGE;
}

// runEventStages lists every distinct scope present in a run, in first-seen
// durable order, so a filter can offer exactly the stages this run visited
// rather than every stage the workflow declares. The unscoped bucket sorts
// last: it is a fallback, never something a reader is looking for first.
export function runEventStages(events: RunEvent[]): string[] {
  const seen = new Set<string>();
  const stages: string[] = [];
  for (const event of orderRunEvents(events)) {
    const stage = eventStage(event);
    if (stage === UNSCOPED_EVENT_STAGE || seen.has(stage)) {
      continue;
    }
    seen.add(stage);
    stages.push(stage);
  }
  if (events.some((event) => eventStage(event) === UNSCOPED_EVENT_STAGE)) {
    stages.push(UNSCOPED_EVENT_STAGE);
  }
  return stages;
}
