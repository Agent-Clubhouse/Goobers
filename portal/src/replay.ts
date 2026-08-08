import type { RunEvent, WorkflowGraph } from "./api/types";
import { eventNodeId, nodeOwner } from "./runDetailData";

export const replaySpeeds = [1, 5, 10] as const;
export type ReplaySpeed = (typeof replaySpeeds)[number];

export const idleCompressionThresholdMs = 3_000;
export const compressedIdleDelayMs = 1_500;
const minimumEventDelayMs = 250;

export interface ReplayTransition {
  realDelayMs: number;
  playbackDelayMs: number;
  idleCompressed: boolean;
}

export type ReplayChapterKind =
  | "transition"
  | "decision"
  | "failure"
  | "escalation"
  | "external"
  | "terminal";

export interface ReplayTimelinePoint {
  event: RunEvent;
  index: number;
  compressedOffsetMs: number;
  realOffsetMs: number;
  percent: number;
}

export interface ReplayChapter extends ReplayTimelinePoint {
  kind: ReplayChapterKind;
}

export interface ReplayIdleGap {
  fromSeq: number;
  toSeq: number;
  realDelayMs: number;
  compressedDelayMs: number;
  startPercent: number;
  endPercent: number;
}

// ReplayStageSegment is a contiguous run of timeline points attributed to the
// same workflow stage/gate node — the run's own hierarchy (stage, and the
// goober that owns it) rendered as a band beneath the chapter markers instead
// of the flat, same-weight marker list #2538 reports.
export interface ReplayStageSegment {
  key: string;
  stageId: string;
  label: string;
  owner?: string;
  /** Stable per stage id (first-appearance order), so a repassed stage's segments always share a color. */
  colorIndex: number;
  startPercent: number;
  endPercent: number;
}

export interface ReplayTimeline {
  events: RunEvent[];
  points: ReplayTimelinePoint[];
  chapters: ReplayChapter[];
  idleGaps: ReplayIdleGap[];
  stageSegments: ReplayStageSegment[];
  compressedDurationMs: number;
  realDurationMs: number;
}

// orderedReplayEvents returns the events in durable-sequence order. Live events
// can arrive out of order (branches, reconnect backfill); replay always plays
// the canonical sequence.
export function orderedReplayEvents(events: RunEvent[]): RunEvent[] {
  const ordered = [...events].sort((left, right) => left.seq - right.seq);
  for (let index = 1; index < ordered.length; index += 1) {
    if (ordered[index - 1].seq === ordered[index].seq) {
      throw new Error(`Duplicate durable event sequence: ${ordered[index].seq}`);
    }
  }
  return ordered;
}

function eventMillis(event: RunEvent): number {
  const value = Date.parse(event.time);
  return Number.isFinite(value) ? value : 0;
}

// replayTransition computes the wait before advancing from the event at
// currentIndex to the next one. Real wall-clock gaps come from the events'
// durable timestamps (the live RunEvent contract, not the old fixture
// `.elapsed` string); long idle gaps are compressed, and every step honors a
// minimum so bursts stay legible.
export function replayTransition(
  events: RunEvent[],
  currentIndex: number,
  speed: ReplaySpeed,
): ReplayTransition | undefined {
  const current = events[currentIndex];
  const next = events[currentIndex + 1];
  if (!current || !next) {
    return undefined;
  }

  const realDelayMs = Math.max(0, eventMillis(next) - eventMillis(current));
  const idleCompressed = realDelayMs > idleCompressionThresholdMs;
  const baseDelayMs = idleCompressed
    ? compressedIdleDelayMs
    : Math.max(realDelayMs, minimumEventDelayMs);

  return {
    realDelayMs,
    playbackDelayMs: baseDelayMs / speed,
    idleCompressed,
  };
}

