import type { ReadState } from "./types";

/**
 * The one coalescing rule, shared by all three data primitives (#1930, §8.2).
 *
 * # What was wrong
 *
 * Every page fetched data its own way, so every page reintroduced the same
 * failure modes independently. Under a live stream at any real event rate, the
 * shared symptom was request pile-up: an invalidation arrived, a refresh
 * started, another invalidation arrived, and the first request was aborted to
 * start a second — repeatedly, so that at a high enough event rate *no request
 * ever finished*. The data went permanently stale precisely when it was
 * changing fastest.
 *
 * # The rule
 *
 * At most one in-flight and one queued refresh per family, and **a useful
 * in-flight request is never aborted merely because a newer event arrived**.
 *
 * That second half is the important one. A request already in flight will
 * return data that is at least as new as the event that would have cancelled
 * it — the event says "something changed", and the response reflects the
 * server's state at response time, which is later than the event. Cancelling it
 * to start again throws away work that was about to answer the question.
 *
 * Queued collapses to a single slot on purpose: ten invalidations arriving
 * during one in-flight request need exactly one more fetch, not ten.
 */

/** Why a refresh was requested. Carried through so a loader can tell an
 * event-driven refresh from a user-driven one. */
export type RefreshReason = "initial" | "event" | "retry" | "scope";

/** Counters the load harness asserts on. Exposed rather than inferred, because
 * "zero aborts attributable to a newer event" is not observable from outside. */
export interface FamilyStats {
  /** Requests started. */
  started: number;
  /** Requests that completed, successfully or not. */
  settled: number;
  /** Refreshes folded into an already-queued slot. */
  coalesced: number;
  /** Aborts caused by unmount or a scope change — legitimate. */
  abortsByScope: number;
  /** Aborts caused by a newer event arriving. Must stay zero. */
  abortsByNewerEvent: number;
  /** Peak simultaneous in-flight requests. Must stay <= 1. */
  peakInFlight: number;
  /** Peak queued refreshes. Must stay <= 1. */
  peakQueued: number;
}

export function emptyStats(): FamilyStats {
  return {
    started: 0,
    settled: 0,
    coalesced: 0,
    abortsByScope: 0,
    abortsByNewerEvent: 0,
    peakInFlight: 0,
    peakQueued: 0,
  };
}

/** What a loader is handed when it runs. */
export interface LoadContext {
  signal: AbortSignal;
  reason: RefreshReason;
}

/**
 * A single-flight, single-queue request family.
 *
 * Not a React hook: the primitives own one of these in a ref, so its identity
 * survives re-renders and its counters survive a state update. Making it a hook
 * would reset the queue on every render, which is how the pile-up got in.
 */
export class QueryFamily {
  readonly stats: FamilyStats = emptyStats();

  private inFlight: AbortController | undefined;
  private queued: RefreshReason | undefined;
  private closed = false;

  constructor(private readonly load: (context: LoadContext) => Promise<void>) {}

  /**
   * Request a refresh.
   *
   * Starts immediately when idle. Otherwise records that another pass is needed
   * and returns — deliberately without touching the in-flight request.
   */
  request(reason: RefreshReason): void {
    if (this.closed) return;
    if (this.inFlight) {
      // Coalesce. A "retry" or "scope" reason wins over "event" so the loader
      // can tell why the follow-up pass exists, but the SLOT is still one.
      if (this.queued === undefined || reason !== "event") {
        this.queued = reason;
      }
      this.stats.coalesced += 1;
      this.stats.peakQueued = Math.max(this.stats.peakQueued, 1);
      return;
    }
    void this.run(reason);
  }

  /**
   * Abandon the in-flight request because its scope no longer applies.
   *
   * This is the ONLY legitimate abort: the filters changed, or the component
   * unmounted, so the response would answer a question nobody is asking. It is
   * counted separately from event-driven aborts precisely so the two cannot be
   * confused when reading the stats.
   */
  cancelForScope(): void {
    if (this.inFlight) {
      this.stats.abortsByScope += 1;
      this.inFlight.abort();
      this.inFlight = undefined;
    }
    this.queued = undefined;
  }

  /** Stop accepting work. Idempotent. */
  close(): void {
    this.closed = true;
    this.cancelForScope();
  }

  /** Whether a request is currently running. */
  get busy(): boolean {
    return this.inFlight !== undefined;
  }

  private async run(reason: RefreshReason): Promise<void> {
    const controller = new AbortController();
    this.inFlight = controller;
    this.stats.started += 1;
    this.stats.peakInFlight = Math.max(this.stats.peakInFlight, 1);
    try {
      await this.load({ signal: controller.signal, reason });
    } catch {
      // A loader that throws must not wedge the family. The loader itself is
      // responsible for surfacing the error; here the only concern is that the
      // queue keeps draining, because a wedged family shows stale data forever
      // with no indication that it stopped trying.
    } finally {
      this.stats.settled += 1;
      // Only clear if this is still the current request. cancelForScope may
      // have already replaced or dropped it.
      if (this.inFlight === controller) {
        this.inFlight = undefined;
      }
      const next = this.queued;
      this.queued = undefined;
      if (next !== undefined && !this.closed) {
        void this.run(next);
      }
    }
  }
}

/**
 * How a response's projection position compares to what the client holds.
 *
 * The two identities are distinct (§15.3): a SOURCE position is `(runID,
 * journalSeq)`, a PROJECTION position is `<epoch>:<seq>`. This is the
 * projection one, and its epoch is OPAQUE — compared for equality, never
 * ordered. A rebuilt store mints a new epoch and its sequence numbers restart,
 * so `seq > held.seq` across an epoch boundary is meaningless.
 */
export type PositionVerdict = "newer" | "stale" | "epoch-changed" | "unknown";

export interface Position {
  epoch: string;
  appliedSeq: number;
}

export function positionOf(readState: ReadState | undefined): Position | undefined {
  if (!readState || !readState.epoch) return undefined;
  return { epoch: readState.epoch, appliedSeq: readState.appliedSeq };
}

/**
 * Compare an incoming response's position to the one already applied.
 *
 * # Why a changed epoch is not "stale"
 *
 * A differing epoch means the store was REBUILT: a new file, new sequence
 * numbering, and no relationship between the old cursor and the new one. The
 * tempting handling is to treat the response as unusable and keep what we
 * have — but that leaves the client permanently pinned to an epoch that will
 * never produce another comparable response, so it never updates again. It
 * looks exactly like a working page showing old data.
 *
 * So an epoch change forces a SNAPSHOT: drop the accumulated window and take
 * the response as the new ground truth. Losing pagination depth is a visible,
 * recoverable cost; silent permanent staleness is neither.
 */
export function comparePosition(
  incoming: Position | undefined,
  applied: Position | undefined,
): PositionVerdict {
  if (!incoming) return "unknown";
  if (!applied) return "newer";
  if (incoming.epoch !== applied.epoch) return "epoch-changed";
  return incoming.appliedSeq >= applied.appliedSeq ? "newer" : "stale";
}
