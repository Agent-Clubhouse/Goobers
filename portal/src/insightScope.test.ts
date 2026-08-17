import { describe, expect, it } from "vitest";
import type { InsightScope } from "./insightScope";
import {
  deriveInsightCostTrendState,
  hasInsightScopeIdentity,
  insightScopeApiParameters,
  insightScopeFromKey,
  insightScopeFromRoute,
  insightScopeKey,
  insightScopeOption,
  insightScopeRouteFilters,
} from "./insightScope";

const scopes: InsightScope[] = [
  { kind: "instance" },
  { kind: "gaggle", gaggle: "core / tools" },
  { kind: "workflow", gaggle: "core", workflow: "implementation:review" },
  { kind: "stage", gaggle: "core", workflow: "implementation", stage: "local-ci" },
];

describe("Insight scope", () => {
  it.each(scopes)("round-trips $kind select and route serialization", (scope) => {
    expect(insightScopeFromKey(insightScopeKey(scope))).toEqual(scope);

    const route = insightScopeRouteFilters(scope, "30d");
    expect(insightScopeFromRoute(route)).toEqual(scope);
    expect(route).toEqual({ ...insightScopeApiParameters(scope), window: "30d" });
  });

  it("fails closed to instance for malformed select keys and incomplete routes", () => {
    expect(insightScopeFromKey('["stage","core"]')).toEqual({ kind: "instance" });
    expect(insightScopeFromKey("not-json")).toEqual({ kind: "instance" });
    expect(insightScopeFromRoute({ workflow: "implementation", stage: "review" })).toEqual({
      kind: "instance",
    });
  });

  it("owns stable labels for every scope variant", () => {
    expect(scopes.map((scope) => insightScopeOption(scope).label)).toEqual([
      "Instance",
      "Gaggle · core / tools",
      "Workflow · core / implementation:review",
      "Stage · core / implementation / local-ci",
    ]);
  });

  it("matches metric aggregates by exact scope identity", () => {
    const workflow: InsightScope = {
      kind: "workflow",
      gaggle: "core",
      workflow: "implementation",
    };
    const usage = {
      totalAttempts: 1,
      tokenSamples: 0,
      premiumRequestSamples: 0,
      costSamples: 0,
      retryWasteAttempts: 0,
    };

    const state = deriveInsightCostTrendState(workflow, {
      status: "ready",
      data: {
        buckets: [
          {
            since: "2026-08-01T00:00:00Z",
            until: "2026-08-02T00:00:00Z",
            usage: [
              { ...usage, scope: "instance" },
              { ...usage, scope: "workflow", gaggle: "core", workflow: "implementation" },
              {
                ...usage,
                scope: "stage",
                gaggle: "core",
                workflow: "implementation",
                stage: "review",
              },
            ],
          },
        ],
        window: "24h",
      },
    });

    expect(state).toMatchObject({
      status: "ready",
      data: {
        points: [
          {
            usage: { scope: "workflow", gaggle: "core", workflow: "implementation" },
          },
        ],
      },
    });
    expect(hasInsightScopeIdentity(workflow)).toBe(true);
    expect(hasInsightScopeIdentity({ kind: "instance" })).toBe(false);
  });
});
