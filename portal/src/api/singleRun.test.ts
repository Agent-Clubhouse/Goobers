import { describe, expect, it } from "vitest";
import {
  fingerprintKey,
  invalidationTouchesRun,
  RunEntryStore,
  type SourceFingerprint,
} from "./singleRun";

type Store = RunEntryStore<string, string[], string[]>;

const at = (journalSeq: number): SourceFingerprint => ({ runId: "run-1", journalSeq });

/** Stands in for the three panels of the run detail page. */
function openPanels(store: Store, runId: string, fingerprint: SourceFingerprint | undefined) {
  const summary = store.acquire(runId, fingerprint);
  const events = store.acquire(runId, fingerprint);
  const attempts = store.acquire(runId, fingerprint);
  return { summary, events, attempts };
}

describe("RunEntryStore (one loader per run)", () => {
  it("parses an unchanged journal at most once across all three panels", () => {
    const store: Store = new RunEntryStore();
    const panels = openPanels(store, "run-1", at(42));

    // The acceptance criterion: summary, event ledger, and stage attempts are
    // three views of ONE entry, not three fetches of one journal.
    expect(panels.summary).toBe(panels.events);
    expect(panels.events).toBe(panels.attempts);
    expect(store.size).toBe(1);

    // Whoever loads it, loads it once.
    panels.summary.loads += 1;
    expect(panels.attempts.loads).toBe(1);
  });

  it("keeps the entry alive until the last panel closes", () => {
    const store: Store = new RunEntryStore();
    const panels = openPanels(store, "run-1", at(42));

    store.release(panels.summary);
    store.release(panels.events);
    expect(store.size).toBe(1);

    store.release(panels.attempts);
    expect(store.size).toBe(0);
  });

  it("gives a live run a new entry when its journal advances (#1714)", () => {
    // The bug: stage attempts never refreshed on a live run, because that panel
    // had no subscription of its own. Keying by source position means the
    // attempts panel gets fresh data for free when the journal moves — there is
    // nothing for it to forget to subscribe to.
    const store: Store = new RunEntryStore();
    const first = store.acquire("run-1", at(42));
    first.attempts = ["attempt-1"];

    const second = store.acquire("run-1", at(43));
    expect(second).not.toBe(first);
    expect(second.attempts).toBeUndefined();
  });

  it("serves a finished run from one entry indefinitely", () => {
    // A completed run's journal does not advance, so its fingerprint is stable
    // and its entry keeps serving. No "is it live" special case is needed.
    const store: Store = new RunEntryStore();
    const a = store.acquire("run-1", at(99));
    const b = store.acquire("run-1", at(99));
    expect(b).toBe(a);
  });

  it("retires stale entries when a run advances, leaving the current one", () => {
    const store: Store = new RunEntryStore();
    store.acquire("run-1", at(42));
    store.acquire("run-1", at(43));
    store.acquire("run-2", at(1));

    const retired = store.retireStale("run-1", at(43));
    expect(retired).toEqual([fingerprintKey("run-1", at(42))]);
    expect(store.get("run-1", at(43))).toBeDefined();
    // Another run's entry is untouched.
    expect(store.get("run-2", at(1))).toBeDefined();
  });

  it("refetches a run whose fingerprint is unknown rather than serving stale data", () => {
    // An unknown fingerprint must not collide with a known one, or a run served
    // before the read state arrived would be cached forever at unknown age.
    expect(fingerprintKey("run-1", undefined)).not.toBe(fingerprintKey("run-1", at(0)));
    const store: Store = new RunEntryStore();
    const unknown = store.acquire("run-1", undefined);
    const known = store.acquire("run-1", at(1));
    expect(known).not.toBe(unknown);
  });
});

describe("invalidationTouchesRun", () => {
  it("matches an invalidation naming the run", () => {
    expect(invalidationTouchesRun({ runIds: ["run-1", "run-2"] }, "run-1")).toBe(true);
  });

  it("ignores an invalidation naming only other runs", () => {
    expect(invalidationTouchesRun({ runIds: ["run-2"] }, "run-1")).toBe(false);
  });

  it("refreshes on a detail-free invalidation", () => {
    // A stage attempt that silently stops updating is worse than an extra fetch.
    expect(invalidationTouchesRun({}, "run-1")).toBe(true);
  });
});
