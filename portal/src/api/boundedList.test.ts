import { describe, expect, it } from "vitest";
import {
  applyRefresh,
  appendPage,
  compareOrder,
  emptyWindow,
  invalidationTouchesScope,
  type ListWindow,
} from "./boundedList";

interface Run {
  id: string;
  startedAt: string;
  phase: string;
}

const orderOf = (run: Run) => ({ startedAt: run.startedAt, id: run.id });

function run(id: string, startedAt: string, phase = "running"): Run {
  return { id, startedAt, phase };
}

/** A window three pages deep, as an operator who has paged would have. */
function deepWindow(): ListWindow<Run> {
  return {
    items: [
      run("r9", "2026-07-29T09:00:00Z"),
      run("r8", "2026-07-29T08:00:00Z"),
      run("r7", "2026-07-29T07:00:00Z"),
      run("r6", "2026-07-29T06:00:00Z"),
      run("r5", "2026-07-29T05:00:00Z"),
    ],
    nextCursor: "cursor-page-4",
    hasMore: true,
    position: { epoch: "e1", appliedSeq: 100 },
  };
}

describe("applyRefresh (#1713: paging survives an arriving event)", () => {
  it("keeps every loaded page when an event arrives", () => {
    const before = deepWindow();
    // The refresh fetches only the head page.
    const after = applyRefresh(
      before,
      {
        items: [run("r9", "2026-07-29T09:00:00Z", "completed")],
        nextCursor: "cursor-page-2",
        hasMore: true,
      },
      { epoch: "e1", appliedSeq: 101 },
      { orderOf },
    );

    // This is the assertion #1713 is about: five rows in, five rows out. The
    // old refresh() reset the cursor and snapped back to the first page.
    expect(after.items).toHaveLength(5);
    expect(after.items.map((r) => r.id)).toEqual(["r9", "r8", "r7", "r6", "r5"]);
    // And the cursor still points past page THREE, not past the head page.
    expect(after.nextCursor).toBe("cursor-page-4");
    expect(after.hasMore).toBe(true);
  });

  it("patches a changed row in place without moving it", () => {
    const after = applyRefresh(
      deepWindow(),
      {
        items: [run("r7", "2026-07-29T07:00:00Z", "failed")],
        nextCursor: "x",
        hasMore: true,
      },
      { epoch: "e1", appliedSeq: 101 },
      { orderOf },
    );
    expect(after.items[2]).toEqual(run("r7", "2026-07-29T07:00:00Z", "failed"));
    expect(after.items.map((r) => r.id)).toEqual(["r9", "r8", "r7", "r6", "r5"]);
  });

  it("prepends a genuinely new run at the head", () => {
    const after = applyRefresh(
      deepWindow(),
      { items: [run("r10", "2026-07-29T10:00:00Z")], nextCursor: "x", hasMore: true },
      { epoch: "e1", appliedSeq: 101 },
      { orderOf },
    );
    expect(after.items.map((r) => r.id)).toEqual(["r10", "r9", "r8", "r7", "r6", "r5"]);
  });

  it("keeps new rows in server order when several arrive at once", () => {
    const after = applyRefresh(
      deepWindow(),
      {
        items: [run("r10", "2026-07-29T10:00:00Z"), run("r11", "2026-07-29T11:00:00Z")],
        nextCursor: "x",
        hasMore: true,
      },
      { epoch: "e1", appliedSeq: 101 },
      { orderOf },
    );
    // Newest first. Blindly unshifting in arrival order would put r10 above r11
    // and the next cursor would no longer continue the sequence.
    expect(after.items.slice(0, 2).map((r) => r.id)).toEqual(["r11", "r10"]);
  });

  it("does not delete window rows just because the head page omits them", () => {
    // r5..r8 are older than the head page boundary. Their absence is expected,
    // not a deletion — treating it as one is the original bug in disguise.
    const after = applyRefresh(
      deepWindow(),
      { items: [run("r9", "2026-07-29T09:00:00Z")], nextCursor: "x", hasMore: true },
      { epoch: "e1", appliedSeq: 101 },
      { orderOf },
    );
    expect(after.items).toHaveLength(5);
  });

  it("ignores a response from behind the applied position", () => {
    const before = deepWindow();
    const after = applyRefresh(
      before,
      { items: [run("r9", "2026-07-29T09:00:00Z", "stale-data")], nextCursor: "x", hasMore: true },
      { epoch: "e1", appliedSeq: 99 },
      { orderOf },
    );
    expect(after).toBe(before);
  });

  it("snapshots on an epoch change rather than discarding the response", () => {
    // The store was rebuilt. Keeping the old window would pin the client to an
    // epoch that never produces another comparable response — a page that looks
    // healthy and never updates again.
    const after = applyRefresh(
      deepWindow(),
      { items: [run("n1", "2026-07-30T01:00:00Z")], nextCursor: "fresh", hasMore: true },
      { epoch: "e2", appliedSeq: 1 },
      { orderOf },
    );
    expect(after.items.map((r) => r.id)).toEqual(["n1"]);
    expect(after.nextCursor).toBe("fresh");
    expect(after.position).toEqual({ epoch: "e2", appliedSeq: 1 });
  });

  it("applies a response at the same position, which is a re-read not a conflict", () => {
    const after = applyRefresh(
      deepWindow(),
      { items: [run("r9", "2026-07-29T09:00:00Z", "completed")], nextCursor: "x", hasMore: true },
      { epoch: "e1", appliedSeq: 100 },
      { orderOf },
    );
    expect(after.items[0].phase).toBe("completed");
  });
});

