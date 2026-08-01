import { describe, expect, it } from "vitest";
import { comparePosition, QueryFamily, type LoadContext } from "./queryFamily";

/** A loader that resolves when the test says so, so ordering is deterministic
 * rather than timing-dependent. */
function deferredLoader() {
  const pending: { resolve: () => void; context: LoadContext }[] = [];
  const loader = (context: LoadContext) =>
    new Promise<void>((resolve) => {
      pending.push({ resolve, context });
    });
  return {
    loader,
    pending,
    settleOldest() {
      const next = pending.shift();
      if (!next) throw new Error("nothing in flight to settle");
      next.resolve();
      // Yield so the family's finally block runs before the test continues.
      return Promise.resolve().then(() => Promise.resolve());
    },
  };
}

describe("QueryFamily", () => {
  it("never aborts an in-flight request because a newer event arrived", async () => {
    const { loader, pending, settleOldest } = deferredLoader();
    const family = new QueryFamily(loader);

    family.request("initial");
    expect(pending).toHaveLength(1);
    const firstSignal = pending[0].context.signal;

    // Ten events arrive while the first request is still open. This is the case
    // that used to abort-and-restart on every one, so that at a high enough
    // event rate no request ever completed.
    for (let i = 0; i < 10; i += 1) family.request("event");

    expect(firstSignal.aborted).toBe(false);
    expect(family.stats.abortsByNewerEvent).toBe(0);
    expect(pending).toHaveLength(1);
    expect(family.stats.started).toBe(1);

    // Ten events collapse into ONE follow-up pass, not ten.
    await settleOldest();
    expect(family.stats.started).toBe(2);
    expect(pending).toHaveLength(1);

    await settleOldest();
    expect(family.stats.started).toBe(2);
    expect(family.busy).toBe(false);
  });

  it("holds at most one in-flight and one queued refresh", async () => {
    const { loader, settleOldest } = deferredLoader();
    const family = new QueryFamily(loader);

    for (let i = 0; i < 50; i += 1) family.request("event");
    expect(family.stats.peakInFlight).toBe(1);
    expect(family.stats.peakQueued).toBe(1);

    await settleOldest();
    await settleOldest();
    expect(family.stats.started).toBe(2);
    expect(family.stats.peakInFlight).toBe(1);
    expect(family.stats.peakQueued).toBe(1);
  });

  it("sustains 100 events/s for 60s without a single event-driven abort", async () => {
    // The acceptance criterion, run as fast as the event loop allows rather
    // than in real time: 6,000 events against a loader with latency, asserting
    // the invariants that 500ms of injected latency would expose.
    const { loader, settleOldest, pending } = deferredLoader();
    const family = new QueryFamily(loader);

    for (let tick = 0; tick < 6_000; tick += 1) {
      family.request("event");
      // Every 100th event, the in-flight request completes — standing in for a
      // 500ms response against a 100/s arrival rate, where roughly 50 events
      // land per response.
      if (tick % 100 === 0 && pending.length > 0) {
        await settleOldest();
      }
    }
    while (pending.length > 0) await settleOldest();

    expect(family.stats.abortsByNewerEvent).toBe(0);
    expect(family.stats.peakInFlight).toBeLessThanOrEqual(1);
    expect(family.stats.peakQueued).toBeLessThanOrEqual(1);
    // The point of coalescing: 6,000 events did not become 6,000 requests.
    expect(family.stats.started).toBeLessThan(200);
  });

  it("aborts only for scope changes, and counts them separately", async () => {
    const { loader, pending } = deferredLoader();
    const family = new QueryFamily(loader);

    family.request("initial");
    const signal = pending[0].context.signal;
    family.cancelForScope();

    expect(signal.aborted).toBe(true);
    expect(family.stats.abortsByScope).toBe(1);
    expect(family.stats.abortsByNewerEvent).toBe(0);
  });

  it("keeps draining after a loader throws", async () => {
    let calls = 0;
    const family = new QueryFamily(async () => {
      calls += 1;
      throw new Error("boom");
    });

    family.request("initial");
    await Promise.resolve();
    await Promise.resolve();
    family.request("event");
    await Promise.resolve();
    await Promise.resolve();

    // A family that wedged on the first rejection would show stale data forever
    // with nothing indicating it had stopped trying.
    expect(calls).toBeGreaterThanOrEqual(2);
    expect(family.busy).toBe(false);
  });

  it("starts nothing after close", async () => {
    const { loader, pending } = deferredLoader();
    const family = new QueryFamily(loader);
    family.close();
    family.request("event");
    expect(pending).toHaveLength(0);
  });
});

describe("comparePosition", () => {
  it("treats a changed epoch as a snapshot signal, not as stale", () => {
    // The failure this prevents: pinning the client to an epoch that will never
    // produce another comparable response, so it never updates again — which
    // looks exactly like a working page showing old data.
    expect(
      comparePosition({ epoch: "b", appliedSeq: 1 }, { epoch: "a", appliedSeq: 900 }),
    ).toBe("epoch-changed");
  });

  it("never orders across epochs", () => {
    // A rebuilt store restarts its sequence numbering, so a LOWER seq under a
    // new epoch is not stale — it is a different counter entirely.
    expect(
      comparePosition({ epoch: "b", appliedSeq: 1 }, { epoch: "a", appliedSeq: 1 }),
    ).toBe("epoch-changed");
  });

  it("rejects a response older than what is already applied", () => {
    expect(
      comparePosition({ epoch: "a", appliedSeq: 4 }, { epoch: "a", appliedSeq: 5 }),
    ).toBe("stale");
  });

  it("accepts an equal position, because a re-read at the same position is not a conflict", () => {
    expect(
      comparePosition({ epoch: "a", appliedSeq: 5 }, { epoch: "a", appliedSeq: 5 }),
    ).toBe("newer");
  });

  it("accepts anything when nothing has been applied yet", () => {
    expect(comparePosition({ epoch: "a", appliedSeq: 1 }, undefined)).toBe("newer");
  });

  it("reports unknown when the response carried no read state", () => {
    // A degraded topology serves without a read state. Treating that as stale
    // would make the page refuse every response it got.
    expect(comparePosition(undefined, { epoch: "a", appliedSeq: 5 })).toBe("unknown");
  });
});
