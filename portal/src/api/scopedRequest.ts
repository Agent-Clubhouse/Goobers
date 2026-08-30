/**
 * Abort controllers for background refreshes, bound to the scope that asked
 * for them (#3656).
 *
 * A background refresh is started from a live event, not from a render, so
 * nothing else in the hook holds its controller: the filter can change, or the
 * route can, while the request is still in flight. Left unowned, that request
 * comes back later and merges an old filter's rows into the window the user is
 * now looking at — the newer, correct response having already been applied.
 *
 * Two guards, because either alone leaks:
 *
 *   - every request started under a scope is aborted when that scope tears
 *     down, so the daemon stops doing work nobody wants; and
 *   - a generation stamp rejects a completion that had already resolved when
 *     the teardown happened, which no abort can catch — `signal.aborted` is
 *     false for a promise that is merely waiting for its `.then` to run.
 */
export interface ScopedRequest {
  /** Signal to pass to every request made for this pass. */
  readonly signal: AbortSignal;
  /**
   * Whether this pass may still publish. Check it after every await, before
   * touching shared state: an obsolete pass must drop its result silently, not
   * merge it.
   */
  readonly obsolete: boolean;
  /** Abandon this pass alone, without disturbing the rest of the scope. */
  abort(): void;
  /** Release the pass. Idempotent. */
  end(): void;
}

export class ScopedRequests {
  private generation = 0;
  private readonly active = new Set<AbortController>();

  /**
   * Start a pass. When `scopeSignal` is given (a QueryFamily's, for instance)
   * its abort is chained through, so the family's own scope cancellation
   * reaches the requests the loader actually issued.
   */
  begin(scopeSignal?: AbortSignal): ScopedRequest {
    const controller = new AbortController();
    const generation = this.generation;
    this.active.add(controller);
    const abort = () => controller.abort();
    if (scopeSignal) {
      if (scopeSignal.aborted) {
        controller.abort();
      } else {
        scopeSignal.addEventListener("abort", abort, { once: true });
      }
    }

    let ended = false;
    const owner = this;
    return {
      signal: controller.signal,
      get obsolete(): boolean {
        return controller.signal.aborted || generation !== owner.generation;
      },
      abort: () => {
        controller.abort();
      },
      end: () => {
        if (ended) {
          return;
        }
        ended = true;
        owner.active.delete(controller);
        scopeSignal?.removeEventListener("abort", abort);
      },
    };
  }

  /**
   * Abandon everything in flight because the scope no longer applies — a
   * filter change, a route change, or unmount. Every pass started before this
   * call is obsolete from here on, whether or not its abort landed in time.
   */
  cancelScope(): void {
    this.generation += 1;
    for (const controller of this.active) {
      controller.abort();
    }
    this.active.clear();
  }
}