export function replayTimeline(
  events: RunEvent[],
  graph?: WorkflowGraph,
  runId?: string,
): ReplayTimeline {
  const ordered = orderedReplayEvents(events);
  const offsets: Array<{ compressed: number; real: number }> = [];
  const idleOffsets: Array<{
    fromSeq: number;
    toSeq: number;
    realDelayMs: number;
    compressedDelayMs: number;
    start: number;
    end: number;
  }> = [];
  let compressedOffsetMs = 0;
  let realOffsetMs = 0;

  for (let index = 0; index < ordered.length; index += 1) {
    if (index > 0) {
      const transition = replayTransition(ordered, index - 1, 1);
      if (transition) {
        const start = compressedOffsetMs;
        compressedOffsetMs += transition.playbackDelayMs;
        realOffsetMs += transition.realDelayMs;
        if (transition.idleCompressed) {
          idleOffsets.push({
            fromSeq: ordered[index - 1].seq,
            toSeq: ordered[index].seq,
            realDelayMs: transition.realDelayMs,
            compressedDelayMs: transition.playbackDelayMs,
            start,
            end: compressedOffsetMs,
          });
        }
      }
    }
    offsets.push({ compressed: compressedOffsetMs, real: realOffsetMs });
  }

  const percentAt = (offset: number) =>
    compressedOffsetMs === 0 ? 0 : (offset / compressedOffsetMs) * 100;
  const points = ordered.map<ReplayTimelinePoint>((event, index) => ({
    event,
    index,
    compressedOffsetMs: offsets[index].compressed,
    realOffsetMs: offsets[index].real,
    percent: percentAt(offsets[index].compressed),
  }));

  return {
    events: ordered,
    points,
    chapters: points
      .filter((point) => point.event.replayChapter === true)
      .map((point) => ({ ...point, kind: replayChapterKind(point.event) })),
    idleGaps: idleOffsets.map((gap) => ({
      fromSeq: gap.fromSeq,
      toSeq: gap.toSeq,
      realDelayMs: gap.realDelayMs,
      compressedDelayMs: gap.compressedDelayMs,
      startPercent: percentAt(gap.start),
      endPercent: percentAt(gap.end),
    })),
    stageSegments: replayStageSegments(points, graph, runId),
    compressedDurationMs: compressedOffsetMs,
    realDurationMs: realOffsetMs,
  };
}

// replayStageSegments groups consecutive timeline points by the run's own
// stage/gate node id, carrying the last-known node forward across events that
// don't name one directly (evidence, liveness) — the same attribution
// eventNodeAtSequence uses for the graph and journal. Points before the run's
// first stage-bearing event (run.started and the like) carry no stage id and
// are intentionally left uncovered by any segment: there is no stage yet to
// attribute them to.
function replayStageSegments(
  points: ReplayTimelinePoint[],
  graph: WorkflowGraph | undefined,
  runId: string | undefined,
): ReplayStageSegment[] {
  const runs: Array<{ stageId: string; startIndex: number; endIndex: number }> = [];
  let activeStageId: string | undefined;

  points.forEach((point, index) => {
    activeStageId = eventNodeId(point.event, runId) ?? activeStageId;
    if (!activeStageId) {
      return;
    }
    const current = runs.at(-1);
    if (current && current.stageId === activeStageId) {
      current.endIndex = index;
    } else {
      runs.push({ stageId: activeStageId, startIndex: index, endIndex: index });
    }
  });

  // Colors key off the stage id's first appearance, not the segment's
  // position, so a repassed stage's second visit matches its first instead of
  // drifting to whatever color that position in the list lands on.
  const colorIndexByStage = new Map<string, number>();
  for (const run of runs) {
    if (!colorIndexByStage.has(run.stageId)) {
      colorIndexByStage.set(run.stageId, colorIndexByStage.size);
    }
  }

  return runs.map((run) => ({
    key: `${run.stageId}-${run.startIndex}`,
    stageId: run.stageId,
    label: run.stageId,
    owner: nodeOwner(graph, run.stageId),
    colorIndex: colorIndexByStage.get(run.stageId) ?? 0,
    startPercent: points[run.startIndex].percent,
    endPercent: points[run.endIndex].percent,
  }));
}

export function replayChapterKind(event: RunEvent): ReplayChapterKind {
  if (event.type === "run.finished") {
    return "terminal";
  }
  if (event.type === "ref.touched" && event.externalRef?.kind === "pr") {
    return "external";
  }
  if (event.escalated === true) {
    return "escalation";
  }
  if (
    event.type === "error" ||
    event.status === "failure" ||
    event.status === "failed"
  ) {
    return "failure";
  }
  if (event.category === "decision") {
    return "decision";
  }
  return "transition";
}

export function formatReplayDuration(milliseconds: number): string {
  if (milliseconds < 1_000) {
    return `${milliseconds}ms`;
  }
  if (milliseconds < 60_000) {
    const seconds = milliseconds / 1_000;
    return `${Number.isInteger(seconds) ? seconds : seconds.toFixed(1)}s`;
  }

  const totalSeconds = Math.round(milliseconds / 1_000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  return seconds === 0 ? `${minutes}m` : `${minutes}m ${seconds}s`;
}

export function formatReplayClock(milliseconds: number): string {
  const totalSeconds = Math.max(0, Math.floor(milliseconds / 1_000));
  const hours = Math.floor(totalSeconds / 3_600);
  const minutes = Math.floor((totalSeconds % 3_600) / 60);
  const seconds = totalSeconds % 60;
  return hours > 0
    ? `${hours}:${String(minutes).padStart(2, "0")}:${String(seconds).padStart(2, "0")}`
    : `${minutes}:${String(seconds).padStart(2, "0")}`;
}
