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
    if (!current) {
      const visitNumber = (counts.get(key) ?? 0) + 1;
      counts.set(key, visitNumber);
      current = {
        branch: event.branch,
        stage,
        visit: visitNumber,
        attempt: event.attempt,
        finishedAt: event.time,
        status: event.status ?? event.verdict,
      };
      visits.set(`${key}:${visitNumber}`, current);
      continue;
    }
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
  const active = new Map<string, WaterfallRow[]>();
  const ordered = orderRunEvents(events);
  for (const event of ordered) {
    if (!["stage.started", "gate.started", "parallel.started"].includes(event.type)) {
      const stage = eventNodeId(event, runId);
      const endType =
        event.type === "stage.finished"
          ? "stage.started"
          : event.type === "gate.evaluated"
            ? "gate.started"
            : event.type === "parallel.finished"
              ? "parallel.started"
              : undefined;
      if (!stage || !endType) continue;
      const key = `${event.branch}:${endType}:${stage}`;
      const candidates = active.get(key);
      const row = candidates?.shift();
      if (row) {
        row.finishedAt = event.time;
        row.status = event.status ?? event.verdict;
      }
      continue;
    }
    const stage = eventNodeId(event, runId) ?? event.stage ?? event.gate ?? event.parallel ?? "unknown";
    const row: WaterfallRow = {
      key: `${event.branch}:${event.seq}`,
      branch: event.branch,
      stage,
      kind: event.type.startsWith("gate") ? "gate" : event.type.startsWith("parallel") ? "parallel" : "stage",
      attempt: event.attempt,
      startedAt: event.time,
      finishedAt: undefined,
      durationMillis: undefined,
      idleBeforeMillis: 0,
      status: undefined,
    };
    rows.push(row);
    const key = `${event.branch}:${event.type}:${stage}`;
    const candidates = active.get(key) ?? [];
    candidates.push(row);
    active.set(key, candidates);
  }
  const previousEnd = new Map<number, number>();
  for (const row of rows) {
    const started = millis(row.startedAt);
    const finished = millis(row.finishedAt);
    const branchEnd = previousEnd.get(row.branch);
    row.idleBeforeMillis =
      started !== undefined && branchEnd !== undefined ? Math.max(0, started - branchEnd) : 0;
    row.durationMillis =
      started !== undefined && finished !== undefined ? Math.max(0, finished - started) : undefined;
    if (finished !== undefined) {
      previousEnd.set(row.branch, Math.max(branchEnd ?? finished, finished));
    }
  }
  return rows;
}

export function deriveFailureBreadcrumb(run: RunDetail, events: RunEvent[]): string {
  const failure = runFailure(run, events);
  if (!failure) return "Run failed without a recorded reason.";
  const location = [
    failure.stage,
    failure.attempt === undefined ? undefined : `Attempt ${failure.attempt}`,
  ]
    .filter(Boolean)
    .join(" · ");
  return `${failure.message}${location ? ` → ${location}` : ""}`;
}
