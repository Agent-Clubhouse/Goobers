import { describe, expect, it } from "vitest";
import { insightErrorSignatureFilters, insightWindowFilters } from "./insightData";

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
