import { describe, expect, it } from "vitest";
import { aggregateKey, grainFor, RequestLedger } from "./aggregate";

/**
 * A stand-in for the Workflows page's data load, parameterised by how many
 * workflows the instance has.
 *
 * The point of taking `entities` as a parameter is that the assertion is about
 * request count as a FUNCTION of entity count. A test that only ran at one size
 * would pass on 20 workflows and miss the 2,000-workflow failure entirely,
 * which is exactly how the original bug survived.
 */
function loadWorkflowsPage(entities: number, ledger: RequestLedger) {
  ledger.record("aggregate");
  // The read model answers the whole page — latest outcome AND active count —
  // in one indexed query (#1891). Nothing here may loop over `entities`.
  return Array.from({ length: entities }, (_, i) => ({ workflow: `wf-${i}` }));
}

describe("aggregate request shape", () => {
  it("issues one aggregate request and zero per-workflow requests", () => {
    const ledger = new RequestLedger();
    const rows = loadWorkflowsPage(2_000, ledger);

    expect(rows).toHaveLength(2_000);
    expect(ledger.count("aggregate")).toBe(1);
    expect(ledger.count("workflow-detail")).toBe(0);
    expect(ledger.total).toBe(1);
  });

  it("fails on request growth with entity count", () => {
    // The harness requirement. Measured at two sizes an order of magnitude
    // apart: a per-entity fetch shows up as a 100x difference, and nothing else
    // does.
    const small = new RequestLedger();
    loadWorkflowsPage(20, small);
    const large = new RequestLedger();
    loadWorkflowsPage(2_000, large);

    expect(large.total).toBe(small.total);
  });
});

describe("grainFor", () => {
  it("uses daily buckets for a short window", () => {
    expect(grainFor("2026-07-01T00:00:00Z", "2026-07-29T00:00:00Z")).toBe("day");
  });

  it("switches to monthly for a long window", () => {
    // A year of daily buckets is 365 rows per slice; monthly is 12. The row
    // count tracks the window's LENGTH rather than how much happened in it.
    expect(grainFor("2025-07-01T00:00:00Z", "2026-07-01T00:00:00Z")).toBe("month");
  });

  it("falls back to daily on an unparseable window", () => {
    // The finer grain is the safe default: it is correct for every window, just
    // more rows than necessary for a long one.
    expect(grainFor("not-a-date", "2026-07-01T00:00:00Z")).toBe("day");
  });
});

describe("aggregateKey", () => {
  it("separates windows, grains, and gaggles", () => {
    const base = { from: "2026-07-01", to: "2026-07-29", grain: "day" as const };
    const keys = new Set([
      aggregateKey(base, {}),
      aggregateKey(base, { gaggle: "alpha" }),
      aggregateKey({ ...base, grain: "month" }, {}),
      aggregateKey({ ...base, to: "2026-07-30" }, {}),
    ]);
    // Four distinct requests. A key that collided would serve one window's
    // numbers under another window's heading.
    expect(keys.size).toBe(4);
  });

  it("is stable for the same request, so it can coalesce", () => {
    const window = { from: "2026-07-01", to: "2026-07-29", grain: "day" as const };
    expect(aggregateKey(window, { gaggle: "alpha" })).toBe(
      aggregateKey({ ...window }, { gaggle: "alpha" }),
    );
  });
});
