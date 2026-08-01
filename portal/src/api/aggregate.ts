/**
 * Bucket-backed, window-parameterised aggregation (#1930, §5.6/§8.2).
 *
 * # The request-growth bug this closes
 *
 * The Workflows page issued one request PER WORKFLOW to find each one's latest
 * outcome. At 20 workflows that is invisible; at 2,000 it is 2,000 requests,
 * and the page never finishes loading. The cost grew with the entity count,
 * which means the page worked in every test and failed on exactly the instances
 * that needed it.
 *
 * The read model answers the whole page in one aggregate query (#1891), so the
 * client's job is only to ask once — and to have a test that FAILS when someone
 * reintroduces a per-entity fetch, because the regression is invisible on a
 * small instance.
 */

/** A half-open window over which an aggregate is computed. */
export interface AggregateWindow {
  from: string;
  to: string;
  /** Bucket grain. The store keeps daily and monthly rollups; asking for a
   * grain it does not keep would mean aggregating rows at read time, which is
   * the unbounded shape buckets exist to avoid. */
  grain: "day" | "month";
}

/** Identity of an aggregate request, used as a coalescing key. */
export function aggregateKey(window: AggregateWindow, scope: { gaggle?: string }): string {
  return [scope.gaggle ?? "*", window.grain, window.from, window.to].join("|");
}

/**
 * Choose the grain for a window.
 *
 * Long windows use monthly buckets so the row count stays proportional to the
 * window's LENGTH rather than to how much happened in it. A year of daily
 * buckets is 365 rows per slice; monthly is 12.
 */
export function grainFor(fromISO: string, toISO: string): "day" | "month" {
  const from = Date.parse(fromISO);
  const to = Date.parse(toISO);
  if (!Number.isFinite(from) || !Number.isFinite(to)) return "day";
  const days = (to - from) / 86_400_000;
  return days > 92 ? "month" : "day";
}

/**
 * Counts requests by kind so a test can assert the shape of a page load rather
 * than its timing.
 *
 * The acceptance criterion is "a 2,000-workflow page issues one aggregate
 * request and ZERO per-workflow requests, and the harness fails on request
 * growth with entity count". That needs the client to be observable, not fast.
 */
export class RequestLedger {
  private readonly counts = new Map<string, number>();

  record(kind: string): void {
    this.counts.set(kind, (this.counts.get(kind) ?? 0) + 1);
  }

  count(kind: string): number {
    return this.counts.get(kind) ?? 0;
  }

  get total(): number {
    let sum = 0;
    for (const n of this.counts.values()) sum += n;
    return sum;
  }

  reset(): void {
    this.counts.clear();
  }
}
