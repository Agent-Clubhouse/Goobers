import { describe, expect, it } from "vitest";
import { ScopedRequests } from "./scopedRequest";

describe("ScopedRequests", () => {
  it("aborts everything in flight when the scope is cancelled", () => {
    const requests = new ScopedRequests();
    const first = requests.begin();
    const second = requests.begin();

    requests.cancelScope();

    expect(first.signal.aborted).toBe(true);
    expect(second.signal.aborted).toBe(true);
    expect(first.obsolete).toBe(true);
    expect(second.obsolete).toBe(true);
  });

  it("rejects a pass that started before the cancellation even without an abort", () => {
    const requests = new ScopedRequests();
    const pending = requests.begin();
    // A response that had already resolved is past its abort check: the
    // generation stamp is the only thing left that can reject it.
    pending.end();

    requests.cancelScope();

    expect(pending.signal.aborted).toBe(false);
    expect(pending.obsolete).toBe(true);
  });

  it("leaves a pass started after the cancellation alone", () => {
    const requests = new ScopedRequests();
    requests.begin();
    requests.cancelScope();

    const current = requests.begin();

    expect(current.obsolete).toBe(false);
    expect(current.signal.aborted).toBe(false);
  });

  it("chains an outer scope signal through to the pass", () => {
    const requests = new ScopedRequests();
    const scope = new AbortController();
    const pending = requests.begin(scope.signal);

    expect(pending.obsolete).toBe(false);
    scope.abort();

    expect(pending.signal.aborted).toBe(true);
    expect(pending.obsolete).toBe(true);
  });

  it("begins already-aborted when the outer scope is gone", () => {
    const requests = new ScopedRequests();
    const scope = new AbortController();
    scope.abort();

    expect(requests.begin(scope.signal).obsolete).toBe(true);
  });

  it("aborts one pass without disturbing its siblings", () => {
    const requests = new ScopedRequests();
    const first = requests.begin();
    const second = requests.begin();

    first.abort();

    expect(first.obsolete).toBe(true);
    expect(second.obsolete).toBe(false);
  });

  it("stops tracking an ended pass so a later cancellation cannot revive it", () => {
    const requests = new ScopedRequests();
    const pending = requests.begin();
    pending.end();
    pending.end();

    requests.cancelScope();

    expect(pending.signal.aborted).toBe(false);
  });
});
