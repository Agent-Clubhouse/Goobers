import { describe, expect, it } from "vitest";
import type { TelemetryCostResult } from "./api/types";
import {
  deriveExternalCostRows,
  filterExternalCostRows,
  sortExternalCostRows,
  type ExternalCostRow,
} from "./costView";

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
        externalKind: "pr",
        externalId: "4398",
        provider: "github",
        native: "2.5 AI credits",
        nativeValue: 2.5,
        normalized: "$0.025 estimated",
        normalizedValue: 0.025,
        coverage: "Lower bound: 2 of 3 runs and 3 of 4 attempts measured.",
        coverageRatio: 0.75,
        lowerBound: true,
        models: ["gpt-5.6-sol: 2.5 AI credits · 3/3 attempts"],
        runs: ["run-1: 2.5 AI credits · 3/3 attempts"],
      },
    ]);
  });

  it("filters across work item metadata and detail text", () => {
    const rows = externalCostRows();

    expect(filterExternalCostRows(rows, "claude", "all").map((row) => row.key)).toEqual([
      "github:issue:41",
    ]);
    expect(filterExternalCostRows(rows, "run-pr", "all").map((row) => row.key)).toEqual([
      "github:pr:12",
    ]);
    expect(filterExternalCostRows(rows, "", "issue").map((row) => row.key)).toEqual([
      "github:issue:41",
    ]);
  });

  it("sorts work items and numeric fields while keeping unmeasured values last", () => {
    const rows = externalCostRows();

    expect(sortExternalCostRows(rows, "work-item", "asc").map((row) => row.key)).toEqual([
      "github:issue:41",
      "github:pr:7",
      "github:pr:12",
    ]);
    expect(sortExternalCostRows(rows, "native", "desc").map((row) => row.key)).toEqual([
      "github:pr:12",
      "github:issue:41",
      "github:pr:7",
    ]);
    expect(sortExternalCostRows(rows, "native", "asc").map((row) => row.key)).toEqual([
      "github:issue:41",
      "github:pr:12",
      "github:pr:7",
    ]);
  });
});

function externalCostRows(): ExternalCostRow[] {
  return [
    {
      key: "github:pr:12",
      label: "PR #12",
      externalKind: "pr",
      externalId: "12",
      provider: "github",
      native: "2.5 AI credits",
      nativeValue: 2.5,
      normalized: "$0.025 estimated",
      normalizedValue: 0.025,
      coverage: "Complete coverage",
      coverageRatio: 1,
      lowerBound: false,
      models: ["gpt-5.6-sol"],
      runs: ["run-pr"],
    },
    {
      key: "github:issue:41",
      label: "Issue #41",
      externalKind: "issue",
      externalId: "41",
      provider: "github",
      native: "$0.42",
      nativeValue: 0.42,
      normalized: "42 AI credits estimated",
      normalizedValue: 42,
      coverage: "Lower bound",
      coverageRatio: 0.5,
      lowerBound: true,
      models: ["claude-sonnet"],
      runs: ["run-issue"],
    },
    {
      key: "github:pr:7",
      label: "PR #7",
      externalKind: "pr",
      externalId: "7",
      provider: "github",
      native: "Unmeasured",
      normalized: "Unavailable",
      coverage: "No coverage",
      coverageRatio: 0,
      lowerBound: true,
      models: [],
      runs: [],
    },
  ];
}
