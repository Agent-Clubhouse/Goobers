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
