import { describe, expect, it } from "vitest";
import {
  insightErrorSignatureFilters,
  insightPreviousWindowFilters,
  serializeInsightAggregate,
  selectInsightCostTrendBuckets,
  insightTrendBuckets,
  insightWindowFilters,
} from "./insightData";
import type { DaemonClient } from "./api/types";

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
});

describe("Insight aggregate admission", () => {
  it("serializes simultaneous refreshes for one daemon client", async () => {
    const client = {} as DaemonClient;
    const signal = new AbortController().signal;
    const releases: Array<() => void> = [];
    let active = 0;
    let maximumActive = 0;
    const load = () =>
      new Promise<number>((resolve) => {
        active += 1;
        maximumActive = Math.max(maximumActive, active);
        releases.push(() => {
          active -= 1;
          resolve(active);
        });
      });

    const first = serializeInsightAggregate(client, signal, load);
    const second = serializeInsightAggregate(client, signal, load);
    await Promise.resolve();
    await Promise.resolve();

    expect(active).toBe(1);
    expect(releases).toHaveLength(1);
    releases[0]();
    await first;
    await Promise.resolve();
    await Promise.resolve();

    expect(active).toBe(1);
    expect(releases).toHaveLength(2);
    releases[1]();
    await second;
    expect(maximumActive).toBe(1);
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

  it("preserves current buckets when the response includes a separate previous entry", () => {
    const trend = Array.from({ length: 15 }, (_, index) => ({
      since: `bucket-${index}`,
      until: `bucket-${index + 1}`,
      usage: [],
    }));

    expect(selectInsightCostTrendBuckets(trend, 7, true)).toEqual(
      trend.slice(7, 14),
    );
  });
});
