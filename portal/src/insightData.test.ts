import { describe, expect, it } from "vitest";
import {
  insightErrorSignatureFilters,
  insightPreviousWindowFilters,
  insightStatsFilters,
  insightTrendBuckets,
  insightWindowFilters,
} from "./insightData";

describe("Insight time windows", () => {
  it("pins both ends of a bounded snapshot and the end of all-time snapshots", () => {
    const now = new Date("2026-07-22T12:00:00Z");

    expect(insightWindowFilters("7d", now)).toEqual({
      since: "2026-07-15T12:00:00.000Z",
      until: "2026-07-22T12:00:00.000Z",
    });
    expect(insightWindowFilters("all", now)).toEqual({
      until: "2026-07-22T12:00:00.000Z",
    });
  });

  it("adds the selected operational scope to failure-reason queries", () => {
    const now = new Date("2026-07-22T12:00:00Z");

    expect(
      insightErrorSignatureFilters("24h", "core", "implementation", "review", now),
    ).toEqual({
      gaggle: "core",
      workflow: "implementation",
      stage: "review",
      since: "2026-07-21T12:00:00.000Z",
      until: "2026-07-22T12:00:00.000Z",
      limit: 20,
    });
  });

  it("adds the selected operational scope to stats queries", () => {
    const now = new Date("2026-07-22T12:00:00Z");

    expect(insightStatsFilters("all", "core", "implementation", now)).toEqual({
      gaggle: "core",
      workflow: "implementation",
      until: "2026-07-22T12:00:00.000Z",
    });
  });
});

describe("Insight cost trend buckets", () => {
  it("splits a 7-day window into 7 contiguous, ascending daily buckets", () => {
    const now = new Date("2026-07-22T12:00:00Z");

    const buckets = insightTrendBuckets("7d", now);

    expect(buckets).toHaveLength(7);
    expect(buckets[0].since).toBe("2026-07-15T12:00:00.000Z");
    expect(buckets[buckets.length - 1].until).toBe("2026-07-22T12:00:00.000Z");
    for (let index = 1; index < buckets.length; index += 1) {
      expect(buckets[index].since).toBe(buckets[index - 1].until);
    }
  });

  it("produces no buckets for an unbounded window", () => {
    expect(insightTrendBuckets("all", new Date("2026-07-22T12:00:00Z"))).toEqual([]);
  });

  it("computes the immediately preceding window of the same length", () => {
    const now = new Date("2026-07-22T12:00:00Z");

    expect(insightPreviousWindowFilters("24h", now)).toEqual({
      since: "2026-07-20T12:00:00.000Z",
      until: "2026-07-21T12:00:00.000Z",
    });
    expect(insightPreviousWindowFilters("7d", now)).toEqual({
      since: "2026-07-08T12:00:00.000Z",
      until: "2026-07-15T12:00:00.000Z",
    });
  });

  it("has no preceding window for an unbounded window", () => {
    expect(insightPreviousWindowFilters("all", new Date("2026-07-22T12:00:00Z"))).toBeUndefined();
  });
});