describe("appendPage", () => {
  it("extends the window and adopts the new cursor", () => {
    const after = appendPage(
      deepWindow(),
      { items: [run("r4", "2026-07-29T04:00:00Z")], nextCursor: "cursor-page-5", hasMore: true },
      { epoch: "e1", appliedSeq: 101 },
    );
    expect(after.items).toHaveLength(6);
    expect(after.nextCursor).toBe("cursor-page-5");
  });

  it("drops rows the window already holds", () => {
    // A row can appear on two pages when rows are inserted between requests.
    // Without the guard it would render twice with the same React key.
    const after = appendPage(
      deepWindow(),
      { items: [run("r5", "2026-07-29T05:00:00Z"), run("r4", "2026-07-29T04:00:00Z")], nextCursor: "c", hasMore: false },
      undefined,
    );
    expect(after.items.map((r) => r.id)).toEqual(["r9", "r8", "r7", "r6", "r5", "r4"]);
  });
});

describe("compareOrder", () => {
  it("sorts newest first with an ascending id tiebreak", () => {
    const keys = [
      { startedAt: "2026-07-29T05:00:00Z", id: "b" },
      { startedAt: "2026-07-29T09:00:00Z", id: "a" },
      { startedAt: "2026-07-29T05:00:00Z", id: "a" },
    ];
    keys.sort(compareOrder);
    expect(keys.map((k) => `${k.startedAt}/${k.id}`)).toEqual([
      "2026-07-29T09:00:00Z/a",
      "2026-07-29T05:00:00Z/a",
      "2026-07-29T05:00:00Z/b",
    ]);
  });
});

describe("invalidationTouchesScope", () => {
  const ids = new Set(["r9", "r8"]);

  it("ignores another gaggle's workflow", () => {
    expect(
      invalidationTouchesScope(
        { workflows: [{ gaggle: "beta", workflow: "wf" }] },
        { gaggle: "alpha" },
        ids,
      ),
    ).toBe(false);
  });

  it("matches this gaggle's workflow", () => {
    expect(
      invalidationTouchesScope(
        { workflows: [{ gaggle: "alpha", workflow: "wf" }] },
        { gaggle: "alpha" },
        ids,
      ),
    ).toBe(true);
  });

  it("matches a run already on screen even with no workflow reference", () => {
    expect(invalidationTouchesScope({ runIds: ["r9"] }, { gaggle: "alpha" }, ids)).toBe(true);
  });

  it("ignores a run that is neither on screen nor in scope", () => {
    expect(invalidationTouchesScope({ runIds: ["zzz"] }, { gaggle: "alpha" }, ids)).toBe(false);
  });

  it("refreshes on a detail-free invalidation", () => {
    // "Something changed, details unknown" — refreshing is the safe reading.
    expect(invalidationTouchesScope({}, { gaggle: "alpha" }, ids)).toBe(true);
  });

  it("refreshes an unscoped list on anything", () => {
    expect(invalidationTouchesScope({ runIds: ["zzz"] }, {}, ids)).toBe(true);
  });
});

describe("emptyWindow", () => {
  it("starts with nothing applied, so the first response is always accepted", () => {
    expect(emptyWindow<Run>().position).toBeUndefined();
  });
});
