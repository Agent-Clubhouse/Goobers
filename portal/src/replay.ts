import type { RunEvent } from "./fixtures";

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

export function orderedReplayEvents(events: RunEvent[]): RunEvent[] {
  const ordered = [...events].sort((left, right) => left.seq - right.seq);
  for (let index = 1; index < ordered.length; index += 1) {
    if (ordered[index - 1].seq === ordered[index].seq) {
      throw new Error(`Duplicate durable event sequence: ${ordered[index].seq}`);
    }
  }
  return ordered;
}

export function parseElapsedMs(elapsed: string): number {
  const parts = elapsed.split(":");
  if (parts.length < 2 || parts.length > 3 || parts.some((part) => !/^\d+$/.test(part))) {
    throw new Error(`Invalid elapsed time: ${elapsed}`);
  }

  const values = parts.map(Number);
  const seconds = values[values.length - 1];
  const minutes = values[values.length - 2];
  const hours = values.length === 3 ? values[0] : 0;
  if (seconds > 59 || (parts.length === 3 && minutes > 59)) {
    throw new Error(`Invalid elapsed time: ${elapsed}`);
  }
  return ((hours * 60 + minutes) * 60 + seconds) * 1_000;
}

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

  const realDelayMs = parseElapsedMs(next.elapsed) - parseElapsedMs(current.elapsed);
  if (realDelayMs < 0) {
    throw new Error(`Elapsed time moved backwards between event sequences ${current.seq} and ${next.seq}`);
  }

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
