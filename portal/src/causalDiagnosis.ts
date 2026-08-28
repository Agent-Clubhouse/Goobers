import type { RunEvent, RunDetail } from "./api/types";
import { eventNodeId, orderRunEvents, runFailure, type RunFailure } from "./runDetailData";

export interface VisitLineage {
  branch: number;
  stage: string;
  visit: number;
  attempt?: number;
  startedAt?: string;
  finishedAt?: string;
  status?: string;
}

export interface WaterfallRow {
  key: string;
  branch: number;
  stage: string;
  kind: "stage" | "gate" | "parallel" | "unknown";
  attempt?: number;
  startedAt: string;
  finishedAt?: string;
  durationMillis?: number;
  idleBeforeMillis: number;
  status?: string;
}

function millis(value: string | undefined): number | undefined {
  if (!value) return undefined;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) ? parsed : undefined;
}

export function deriveVisitLineage(events: RunEvent[], runId?: string): VisitLineage[] {
  const visits = new Map<string, VisitLineage>();
  const counts = new Map<string, number>();
  const active = new Map<string, VisitLineage>();
  for (const event of orderRunEvents(events)) {
    const stage = eventNodeId(event, runId);
    if (!stage || !["stage.started", "gate.started", "stage.finished", "gate.evaluated"].includes(event.type)) {
      continue;
    }
    const key = `${event.branch}:${stage}`;
    let current = active.get(key);
    if (event.type === "stage.started" || event.type === "gate.started") {
      const visitNumber = (counts.get(key) ?? 0) + 1;
      counts.set(key, visitNumber);
      current = {
        branch: event.branch,
        stage,
        visit: visitNumber,
        attempt: event.attempt,
        startedAt: event.time,
      };
      visits.set(`${key}:${visitNumber}`, current);
      active.set(key, current);
    }
    if (!current) continue;
    if (event.attempt !== undefined) current.attempt = event.attempt;
    if (event.type === "stage.finished" || event.type === "gate.evaluated") {
      current.finishedAt = event.time;
      current.status = event.status ?? event.verdict;
      active.delete(key);
    }
  }
  return [...visits.values()];
}

export function deriveWaterfall(events: RunEvent[], runId?: string): WaterfallRow[] {
  const rows: WaterfallRow[] = [];
  let previousEnd: number | undefined;
  for (const event of orderRunEvents(events)) {
    if (!["stage.started", "gate.started", "parallel.started"].includes(event.type)) continue;
    const stage = eventNodeId(event, runId) ?? event.stage ?? event.gate ?? event.parallel ?? "unknown";
    const end = orderRunEvents(events).find(
      (candidate) =>
        candidate.branch === event.branch &&
        candidate.seq > event.seq &&
        ((event.type === "stage.started" && candidate.type === "stage.finished") ||
          (event.type === "gate.started" && candidate.type === "gate.evaluated") ||
          (event.type === "parallel.started" && candidate.type === "parallel.finished")) &&
        eventNodeId(candidate, runId) === stage,
    );
    const started = millis(event.time);
    const finished = millis(end?.time);
    const idleBeforeMillis =
      started !== undefined && previousEnd !== undefined ? Math.max(0, started - previousEnd) : 0;
    if (finished !== undefined) previousEnd = finished;
    rows.push({
      key: `${event.branch}:${event.seq}`,
      branch: event.branch,
      stage,
      kind: event.type.startsWith("gate") ? "gate" : event.type.startsWith("parallel") ? "parallel" : "stage",
      attempt: event.attempt,
      startedAt: event.time,
      finishedAt: end?.time,
      durationMillis: started !== undefined && finished !== undefined ? Math.max(0, finished - started) : undefined,
      idleBeforeMillis,
      status: end?.status ?? end?.verdict,
    });
  }
  return rows;
}

export function causalFailure(run: RunDetail, events: RunEvent[]): RunFailure | undefined {
  return runFailure(run, events);
}
