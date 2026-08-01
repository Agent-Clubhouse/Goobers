/**
 * One loader per (runId, sourceFingerprint), shared by the run detail page's
 * three panels (#1930, absorbs #1714).
 *
 * # Two bugs, one cause
 *
 * `getRun` and `listRunEvents` independently reparsed the same journal
 * (`runDetailData.ts:149`), so opening a run detail page parsed the same file
 * twice — three times once stage attempts loaded. And stage attempts never
 * refreshed on a live run (#1714): the summary and the event ledger subscribed
 * to invalidations, the attempts panel did not, so a running stage's attempt
 * count froze at whatever it was when the page opened.
 *
 * Both follow from each panel owning its own fetch. One entry per run, shared
 * by all three panels, fixes the duplicate parse; making that entry the single
 * subscriber fixes the frozen panel, because there is now exactly one place
 * that decides when a run's data is refetched.
 *
 * # Why the fingerprint is part of the key
 *
 * A run's data is immutable for a given source position. Keying only by runId
 * would make a completed run's cached entry serve forever, which is right, and
 * a running run's entry serve forever, which is wrong. Keying by (runId,
 * fingerprint) means a live run gets a new entry each time its journal advances
 * and a finished run keeps one — no special-casing of "is it live".
 */

/** A run's source position, as the read contract reports it (§15.3). This is
 * the SOURCE identity — `(runID, journalSeq)` — not the projection one. */
export interface SourceFingerprint {
  runId: string;
  journalSeq: number;
}

export function fingerprintKey(runId: string, fingerprint: SourceFingerprint | undefined): string {
  // A run with no reported fingerprint gets a key that never matches a later
  // one, so it refetches rather than serving unknown-age data indefinitely.
  return fingerprint ? `${runId}@${fingerprint.journalSeq}` : `${runId}@unknown`;
}

/** What one shared entry holds. Panels read from this; none of them fetch. */
export interface RunEntry<S, E, A> {
  key: string;
  runId: string;
  summary: S | undefined;
  events: E | undefined;
  attempts: A | undefined;
  /** How many panels are currently reading this entry. */
  readers: number;
  /** How many times the underlying source was loaded for this key. The
   * duplicate-parse assertion reads this. */
  loads: number;
}

/**
 * A per-run entry store with reference counting.
 *
 * Reference-counted rather than TTL'd: the lifetime that matters is "is any
 * panel showing this run", which the panels know exactly and a timer can only
 * guess at. A TTL would either evict while a panel was still rendering, or
 * retain every run the operator ever opened.
 */
export class RunEntryStore<S, E, A> {
  private readonly entries = new Map<string, RunEntry<S, E, A>>();

  /** Claim an entry for a reader, creating it if absent. */
  acquire(runId: string, fingerprint: SourceFingerprint | undefined): RunEntry<S, E, A> {
    const key = fingerprintKey(runId, fingerprint);
    let entry = this.entries.get(key);
    if (!entry) {
      entry = {
        key,
        runId,
        summary: undefined,
        events: undefined,
        attempts: undefined,
        readers: 0,
        loads: 0,
      };
      this.entries.set(key, entry);
    }
    entry.readers += 1;
    return entry;
  }

  /** Release a reader's claim, dropping the entry when the last one leaves. */
  release(entry: RunEntry<S, E, A>): void {
    entry.readers -= 1;
    if (entry.readers <= 0) {
      this.entries.delete(entry.key);
    }
  }

  /**
   * Retire every entry for a run whose fingerprint has advanced.
   *
   * Called when a live run's journal moves. The old entries are dropped so the
   * next acquire loads fresh data — which is the mechanism that makes the stage
   * attempts panel refresh on a live run (#1714) without it needing its own
   * subscription.
   */
  retireStale(runId: string, current: SourceFingerprint | undefined): string[] {
    const keep = fingerprintKey(runId, current);
    const retired: string[] = [];
    for (const [key, entry] of this.entries) {
      if (entry.runId === runId && key !== keep) {
        this.entries.delete(key);
        retired.push(key);
      }
    }
    return retired;
  }

  get(runId: string, fingerprint: SourceFingerprint | undefined): RunEntry<S, E, A> | undefined {
    return this.entries.get(fingerprintKey(runId, fingerprint));
  }

  get size(): number {
    return this.entries.size;
  }
}

/**
 * Whether a live-run invalidation should retire this run's entry.
 *
 * An invalidation naming the run always does. One that names no runs at all
 * does too — "something changed, details unknown" — because a stage attempt
 * that silently stops updating is worse than an extra fetch.
 */
export function invalidationTouchesRun(
  invalidation: { runIds?: string[] },
  runId: string,
): boolean {
  if (!invalidation.runIds?.length) return true;
  return invalidation.runIds.includes(runId);
}
