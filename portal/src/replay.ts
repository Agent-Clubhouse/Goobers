import type { RunEvent } from "./api/types";

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

export interface ReplayTimeline {
  events: RunEvent[];
  points: ReplayTimelinePoint[];
  chapters: ReplayChapter[];
  idleGaps: ReplayIdleGap[];
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

export function replayTimeline(events: RunEvent[]): ReplayTimeline {
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
    compressedDurationMs: compressedOffsetMs,
    realDurationMs: realOffsetMs,
  };
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
