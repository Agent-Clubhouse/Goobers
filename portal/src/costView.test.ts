import { describe, expect, it } from "vitest";
import type { TelemetryCostResult } from "./api/types";
import { deriveExternalCostRows } from "./costView";

describe("external cost view model", () => {
  it("labels native units, normalized estimates, and partial coverage", () => {
    const result: TelemetryCostResult = {
      scope: "summary",
      since: "2026-08-01T00:00:00Z",
      until: "2026-08-02T00:00:00Z",
      pullRequests: [
        {
          provider: "github",
          externalKind: "pr",
          externalId: "4398",
          totalRuns: 3,
          measuredRuns: 2,
          totalAttempts: 4,
          measuredAttempts: 3,
          nativeTotals: [{ unit: "aiCredits", value: 2.5, estimated: false }],
          normalizedTotals: [{ unit: "usd", value: 0.025, estimated: true }],
          billingModels: ["ai_credits"],
          costBases: ["vendor_reported"],
          coverage: {
            totalRuns: 3,
            measuredRuns: 2,
            totalAttempts: 4,
            measuredAttempts: 3,
            complete: false,
            lowerBound: true,
          },
          models: [
            {
              model: "gpt-5.6-sol",
              usageAttempts: 3,
              measuredAttempts: 3,
              nativeTotals: [{ unit: "aiCredits", value: 2.5, estimated: false }],
              normalizedTotals: [{ unit: "usd", value: 0.025, estimated: true }],
              billingModels: ["ai_credits"],
              costBases: ["vendor_reported"],
            },
          ],
          runs: [
            {
              runId: "run-1",
              startedAt: "2026-08-01T01:00:00Z",
              usageAttempts: 3,
              measuredAttempts: 3,
              nativeTotals: [{ unit: "aiCredits", value: 2.5, estimated: false }],
              normalizedTotals: [{ unit: "usd", value: 0.025, estimated: true }],
              billingModels: ["ai_credits"],
              costBases: ["vendor_reported"],
              models: [],
            },
          ],
        },
      ],
      issues: [],
    };

    expect(deriveExternalCostRows(result)).toEqual([
      {
        key: "github:pr:4398",
        label: "PR #4398",
        provider: "github",
        native: "2.5 AI credits",
        normalized: "$0.025 estimated",
        coverage: "Lower bound: 2 of 3 runs and 3 of 4 attempts measured.",
        lowerBound: true,
        models: ["gpt-5.6-sol: 2.5 AI credits · 3/3 attempts"],
        runs: ["run-1: 2.5 AI credits · 3/3 attempts"],
      },
    ]);
  });
});
