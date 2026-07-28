import { useCallback, useEffect, useRef, useState } from "react";
import { MalformedResponseError } from "./api/errors";
import type {
  DaemonClient,
  RunDetail,
  RunEvent,
  WorkflowGraph,
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

export function useRunDetail(client: DaemonClient, runId: string): RunDetailQuery {
  const { cache, subscribe } = useLiveData();
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

export function eventNodeId(event: RunEvent): string | undefined {
  const nodeId = event.stage || event.artifact?.stage || event.gate;
  if (!nodeId) {
    return undefined;
  }
  const separator = nodeId.indexOf(":");
  return separator >= 0 ? nodeId.slice(separator + 1) : nodeId;
}

export function eventNodeAtSequence(
  events: RunEvent[],
  selectedSeq: number,
): string | undefined {
  let nodeId: string | undefined;
  for (const event of orderRunEvents(events)) {
    if (event.seq > selectedSeq) {
      break;
    }
    nodeId = eventNodeId(event) ?? nodeId;
  }
  return nodeId;
}

export function journalEntries(events: RunEvent[]): JournalEntry[] {
  const entries: JournalEntry[] = [];
  const visits = new Map<string, number>();
  const activeNodes = new Map<number, string>();

  for (const event of orderRunEvents(events)) {
    const directNodeId = eventNodeId(event);
    if (directNodeId) {
      activeNodes.set(event.branch, directNodeId);
    }
    const nodeId = directNodeId ?? activeNodes.get(event.branch);
    const visitKey = nodeId ? `${event.branch}:${nodeId}` : undefined;
    if (visitKey && startsVisit(event)) {
      visits.set(visitKey, (visits.get(visitKey) ?? 0) + 1);
    } else if (visitKey && !visits.has(visitKey)) {
      visits.set(visitKey, 1);
    }
    const visit = visitKey ? visits.get(visitKey) : undefined;

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

export function evidenceVisit(events: RunEvent[], evidence: RunEvent): number | undefined {
  for (const entry of journalEntries(events)) {
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
): RunEvent | undefined {
  if (!isVerdictArtifact(evidence)) {
    return undefined;
  }
  const nodeId = eventNodeId(evidence);
  for (const event of orderRunEvents(events)) {
    if (event.seq <= evidence.seq) {
      continue;
    }
    if (event.type === "gate.evaluated" && (!nodeId || eventNodeId(event) === nodeId)) {
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
    const nodeId = eventNodeId(event);
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

export function eventSummary(event: RunEvent, associatedDecision?: RunEvent): string {
  if (!event.knownSchema) {
    return `Schema ${event.schema} is not supported; ${event.type} is retained with generic fields.`;
  }

  const node = eventNodeId(event);
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
        const gate = humanize(eventNodeId(associatedDecision) || node || "review");
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

function startsVisit(event: RunEvent): boolean {
  return (
    (event.type === "stage.started" || event.type === "gate.started") &&
    (event.attempt === undefined || event.attempt === 1) &&
    (event.attemptClass === undefined || event.attemptClass === "initial")
  );
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
