import { useCallback, useEffect, useRef, useState } from "react";
import { MalformedResponseError } from "./api/errors";
import type {
  DaemonClient,
  RunDetail,
  RunEvent,
  WorkflowGraph,
} from "./api/types";
import type { QueryState } from "./api/queryState";
import { useLiveData } from "./liveData";

export type RunNodeState =
  | "pending"
  | "running"
  | "completed"
  | "failed"
  | "blocked"
  | "aborted"
  | "escalated";

export interface RunDetailSnapshot {
  run: RunDetail;
  events: RunEvent[];
}

export interface RunDetailQuery {
  retry: () => void;
  state: QueryState<RunDetailSnapshot>;
}

export function useRunDetail(client: DaemonClient, runId: string): RunDetailQuery {
  const [state, setState] = useState<QueryState<RunDetailSnapshot>>({ status: "loading" });
  const request = useRef<AbortController | undefined>(undefined);
  const { subscribe } = useLiveData();

  const refresh = useCallback((): Promise<boolean> => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;

    return loadRunDetail(client, runId, controller.signal).then(
      (data) => {
        if (controller.signal.aborted) {
          return true;
        }
        if (request.current === controller) {
          request.current = undefined;
        }
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
        setState({
          status: "error",
          error: error instanceof Error ? error : new Error("Unable to read run detail."),
        });
        return false;
      },
    );
  }, [client, runId]);

  useEffect(() => {
    setState({ status: "loading" });
    const unsubscribe = subscribe(["run"], refresh);
    return () => {
      unsubscribe();
      request.current?.abort();
      request.current = undefined;
    };
  }, [refresh, subscribe]);

  const retry = useCallback(() => {
    setState({ status: "loading" });
    void refresh();
  }, [refresh]);
  return { retry, state };
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
  return event.stage || event.gate || undefined;
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

export function deriveNodeStates(
  graph: WorkflowGraph,
  events: RunEvent[],
  selectedSeq: number,
): Record<string, RunNodeState> {
  const states = Object.fromEntries(
    graph.nodes.map((node) => [node.id, "pending" as RunNodeState]),
  );
  let activeNodeId: string | undefined;

  for (const event of orderRunEvents(events)) {
    if (event.seq > selectedSeq) {
      break;
    }
    if (event.type === "run.finished") {
      if (activeNodeId) {
        states[activeNodeId] = stateFromStatus(event.status);
      }
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

  return states;
}

export function eventHeading(event: RunEvent): string {
  if (!event.knownSchema) {
    return "Unsupported event";
  }
  const headings: Record<string, string> = {
    "run.started": "Run started",
    "run.finished": "Run finished",
    "stage.started": "Stage started",
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

export function eventSummary(event: RunEvent): string {
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
    case "artifact.recorded":
      return `${event.artifact?.name || event.name || "An artifact"} was recorded.`;
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
